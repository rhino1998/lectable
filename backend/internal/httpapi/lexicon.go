package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/textsplit"
)

// lexiconEntryDTO is one pronunciation rule (store.LexiconEntry) - shared
// by every book in the book's series (store.SeriesScope), like the
// character roster.
type lexiconEntryDTO struct {
	ID            string `json:"id"`
	Term          string `json:"term"`
	Replacement   string `json:"replacement"`
	CaseSensitive bool   `json:"caseSensitive"`
	// Prefix: the rule also covers words starting with Term (see
	// pronounce.LexiconEntry.Prefix).
	Prefix bool `json:"prefix"`
	// Global: the rule applies to every book, not just this series.
	Global bool `json:"global"`
	// Matches is how many paragraphs in this book the rule applies to.
	Matches int `json:"matches"`
	// MatchedWords is the distinct words it respells in this book, as
	// written (whole words for a Prefix rule) - see lexiconMatches.
	MatchedWords []string `json:"matchedWords"`
}

// lexiconMatchesDTO is what a rule matches in one book - the lexicon
// editor's "what will this respell?" check, before or after saving.
type lexiconMatchesDTO struct {
	Paragraphs int `json:"paragraphs"`
	// Words is the distinct matched words (case-insensitively), at most
	// maxLexiconMatchedWords, most frequent first.
	Words []string `json:"words"`
	// MoreWords counts distinct words past Words' cap.
	MoreWords int `json:"moreWords,omitempty"`
}

// maxLexiconMatchedWords caps lexiconMatchesDTO.Words.
const maxLexiconMatchedWords = 12

// lexiconMatches is what rule matches across paragraphs.
func lexiconMatches(paragraphs []store.Paragraph, rule pronounce.LexiconEntry) lexiconMatchesDTO {
	counts := map[string]int{}
	spelling := map[string]string{}
	var order []string
	out := lexiconMatchesDTO{}
	for _, p := range paragraphs {
		words := pronounce.LexiconMatchedWords(p.Text, rule)
		if len(words) == 0 {
			continue
		}
		out.Paragraphs++
		for _, w := range words {
			key := strings.ToLower(w)
			if _, ok := counts[key]; !ok {
				spelling[key] = w
				order = append(order, key)
			}
			counts[key]++
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	for i, key := range order {
		if i == maxLexiconMatchedWords {
			out.MoreWords = len(order) - i
			break
		}
		out.Words = append(out.Words, spelling[key])
	}
	if out.Words == nil {
		out.Words = []string{}
	}
	return out
}

// handleLexiconMatches reports what a rule - saved or not - matches in this
// book, so the editor can show it before saving.
func (s *Server) handleLexiconMatches(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req upsertLexiconRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	rule := req.rule()
	if rule.Term == "" {
		writeJSON(w, http.StatusOK, lexiconMatchesDTO{Words: []string{}})
		return
	}
	paragraphs, err := s.Store.ListParagraphsRawForBook(book.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, lexiconMatches(paragraphs, rule))
}

func (s *Server) handleListLexicon(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildLexicon(r.PathValue("id"))
	writeBuilt(w, v, err)
}

// buildLexicon lists bookID's scope's pronunciation rules, each with how
// many of this book's paragraphs it matches (so a typo'd rule that never
// fires is visible).
func (s *Server) buildLexicon(bookID string) ([]lexiconEntryDTO, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}
	entries, err := store.BookLexicon(s.Store, book)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]lexiconEntryDTO, len(entries))
	if len(entries) == 0 {
		return out, nil
	}
	paragraphs, err := s.Store.ListParagraphsRawForBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	for i, e := range entries {
		out[i] = lexiconEntryDTO{
			ID: e.ID, Term: e.Term, Replacement: e.Replacement, CaseSensitive: e.CaseSensitive,
			Prefix: e.Prefix, Global: e.Scope == store.GlobalLexiconScope,
		}
		m := lexiconMatches(paragraphs, e.LexiconEntry)
		out[i].Matches, out[i].MatchedWords = m.Paragraphs, m.Words
	}
	return out, nil
}

type upsertLexiconRequest struct {
	Term          string `json:"term"`
	Replacement   string `json:"replacement"`
	CaseSensitive bool   `json:"caseSensitive"`
	// Prefix also matches words starting with Term ("gaol" -> "gaoler").
	Prefix bool `json:"prefix"`
	// Global saves the rule for every book instead of this book's series.
	Global bool `json:"global"`
}

func (r upsertLexiconRequest) rule() pronounce.LexiconEntry {
	return pronounce.LexiconEntry{
		Term: strings.TrimSpace(r.Term), Replacement: strings.TrimSpace(r.Replacement),
		CaseSensitive: r.CaseSensitive, Prefix: r.Prefix,
	}
}

