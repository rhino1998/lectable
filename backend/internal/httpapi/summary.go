package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/namekey"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// chapterNotesDTO is one chapter's summary-pass notes (store.ChapterNotes):
// what happens, its scenes, and its cast.
type chapterNotesDTO struct {
	Summary string            `json:"summary"`
	Scenes  []summarySceneDTO `json:"scenes"`
	Cast    []castMemberDTO   `json:"cast"`
	// Edited notes were saved by the reader (PUT .../notes); the summary
	// pass keeps them unless a summarize request passes overwriteEdits.
	// Ignored on PUT.
	Edited bool `json:"edited"`
}

type summarySceneDTO struct {
	// StartIdx is the scene's first paragraph; it runs to the next scene's.
	StartIdx int      `json:"startIdx"`
	Location string   `json:"location"`
	Time     string   `json:"time"`
	Present  []string `json:"present"`
}

type castMemberDTO struct {
	Name     string   `json:"name"`
	Role     bool     `json:"role"`
	Pronouns string   `json:"pronouns"`
	Refs     []string `json:"refs"`
}

// chapterNotesResponse is GET .../notes' reply and the chapterNotes live
// topic's value. Notes is nil until the chapter has been summarized.
type chapterNotesResponse struct {
	Notes *chapterNotesDTO `json:"notes"`
}

func chapterNotesDTOFrom(n store.ChapterNotes) *chapterNotesDTO {
	out := &chapterNotesDTO{Summary: n.Summary, Scenes: []summarySceneDTO{}, Cast: []castMemberDTO{}, Edited: n.Edited}
	for _, sc := range n.Scenes {
		out.Scenes = append(out.Scenes, summarySceneDTO{StartIdx: sc.StartIdx, Location: sc.Location, Time: sc.Time, Present: nonNil(sc.Present)})
	}
	for _, m := range n.Cast {
		out.Cast = append(out.Cast, castMemberDTO{Name: m.Name, Role: m.Role, Pronouns: m.Pronouns, Refs: nonNil(m.Refs)})
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func storeNotesFrom(s speakerattr.ChapterSummary) store.ChapterNotes {
	out := store.ChapterNotes{Summary: s.Summary}
	for _, sc := range s.Scenes {
		out.Scenes = append(out.Scenes, store.SummaryScene{StartIdx: sc.StartIdx, Location: sc.Location, Time: sc.Time, Present: sc.Present})
	}
	for _, m := range s.Cast {
		out.Cast = append(out.Cast, store.CastMember{Name: m.Name, Role: m.Role, Pronouns: m.Pronouns, Refs: m.Refs})
	}
	for _, p := range s.Parts {
		out.Parts = append(out.Parts, store.SummaryPart{StartIdx: p.StartIdx, Summary: p.Summary})
	}
	return out
}

func speakerattrNotes(n *store.ChapterNotes) *speakerattr.ChapterSummary {
	if n == nil {
		return nil
	}
	out := &speakerattr.ChapterSummary{Summary: n.Summary}
	for _, sc := range n.Scenes {
		out.Scenes = append(out.Scenes, speakerattr.Scene{StartIdx: sc.StartIdx, Location: sc.Location, Time: sc.Time, Present: sc.Present})
	}
	for _, m := range n.Cast {
		out.Cast = append(out.Cast, speakerattr.CastMember{Name: m.Name, Role: m.Role, Pronouns: m.Pronouns, Refs: m.Refs})
	}
	return out
}

// chapterNotes is ch's summary-pass notes for a consuming pass, nil when it
// has none (never run, or the LLM isn't configured) - every consumer works
// without them, as before the pass existed.
func (s *Server) chapterNotes(ch *store.Chapter) *speakerattr.ChapterSummary {
	n, err := s.Store.GetChapterNotes(ch.ID)
	if err != nil {
		return nil
	}
	return speakerattrNotes(n)
}

// withChapterCast extends an attribution roster with ch's cast from its
// summary (speakerattr.Roster.WithCast), leaving out invalid names; r
// unchanged when the chapter has no summary. all is the whole chapter
// (not just the paragraphs being attributed), for telling whether its
// narration is in the first person: a first-person book's narrator is
// folded onto a cast entry the summary gave another name
// (speakerattr.FoldFirstPersonCast) and listed in every batch, while a
// chapter told in the third person (a prologue elsewhere in time) drops
// them - they aren't there, and no "I said" is theirs.
func (s *Server) withChapterCast(r speakerattr.Roster, ch *store.Chapter, invalid invalidSpeakers, all []store.Paragraph) speakerattr.Roster {
	notes := s.chapterNotes(ch)
	if notes == nil || len(notes.Cast) == 0 {
		return r
	}
	inputs := make([]speakerattr.ParagraphInput, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, IsQuote: p.IsQuote && !p.ScareQuote}
	}
	firstPerson := r.FirstPerson != "" && speakerattr.FirstPersonNarration(inputs)
	folded, _ := speakerattr.FoldFirstPersonCast(*notes, r, inputs)
	var cast []speakerattr.CastMember
	for _, m := range folded.Cast {
		if !invalid.has(m.Name) {
			cast = append(cast, m)
		}
	}
	out := r.WithCast(cast)
	out.Scenes = folded.Scenes
	switch {
	case !firstPerson:
		out.FirstPerson = ""
	case !slices.Contains(out.Prominent, r.FirstPerson):
		out.Prominent = append(out.Prominent, r.FirstPerson)
	}
	return out
}

// summaryRoster is the known-characters roster the summary pass is shown -
// attribution's (see attributeChapter), minus descriptions: the book's
// series-wide valid characters with their roles, aliases and pronouns, and
// its most frequent speakers as always-listed.
func (s *Server) summaryRoster(book *store.Book, ch *store.Chapter) (speakerattr.Roster, error) {
	roster, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		return speakerattr.Roster{}, err
	}
	existing, _ := splitInvalidCharacters(roster)
	r := speakerattr.Roster{Roles: map[string]bool{}, Aliases: map[string][]string{}, Pronouns: map[string]string{}}
	for _, c := range existing {
		r.Names = append(r.Names, c.Name)
		if c.IsRole {
			r.Roles[c.Name] = true
		}
		if len(c.Aliases) > 0 {
			r.Aliases[c.Name] = c.Aliases
		}
		if c.Pronouns != "" {
			r.Pronouns[c.Name] = c.Pronouns
		}
	}
	r.Prominent = s.prominentSpeakers(book.ID, ch.Idx, r.Names)
	return s.withFirstPerson(r, book), nil
}

