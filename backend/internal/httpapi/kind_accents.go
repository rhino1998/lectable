package httpapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// A people that is one culture should sound like one: every drake with
// the same accent. The books here never name a real accent for a people
// (see "Fact finding" in docs/speakerattr.md), so which accent is the
// reader's choice; what this file offers is the choice itself - how the
// characters of each kind are cast today, and one action that records an
// accent as the kind's fact and recasts the ones cast otherwise.
//
// Measured on the library (2026-10-11): kinds with a baseline fact are
// already uniform (6 of 6 Unbound dwarves Scottish, 12 of 12 Primal Hunter
// elves Received Pronunciation); a story's own peoples get the book's main
// accent with stragglers (The Wandering Inn's drakes: 36 General American,
// 4 British, 1 RP; gnolls 12/3/1).

// accentGroupDTO is the characters of one kind cast with one accent.
type accentGroupDTO struct {
	// Accent is the accent's name as speakerattr.CastingAccent reads it
	// from a casting ("General American", "Cockney").
	Accent     string            `json:"accent"`
	Characters []characterRefDTO `json:"characters"`
}

// characterRefDTO names one character.
type characterRefDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// kindAccentsDTO is how the characters of one kind are cast: its accents,
// the most common first.
type kindAccentsDTO struct {
	// Key is the word for the kind: a fact's key, or a word the series'
	// castings use for many characters and other series' don't ("drake").
	Key string `json:"key"`
	// Fact is the text of the fact in force for the kind in this series,
	// "" when it has none; Global says that fact is the one for every
	// book, not the series' own.
	Fact   string `json:"fact,omitempty"`
	Global bool   `json:"global,omitempty"`
	// Members is how many characters are of the kind and cast with a
	// stated accent - the ones in Accents.
	Members int              `json:"members"`
	Accents []accentGroupDTO `json:"accents"`
}

const (
	// minKindMembers is how many characters a kind needs to be listed:
	// with fewer there is no spread to speak of.
	minKindMembers = 3
	// maxKindScopes is in how many rosters a word of a casting's opening
	// may appear and still be taken for a kind of this series' own, when
	// no fact names it: "drake" and "gnoll" are in one roster's castings,
	// "young", "woman" and "gravelly" in all of them.
	maxKindScopes = 2
)

// castingOpening is the clause a casting opens with - "Speak as a young
// drake woman" - which is where it says what kind of being someone is.
func castingOpening(summary string) string {
	opening, _, _ := strings.Cut(summary, ". ")
	for _, sep := range []string{",", " with ", " who ", " whose "} {
		opening, _, _ = strings.Cut(opening, sep)
	}
	return opening
}

// openingWords is the words of a casting's opening clause, lower case,
// plain plurals made singular.
func openingWords(summary string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(castingOpening(summary)), func(r rune) bool {
		return (r < 'a' || r > 'z') && r != '\'' && r != '-'
	}) {
		// Not "monstrous" or "nervous": those are no plurals.
		if len(w) > 4 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") {
			w = strings.TrimSuffix(w, "s")
		}
		if len(w) >= 3 && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// kindAccents lists, for book's series, each kind with at least