// handleUpsertLexicon adds a pronunciation rule to the book's series, or
// replaces the one for the same term (case-insensitively), then clears
// generated audio for every paragraph in the series that the old or new
// rule matches, so it's re-rendered with the new pronunciation.
func (s *Server) handleUpsertLexicon(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req upsertLexiconRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	rule := req.rule()
	if rule.Term == "" || rule.Replacement == "" {
		writeError(w, http.StatusBadRequest, "term and replacement are required")
		return
	}
	scope := store.SeriesScope(book)
	if req.Global {
		scope = store.GlobalLexiconScope
	}
	entry, previous, err := s.Store.UpsertLexiconEntry(scope, rule)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rules := []pronounce.LexiconEntry{entry.LexiconEntry}
	if previous != nil {
		rules = append(rules, previous.LexiconEntry)
	}
	s.invalidateLexiconMatches(book, scope, rules)
	writeJSON(w, http.StatusOK, idResponse{ID: entry.ID})
}

// handleDeleteLexicon removes a pronunciation rule and clears generated
// audio for every paragraph in its series that the rule matched.
func (s *Server) handleDeleteLexicon(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	entry, err := s.Store.DeleteLexiconEntry(r.PathValue("entryId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entry != nil && (entry.Scope == store.SeriesScope(book) || entry.Scope == store.GlobalLexiconScope) {
		s.invalidateLexiconMatches(book, entry.Scope, []pronounce.LexiconEntry{entry.LexiconEntry})
	}
	writeNoContent(w)
}

// globalLexiconEntryDTO is one pronunciation rule that applies to every
// book (store.GlobalLexiconScope), as the Settings page lists them - with
// no match counts, which would mean reading the whole library.
type globalLexiconEntryDTO struct {
	ID            string `json:"id"`
	Term          string `json:"term"`
	Replacement   string `json:"replacement"`
	CaseSensitive bool   `json:"caseSensitive"`
	// Prefix: the rule also covers words starting with Term.
	Prefix bool `json:"prefix"`
}

func (s *Server) handleListGlobalLexicon(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildGlobalLexicon()
	writeBuilt(w, v, err)
}

// buildGlobalLexicon lists the rules that apply to every book.
func (s *Server) buildGlobalLexicon() ([]globalLexiconEntryDTO, error) {
	entries, err := s.Store.ListLexicon(store.GlobalLexiconScope)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]globalLexiconEntryDTO, len(entries))
	for i, e := range entries {
		out[i] = globalLexiconEntryDTO{ID: e.ID, Term: e.Term, Replacement: e.Replacement, CaseSensitive: e.CaseSensitive, Prefix: e.Prefix}
	}
	return out, nil
}

// handleUpsertGlobalLexicon is handleUpsertLexicon for a rule that applies
// to every book, saved from the Settings page rather than from one book:
// it clears generated audio for every paragraph in the library that the
// old or new rule matches.
func (s *Server) handleUpsertGlobalLexicon(w http.ResponseWriter, r *http.Request) {
	var req upsertLexiconRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	rule := req.rule()
	if rule.Term == "" || rule.Replacement == "" {
		writeError(w, http.StatusBadRequest, "term and replacement are required")
		return
	}
	entry, previous, err := s.Store.UpsertLexiconEntry(store.GlobalLexiconScope, rule)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rules := []pronounce.LexiconEntry{entry.LexiconEntry}
	if previous != nil {
		rules = append(rules, previous.LexiconEntry)
	}
	s.invalidateLexiconMatches(nil, store.GlobalLexiconScope, rules)
	writeJSON(w, http.StatusOK, idResponse{ID: entry.ID})
}

// handleDeleteGlobalLexicon removes a rule that applies to every book and
// clears generated audio for every paragraph in the library it matched.
func (s *Server) handleDeleteGlobalLexicon(w http.ResponseWriter, r *http.Request) {
	entries, err := s.Store.ListLexicon(store.GlobalLexiconScope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := r.PathValue("entryId")
	// Only a global rule: a series' own is removed from its book's page.
	if !slices.ContainsFunc(entries, func(e store.LexiconEntry) bool { return e.ID == id }) {
		writeNoContent(w)
		return
	}
	entry, err := s.Store.DeleteLexiconEntry(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entry != nil {
		s.invalidateLexiconMatches(nil, store.GlobalLexiconScope, []pronounce.LexiconEntry{entry.LexiconEntry})
	}
	writeNoContent(w)
}

func (s *Server) lexiconBook(w http.ResponseWriter, bookID string) (*store.Book, bool) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return nil, false
	}
	return book, true
}