// taggedInFirstPerson reports whether bookID's speech tags are told in the
// first person (speakerattr.TaggedInFirstPerson), read once per book per
// process: a book's person doesn't change, and a long book's paragraphs
// take a second to read. A book that can't be read counts as first person,
// leaving its setting's say as it was.
func (s *Server) taggedInFirstPerson(bookID string) bool {
	if v, ok := s.firstPersonBooks.Load(bookID); ok {
		return v.(bool)
	}
	all, err := s.Store.ListParagraphsRawForBook(bookID)
	if err != nil {
		log.Printf("taggedInFirstPerson: book %s: %v", bookID, err)
		return true
	}
	inputs := make([]speakerattr.ParagraphInput, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote}
	}
	first, third := speakerattr.FirstPersonTags(inputs)
	ok := speakerattr.TaggedInFirstPerson(first, third)
	s.firstPersonBooks.Store(bookID, ok)
	return ok
}

// withFirstPerson sets r.FirstPerson from book's setting (its first-person
// narrator), resolved onto r's names so "Steve" matches an existing Steve,
// and makes sure every batch lists them - FirstPerson is left empty for a
// third-person book or one with no setting yet. Every roster a pass reads
// comes through here, so it is also where r gets the world's own titles
// (s.titles). A narrator the book's own speech tags don't bear out - no
// "I said" (taggedInFirstPerson) - is ignored: the setting pass has named
// one for third-person books, and every "he said" line of theirs then
// became Unknown.
func (s *Server) withFirstPerson(r speakerattr.Roster, book *store.Book) speakerattr.Roster {
	r.World = s.titles(book)
	st, err := s.Store.GetBookSetting(book.ID)
	if err != nil || st == nil || st.Setting.Narrator == "" || !s.taggedInFirstPerson(book.ID) {
		return r
	}
	name := st.Setting.Narrator
	if c, ok := speakerattr.NewSpeakerNameResolver(r.Names, r.Aliases).WithTitles(r.World).Resolve(name); ok {
		name = c
	}
	r.FirstPerson = name
	if !slices.Contains(r.Prominent, name) {
		r.Prominent = append(append([]string(nil), r.Prominent...), name)
	}
	return r
}

// summarizeChapter is a jobs.KindSummary task's work: one chapter's summary
// pass (speakerattr.Client.SummarizeChapter), stored with its passes.summary
// flag (Store.SetChapterNotes). Changes no paragraph, so no audio goes stale;
// a pass that reads the notes picks them up the next time it runs. Notes
// the reader edited are kept - see summarizeChapterOverwriting.
func (s *Server) summarizeChapter(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
	return s.summarizeChapterOverwriting(ctx, book, ch, false)
}