// minKindMembers characters cast with a stated accent: the kinds its
// facts name (the series' own in force or proposed, and the global ones),
// then the words its castings' openings share that few other rosters use.
// A character is of a kind when their name, an alias or their casting's
// opening sentence uses its word, or a note the reader accepted says so -
// recastForFact's rule. Kinds cast with
// more than one accent come first, the most outliers first.
func (s *Server) kindAccents(book *store.Book) ([]kindAccentsDTO, error) {
	scope := store.SeriesScope(book)
	characters, err := s.Store.ListCharacters(scope)
	if err != nil {
		return nil, err
	}
	inForce, err := store.BookWorldFacts(s.Store, book)
	if err != nil {
		return nil, err
	}
	own, err := s.Store.ListWorldFacts(scope)
	if err != nil {
		return nil, err
	}

	type kind struct {
		dto   kindAccentsDTO
		words []string
	}
	var kinds []kind
	named := map[string]bool{}
	add := func(f store.WorldFact, fact string) {
		// A title, role or place that says nothing else is no kind of
		// being.
		if named[strings.ToLower(f.Key)] || f.Fact == "" && f.Class != "" && f.Class != store.WordRace && f.Class != store.WordNationality {
			return
		}
		for _, w := range f.Words() {
			named[w] = true
		}
		kinds = append(kinds, kind{kindAccentsDTO{Key: f.Key, Fact: fact, Global: fact != "" && f.Scope == store.GlobalFactScope}, f.Words()})
	}
	for _, f := range inForce {
		add(f, f.Fact)
	}
	for _, f := range own {
		if f.Status == store.WorldFactPending {
			add(f, "")
		}
	}

	// Words this roster's castings open with for several characters and
	// other rosters' castings don't use.
	here := map[string]int{}
	for _, c := range characters {
		if c.Invalid || c.Summary == "" {
			continue
		}
		for _, w := range openingWords(c.Summary) {
			here[w]++
		}
	}
	var shared []string
	for w, n := range here {
		if n >= minKindMembers && !named[w] && !speakerattr.IsAccentWord(w) && !describes(w) {
			shared = append(shared, w)
		}
	}
	if len(shared) > 0 {
		castings, err := s.Store.CastingsByScope()
		if err != nil {
			return nil, err
		}
		// rosters is in how many rosters' castings each word opens one.
		rosters := map[string]int{}
		for _, summaries := range castings {
			in := map[string]bool{}
			for _, summary := range summaries {
				for _, w := range openingWords(summary) {
					in[w] = true
				}
			}
			for w := range in {
				rosters[w]++
			}
		}
		sort.Strings(shared)
		for _, w := range shared {
			if rosters[w] <= maxKindScopes {
				kinds = append(kinds, kind{kindAccentsDTO{Key: w}, []string{w}})
			}
		}
	}

	accepted, err := s.acceptedKinds(scope)
	if err != nil {
		return nil, err
	}
	out := []kindAccentsDTO{}
	for _, k := range kinds {
		// MatchWorldNotes passes over a note that says nothing.
		note := []speakerattr.WorldNote{{Key: k.words[0], Aliases: k.words[1:], Fact: k.dto.Key}}
		groups := map[string]*accentGroupDTO{}
		for _, c := range characters {
			if c.Invalid || c.Summary == "" {
				continue
			}
			accent := speakerattr.CastingAccent(c.Summary)
			if accent == "" {
				continue
			}
			opening, _, _ := strings.Cut(c.Summary, ". ")
			if len(speakerattr.MatchWorldNotes(note, append(append([]string{c.Name, opening}, c.Aliases...), accepted[c.ID]...)...)) == 0 {
				continue
			}
			if groups[accent] == nil {
				groups[accent] = &accentGroupDTO{Accent: accent}
			}
			groups[accent].Characters = append(groups[accent].Characters, characterRefDTO{ID: c.ID, Name: c.Name})
			k.dto.Members++
		}
		if k.dto.Members < minKindMembers {
			continue
		}
		for _, g := range groups {
			k.dto.Accents = append(k.dto.Accents, *g)
		}
		sort.Slice(k.dto.Accents, func(i, j int) bool {
			a, b := k.dto.Accents[i], k.dto.Accents[j]
			if len(a.Characters) != len(b.Characters) {
				return len(a.Characters) > len(b.Characters)
			}
			return a.Accent < b.Accent
		})
		out = append(out, k.dto)
	}
	outliers := func(k kindAccentsDTO) int { return k.Members - len(k.Accents[0].Characters) }
	sort.SliceStable(out, func(i, j int) bool { return outliers(out[i]) > outliers(out[j]) })
	return out, nil
}

// describes reports whether w, a word of a casting's opening, reads as a
// description rather than a kind's name: one series' castings said
// "monstrous" of four characters and no other's did.
func describes(w string) bool {
	for _, suffix := range []string{"ous", "ing", "ly", "ed", "ful", "ive", "ish", "less", "like"} {
		if strings.HasSuffix(w, suffix) {
			return true
		}
	}
	return false
}