// invalidateLexiconMatches resets generated audio for every paragraph that
// any of rules matches, in every book scope covers - book's series, or the
// whole library for a global rule (book may then be nil). Best-effort like
// invalidateParagraphAudio: the rule itself is already saved, so a failure
// here is logged, not returned.
func (s *Server) invalidateLexiconMatches(book *store.Book, scope string, rules []pronounce.LexiconEntry) {
	var books []store.Book
	var err error
	if scope == store.GlobalLexiconScope {
		books, err = s.Store.ListBooks()
	} else {
		books, err = s.booksInScope(book)
	}
	if err != nil {
		log.Printf("lexicon: list books in scope %s: %v", scope, err)
		return
	}
	for i := range books {
		b := &books[i]
		paragraphs, err := s.Store.ListParagraphsRawForBook(b.ID)
		if err != nil {
			log.Printf("lexicon: list paragraphs of %s: %v", b.ID, err)
			continue
		}
		var hit []store.Paragraph
		for _, rule := range rules {
			hit = append(hit, matchingParagraphs(paragraphs, rule)...)
		}
		s.invalidateParagraphAudio(b, dedupParagraphs(hit))
	}
}

func matchingParagraphs(paragraphs []store.Paragraph, rule pronounce.LexiconEntry) []store.Paragraph {
	var out []store.Paragraph
	rules := []pronounce.LexiconEntry{rule}
	for _, p := range paragraphs {
		if len(pronounce.LexiconSubstitutions(p.Text, rules)) > 0 {
			out = append(out, p)
		}
	}
	return out
}

func dedupParagraphs(ps []store.Paragraph) []store.Paragraph {
	seen := make(map[string]bool, len(ps))
	out := ps[:0]
	for _, p := range ps {
		if !seen[p.ID] {
			seen[p.ID] = true
			out = append(out, p)
		}
	}
	return out
}

// lexiconPreviewMaxChars caps the context line a preview renders.
const lexiconPreviewMaxChars = 240