// summarizeChapterOverwriting is summarizeChapter, replacing notes the
// reader edited only when overwriteEdits is set (an explicit summarize
// request asking for it - never a bulk run, the pipeline, or a pass
// waiting on the notes).
func (s *Server) summarizeChapterOverwriting(ctx context.Context, book *store.Book, ch *store.Chapter, overwriteEdits bool) (int, func(), error) {
	if !overwriteEdits {
		existing, err := s.Store.GetChapterNotes(ch.ID)
		if err != nil {
			return 0, nil, err
		}
		if existing != nil && existing.Edited {
			return 0, nil, nil
		}
	}
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	if len(all) == 0 {
		return 0, nil, s.Store.SetChapterNotes(ch.ID, store.ChapterNotes{})
	}
	inputs := make([]speakerattr.ParagraphInput, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote}
	}
	roster, err := s.summaryRoster(book, ch)
	if err != nil {
		return 0, nil, err
	}
	sum, err := s.Speaker.SummarizeChapter(ctx, book.Title, ch.Title, s.bookSetting(ctx, book), roster, inputs)
	if err != nil {
		return 0, nil, fmt.Errorf("summarize: %w", err)
	}
	// A first-person narrator the summary listed under another name
	// ("Jamey" for Arise's Steve) is them - identity retrieval below can't
	// tell, since the text only ever calls them "I".
	sum, narratorAlias := speakerattr.FoldFirstPersonCast(sum, roster, inputs)
	if narratorAlias != "" {
		s.recordCastAliases(book, []speakerattr.CastAlias{{Character: roster.FirstPerson, Alias: narratorAlias}})
	}
	// Check the cast against the rest of the book: a new name or a role's
	// reference ("the storm guard sergeant") the book ties to a known
	// character becomes them. Best-effort - the summary stands without it.
	if ix, ierr := s.identityIndex(book, roster); ierr != nil {
		log.Printf("httpapi: identity index for book %s: %v", book.ID, ierr)
	} else {
		resolved, aliases, rerr := s.Speaker.ResolveCastIdentities(ctx, book.Title, sum, ix, roster)
		if rerr != nil {
			log.Printf("httpapi: identity checks for chapter %s: %v", ch.ID, rerr)
		} else {
			sum = resolved
			s.recordCastAliases(book, aliases)
		}
	}
	if err := s.Store.SetChapterNotes(ch.ID, storeNotesFrom(sum)); err != nil {
		return 0, nil, err
	}
	return len(sum.Scenes), nil, nil
}