// handleKindAccents serves GET /api/books/{id}/facts/accents - see
// kindAccents. Not a live topic: it reads every casting of the roster.
func (s *Server) handleKindAccents(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	out, err := s.kindAccents(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// setKindAccentRequest is POST /api/books/{id}/facts/accents' body.
type setKindAccentRequest struct {
	// Key is the kind, as GET .../facts/accents lists it.
	Key string `json:"key"`
	// Accent is the accent every character of the kind should have.
	Accent string `json:"accent"`
}

// accentSentence is what a kind's fact says once an accent is chosen
// for it.
func accentSentence(accent string) string {
	article := " "
	if accent != "" && strings.ContainsRune("AEIOUaeiou", rune(accent[0])) {
		article = "n "
	}
	return accentSentencePrefix + article + accent + " accent, whatever else is said here."
}

// accentSentencePrefix starts both "…with a Scottish accent" and "…with
// an African accent".
const accentSentencePrefix = "In this series they all speak with a"

// handleSetKindAccent serves POST /api/books/{id}/facts/accents: records
// accent as the kind's accent in the series' own fact for it, and recasts
// the characters of the kind cast with another. The fact keeps what the
// one in force said - the general fact's text and words carried over
// when the series had none of its own, since a series' fact replaces the
// general one whole - with the accent as a last sentence that overrides
// whatever it says of accents (a baseline fact's "the same accent as the
// rest of the cast" sits in one sentence with the kind's voice, so
// nothing is cut out); the reader can tidy it on the Facts tab. A proposed fact
// for the kind is accepted by this, as worded. Replies with the fact's id
// and how many characters were queued.
func (s *Server) handleSetKindAccent(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req setKindAccentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	key, accent := strings.TrimSpace(req.Key), strings.TrimSpace(req.Accent)
	if key == "" || accent == "" {
		writeError(w, http.StatusBadRequest, "key and accent are required")
		return
	}
	kinds, err := s.kindAccents(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	i := slices.IndexFunc(kinds, func(k kindAccentsDTO) bool { return strings.EqualFold(k.Key, key) })
	if i < 0 {
		writeError(w, http.StatusNotFound, "no kind of that name has cast characters in this series")
		return
	}

	scope := store.SeriesScope(book)
	fact := store.WorldFact{Key: kinds[i].Key}
	own, err := s.Store.ListWorldFacts(scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	inForce, err := store.BookWorldFacts(s.Store, book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The series' own fact for the kind, in force or proposed (a skipped
	// one is written over); failing that, the general one's text.
	isKind := func(f store.WorldFact) bool { return slices.Contains(f.Words(), strings.ToLower(key)) }
	if j := slices.IndexFunc(own, func(f store.WorldFact) bool { return isKind(f) && f.Status != store.WorldFactSkipped }); j >= 0 {
		fact = store.WorldFact{ID: own[j].ID, Key: own[j].Key, Aliases: own[j].Aliases, Fact: own[j].Fact, Class: own[j].Class, Effect: own[j].Effect}
	} else if j := slices.IndexFunc(inForce, isKind); j >= 0 {
		// BookWorldFacts leaves only global facts unmatched above.
		fact.Key, fact.Aliases, fact.Fact, fact.Class, fact.Effect = inForce[j].Key, inForce[j].Aliases, inForce[j].Fact, inForce[j].Class, inForce[j].Effect
	}
	// An accent chosen here before is replaced; anything else the fact
	// says of accents stays, and the new sentence says it wins.
	var kept []string
	for _, sentence := range strings.SplitAfter(fact.Fact, ". ") {
		if sentence = strings.TrimSpace(sentence); sentence != "" && !strings.HasPrefix(sentence, accentSentencePrefix) {
			kept = append(kept, sentence)
		}
	}
	fact.Fact = strings.Join(append(kept, accentSentence(accent)), " ")
	saved, err := s.Store.UpsertWorldFact(scope, fact)
	if err != nil || saved == nil {
		msg := "fact not found"
		if err != nil {
			msg = err.Error()
		}
		writeError(w, http.StatusInternalServerError, msg)
		return
	}

	resp := upsertFactResponse{ID: saved.ID}
	if s.Speaker != nil {
		for _, g := range kinds[i].Accents {
			if strings.EqualFold(g.Accent, accent) {
				continue
			}
			for _, ref := range g.Characters {
				char, err := s.Store.GetCharacter(ref.ID)
				if err != nil {
					writeError(w, http.StatusInternalServerError, err.Error())
					return
				}
				if char != nil && !char.Invalid {
					s.enqueueRecast(book, *char)
					resp.Recast++
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