// handlePreviewLexicon renders a rule - saved or not yet saved - in the
// book's own narrator voice and clone model, so a listener can hear a
// respelling before committing to it (and regenerating every line it
// matches). Speaks the first sentence in the book the rule matches, with
// the rule applied, falling back to the replacement alone.
func (s *Server) handlePreviewLexicon(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req upsertLexiconRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	rule := req.rule()
	if rule.Term == "" || rule.Replacement == "" {
		writeError(w, http.StatusBadRequest, "term and replacement are required")
		return
	}
	paragraphs, err := s.Store.ListParagraphsRawForBook(book.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	text := lexiconPreviewText(paragraphs, rule)

	voice, err := s.Narration.ForParagraph(book, "", 0, narration.NoChapter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	refPath, err := s.ensureVoiceRef(r.Context(), voice.PresetID, "", voice.Instruct, voice.Seed, voice.RefText, voice.SpeedMultiplier, voice.DesignModel)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	refAudio, err := os.ReadFile(refPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	language := voice.Language
	if language == "" {
		language = "Auto"
	}
	audio, err := s.TTS.GeneratePreview(r.Context(), text, voice.CloneModel, refAudio, voice.RefText, language, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	_, _ = w.Write(audio)
}

// lexiconPreviewText is the first sentence among paragraphs that rule
// matches, with rule applied (trimmed to lexiconPreviewMaxChars around the
// match if it's a long one), or just the replacement if nothing matches.
func lexiconPreviewText(paragraphs []store.Paragraph, rule pronounce.LexiconEntry) string {
	rules := []pronounce.LexiconEntry{rule}
	for _, p := range paragraphs {
		if len(pronounce.LexiconSubstitutions(p.Text, rules)) == 0 {
			continue
		}
		for _, sentence := range textsplit.SplitSentences(p.Text) {
			subs := pronounce.LexiconSubstitutions(sentence, rules)
			if len(subs) == 0 {
				continue
			}
			if len(sentence) > lexiconPreviewMaxChars {
				// Keep a window around the first match, on rune boundaries.
				start := max(subs[0].Offset-lexiconPreviewMaxChars/2, 0)
				for start > 0 && !utf8.RuneStart(sentence[start]) {
					start--
				}
				end := min(start+lexiconPreviewMaxChars, len(sentence))
				for end < len(sentence) && !utf8.RuneStart(sentence[end]) {
					end++
				}
				sentence = sentence[start:end]
				subs = pronounce.LexiconSubstitutions(sentence, rules)
			}
			return pronounce.Apply(sentence, nil, subs)
		}
	}
	return rule.Replacement
}

// lexiconSuggestionDTO is a word from the character roster that might want
// a pronunciation rule.
type lexiconSuggestionDTO struct {
	Term string `json:"term"`
	// Lines is how many dialogue lines in this book the character whose
	// name contains Term speaks - the ranking.
	Lines int `json:"lines"`
	// Unusual is a spelling heuristic (see unusualSpelling): letter
	// combinations TTS models tend to guess at. Unusual words sort first.
	Unusual bool `json:"unusual"`
}

// maxLexiconSuggestions bounds the suggestion list.
const maxLexiconSuggestions = 24

func (s *Server) handleLexiconSuggestions(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildLexiconSuggestions(r.PathValue("id"))
	writeBuilt(w, v, err)
}

// buildLexiconSuggestions offers the words of the series roster's names
// and aliases that have no rule yet, unusual spellings first, then by how
// much the character speaks in this book - the names a listener is most
// likely to hear mispronounced, one click from a rule.
func (s *Server) buildLexiconSuggestions(bookID string) ([]lexiconSuggestionDTO, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}
	scope := store.SeriesScope(book)
	characters, err := s.Store.ListCharacters(scope)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	entries, err := store.BookLexicon(s.Store, book)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	lines, err := s.Store.SpeakerLineCounts(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	have := map[string]bool{}
	for _, e := range entries {
		have[strings.ToLower(e.Term)] = true
	}
	byWord := map[string]*lexiconSuggestionDTO{}
	for _, c := range characters {
		if c.Invalid {
			continue
		}
		for _, name := range append([]string{c.Name}, c.Aliases...) {
			for _, word := range nameWords(name) {
				key := strings.ToLower(word)
				if have[key] || commonNameWords[key] {
					continue
				}
				sug := byWord[key]
				if sug == nil {
					sug = &lexiconSuggestionDTO{Term: word, Unusual: unusualSpelling(word)}
					byWord[key] = sug
				}
				sug.Lines = max(sug.Lines, lines[c.Name])
			}
		}
	}
	out := make([]lexiconSuggestionDTO, 0, len(byWord))
	for _, sug := range byWord {
		out = append(out, *sug)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Unusual != b.Unusual {
			return a.Unusual
		}
		if a.Lines != b.Lines {
			return a.Lines > b.Lines
		}
		return a.Term < b.Term
	})
	if len(out) > maxLexiconSuggestions {
		out = out[:maxLexiconSuggestions]
	}
	return out, nil
}

// nameWords splits a character name into its capitalized words ("Lady
// Aeris Vane" -> Lady, Aeris, Vane), dropping short and lowercase ones.
func nameWords(name string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(name, func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\'' && r != '’' && r != '-'
	}) {
		w = strings.Trim(w, "'’-")
		first, _ := utf8.DecodeRuneInString(w)
		if utf8.RuneCountInString(w) < 3 || !unicode.IsUpper(first) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// commonNameWords are titles and plain words that turn up in character
// names but never need a rule.
var commonNameWords = map[string]bool{
	"the": true, "lord": true, "lady": true, "sir": true, "king": true, "queen": true,
	"prince": true, "princess": true, "captain": true, "doctor": true, "master": true,
	"mister": true, "miss": true, "mrs": true, "old": true, "young": true, "big": true,
	"little": true, "uncle": true, "aunt": true, "father": true, "mother": true,
	"brother": true, "sister": true, "general": true, "commander": true, "guard": true,
	"man": true, "woman": true, "boy": true, "girl": true, "voice": true,
}

// unusualSpelling is a rough "a TTS model might guess wrong" test: letters
// and clusters rare in plain English words (x/q/z/j/k/y oddities, doubled
// vowels like "ae"/"ii", apostrophes, diacritics), or three consonants in a
// row. Deliberately generous - it only orders suggestions.
func unusualSpelling(word string) bool {
	lower := strings.ToLower(word)
	for _, r := range lower {
		if r > unicode.MaxASCII {
			return true
		}
	}
	if strings.ContainsAny(lower, "'’xqzj") {
		return true
	}
	for _, s := range []string{"ae", "ii", "uu", "aa", "yy", "kh", "zh", "dh", "yx", "yl", "yr"} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	run := 0
	for _, r := range lower {
		if strings.ContainsRune("aeiouy", r) {
			run = 0
			continue
		}
		run++
		if run >= 3 && !strings.Contains(lower, "ght") && !strings.Contains(lower, "tch") && !strings.Contains(lower, "sch") && !strings.Contains(lower, "chr") && !strings.Contains(lower, "str") {
			return true
		}
	}
	return false
}