// handleSummarizeChapter serves POST /api/books/{id}/chapters/{idx}/summarize:
// queues the chapter's summary pass, re-running it if it already has one -
// except over notes the reader edited, unless ?overwriteEdits=true.
func (s *Server) handleSummarizeChapter(w http.ResponseWriter, r *http.Request) {
	overwriteEdits := r.URL.Query().Get("overwriteEdits") == "true"
	book, ch, ok := s.chapterLLMTarget(w, r)
	if !ok {
		return
	}
	s.Jobs.EnqueueSummary(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
		return s.summarizeChapterOverwriting(ctx, book, ch, overwriteEdits)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// buildChapterNotes is the chapterNotes live topic's value and GET
// .../notes' reply.
func (s *Server) buildChapterNotes(bookID string, chapterIdx int) (chapterNotesResponse, error) {
	ch, err := s.Store.GetChapterByIdx(bookID, chapterIdx)
	if err != nil {
		return chapterNotesResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	if ch == nil {
		return chapterNotesResponse{}, httpError(http.StatusNotFound, "chapter not found")
	}
	n, err := s.Store.GetChapterNotes(ch.ID)
	if err != nil {
		return chapterNotesResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	if n == nil {
		return chapterNotesResponse{}, nil
	}
	return chapterNotesResponse{Notes: chapterNotesDTOFrom(*n)}, nil
}

// handlePutChapterNotes serves PUT /api/books/{id}/chapters/{idx}/notes:
// the reader's own version of the chapter's notes - most often a fixed
// cast (someone missing, someone placed in a scene they aren't in, a
// reference tied to the wrong person), which attribution reads on its next
// run. Stored marked edited, so the summary pass keeps it. Trims and
// dedupes; 400 for a nameless or group cast member, a duplicate name, a
// scene starting outside the chapter or twice at one paragraph, or a
// scene's present list naming someone not in the cast. Scenes are sorted
// by start. Passes that already read the old notes aren't re-run.
func (s *Server) handlePutChapterNotes(w http.ResponseWriter, r *http.Request) {
	var req chapterNotesDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	_, ch, ok := s.chapterTarget(w, r)
	if !ok {
		return
	}
	paragraphs, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	notes, err := notesFromEdit(req, len(paragraphs))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The per-window summaries stay with a summary the reader left alone.
	if existing, err := s.Store.GetChapterNotes(ch.ID); err == nil && existing != nil && existing.Summary == notes.Summary {
		notes.Parts = existing.Parts
	}
	if err := s.Store.SetChapterNotes(ch.ID, notes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// notesFromEdit validates and normalizes a reader's notes (see
// handlePutChapterNotes) for a chapter of paragraphCount paragraphs.
func notesFromEdit(req chapterNotesDTO, paragraphCount int) (store.ChapterNotes, error) {
	out := store.ChapterNotes{Summary: strings.TrimSpace(req.Summary), Edited: true}
	castName := map[string]string{} // lower-cased -> as given
	for _, m := range req.Cast {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			return out, fmt.Errorf("a cast member has no name")
		}
		if speakerattr.IsGroupSpeaker(name) {
			return out, fmt.Errorf("%q names a group; list each person", name)
		}
		if _, dup := castName[strings.ToLower(name)]; dup {
			return out, fmt.Errorf("%q is in the cast twice", name)
		}
		castName[strings.ToLower(name)] = name
		out.Cast = append(out.Cast, store.CastMember{Name: name, Role: m.Role, Pronouns: speakerattr.NormalizePronouns(m.Pronouns), Refs: cleanList(m.Refs)})
	}
	starts := map[int]bool{}
	for _, sc := range req.Scenes {
		if sc.StartIdx < 0 || sc.StartIdx >= max(paragraphCount, 1) {
			return out, fmt.Errorf("scene start %d is outside the chapter's %d paragraphs", sc.StartIdx, paragraphCount)
		}
		if starts[sc.StartIdx] {
			return out, fmt.Errorf("two scenes start at paragraph %d", sc.StartIdx)
		}
		starts[sc.StartIdx] = true
		scene := store.SummaryScene{StartIdx: sc.StartIdx, Location: strings.TrimSpace(sc.Location), Time: strings.TrimSpace(sc.Time)}
		for _, p := range cleanList(sc.Present) {
			name, ok := castName[strings.ToLower(p)]
			if !ok {
				return out, fmt.Errorf("scene at paragraph %d lists %q, who isn't in the cast", sc.StartIdx, p)
			}
			scene.Present = append(scene.Present, name)
		}
		out.Scenes = append(out.Scenes, scene)
	}
	slices.SortFunc(out.Scenes, func(a, b store.SummaryScene) int { return a.StartIdx - b.StartIdx })
	return out, nil
}

// cleanList trims each entry and drops blanks and case-insensitive
// duplicates, keeping the first spelling.
func cleanList(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[strings.ToLower(v)] {
			continue
		}
		seen[strings.ToLower(v)] = true
		out = append(out, v)
	}
	return out
}

// handleGetChapterNotes serves GET /api/books/{id}/chapters/{idx}/notes.
func (s *Server) handleGetChapterNotes(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	v, err := s.buildChapterNotes(r.PathValue("id"), idx)
	writeBuilt(w, v, err)
}

// identityIndexTTL is how long a book's identity index (its text and known
// characters, for speakerattr's identity retrieval) is reused before being
// rebuilt - a whole-book summary run reuses one index across its chapters,
// while characters added since show up within minutes.
const identityIndexTTL = 10 * time.Minute

type cachedIdentityIndex struct {
	ix    *speakerattr.IdentityIndex
	built time.Time
}

// identityIndex is book's identity index - every paragraph's text and the
// roster's non-role characters with their aliases - cached per book for
// identityIndexTTL (Server.identityIndexes).
func (s *Server) identityIndex(book *store.Book, roster speakerattr.Roster) (*speakerattr.IdentityIndex, error) {
	if v, ok := s.identityIndexes.Load(book.ID); ok {
		if c := v.(cachedIdentityIndex); time.Since(c.built) < identityIndexTTL {
			return c.ix, nil
		}
	}
	all, err := s.Store.ListParagraphsRawForBook(book.ID)
	if err != nil {
		return nil, err
	}
	texts := make([]string, len(all))
	for i, p := range all {
		texts[i] = p.Text
	}
	names := map[string][]string{}
	for _, n := range roster.Names {
		if !roster.Roles[n] {
			names[n] = roster.Aliases[n]
		}
	}
	ix := speakerattr.NewIdentityIndex(texts, names)
	s.identityIndexes.Store(book.ID, cachedIdentityIndex{ix: ix, built: time.Now()})
	return ix, nil
}

type cachedLowercaseWords struct {
	words speakerattr.LowercaseWords
	built time.Time
}

// bookLowercaseWords returns book's speakerattr.LowercaseWords, cached for
// identityIndexTTL (Server.lowercaseWords) - attribution checks every new
// [role] answer against it.
func (s *Server) bookLowercaseWords(book *store.Book) (speakerattr.LowercaseWords, error) {
	if v, ok := s.lowercaseWords.Load(book.ID); ok {
		if c := v.(cachedLowercaseWords); time.Since(c.built) < identityIndexTTL {
			return c.words, nil
		}
	}
	all, err := s.Store.ListParagraphsRawForBook(book.ID)
	if err != nil {
		return nil, err
	}
	texts := make([]string, len(all))
	for i, p := range all {
		texts[i] = p.Text
	}
	words := speakerattr.NewLowercaseWords(texts)
	s.lowercaseWords.Store(book.ID, cachedLowercaseWords{words: words, built: time.Now()})
	return words, nil
}

// realRoles returns the names in roleNames (attribution's [role] answers)
// whose head noun the book uses in lower case - dropping named people the
// model marked as roles ("Henry", "Count Wavereach"), who'd otherwise be
// listed to attribution as a kind and cast as a generic type.
func (s *Server) realRoles(book *store.Book, roleNames map[string]bool) map[string]bool {
	if len(roleNames) == 0 {
		return roleNames
	}
	words, err := s.bookLowercaseWords(book)
	if err != nil {
		log.Printf("role check: %v", err)
		return roleNames
	}
	out := make(map[string]bool, len(roleNames))
	for name, role := range roleNames {
		if role && !words.NamesAKind(name) {
			log.Printf("attribution: %q answered as a role, but the book never uses %q in lower case - kept as a named character", name, name)
			continue
		}
		out[name] = role
	}
	return out
}

// recordCastAliases saves each name ResolveCastIdentities found to be
// another name for a known character as that character's alias, so
// attribution folds it back in from now on. Best-effort; a name that
// already belongs to another character is skipped (it's already known).
// So is a name for a kind of person - a role, or a name made of the
// world's roles, races and nationalities or reading as a description
// ("Gnolls", "One of the Drakes"): a named person is never one of those
// (the check filed "Poinr" under "Gnolls", "Miihey" under "Elf"). And an
// alias the reader took off that character in a roster review stays off.
func (s *Server) recordCastAliases(book *store.Book, aliases []speakerattr.CastAlias) {
	if len(aliases) == 0 {
		return
	}
	scope := store.SeriesScope(book)
	titles := s.titles(book)
	removed := map[string]bool{} // character id + folded alias
	if proposals, err := s.Store.ListRosterProposals(scope); err == nil {
		for _, p := range proposals {
			kind := speakerattr.RosterFixKind(p.Kind)
			if p.Status == store.RosterAccepted && (kind == speakerattr.RosterRemoveAlias || kind == speakerattr.RosterSplitAlias) {
				removed[p.SubjectID+"\x00"+namekey.Fold(p.Detail)] = true
			}
		}
	}
	for _, a := range aliases {
		if clean, _ := speakerattr.SanitizeSpeaker(a.Alias); clean != a.Alias {
			continue
		}
		if other, err := s.Store.GetCharacterByName(scope, a.Alias); err != nil || other != nil {
			continue
		}
		char, err := s.Store.GetCharacterByName(scope, a.Character)
		if err != nil || char == nil {
			continue
		}
		if char.IsRole || titles.IsPeopleDescription(char.Name) || speakerattr.DescriptorWithoutArticle(char.Name) {
			log.Printf("httpapi: identity: %q is %s - a kind of person, not recorded as an alias", a.Alias, char.Name)
			continue
		}
		if _, _, oneOf := speakerattr.OneOfKind(char.Name); oneOf {
			continue
		}
		if removed[char.ID+"\x00"+namekey.Fold(a.Alias)] {
			continue
		}
		if err := s.Store.AddCharacterAliases(char.ID, a.Alias); err != nil {
			log.Printf("httpapi: record alias %q for %s: %v", a.Alias, a.Character, err)
		} else {
			log.Printf("httpapi: identity: %q is %s (alias recorded)", a.Alias, a.Character)
		}
	}
}
