package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicefx"
)

// worldFactDTO is one keyed fact about a story's world (store.WorldFact):
// what the casting prompt is told whenever Key or one of Aliases comes up
// in a character's material.
type worldFactDTO struct {
	ID string `json:"id"`
	// Key is the word the fact is filed under ("dwarf").
	Key string `json:"key"`
	// Aliases are other words that bring it up ("dwarven"); a plain
	// plural of Key needs none.
	Aliases []string `json:"aliases"`
	// Fact may be empty for a word with a Class: the class is then all
	// it says. A fact can have both - a race with its lifespan and accent.
	Fact string `json:"fact"`
	// Class: the word is a title ("Prognugator", "Ser"), a role - a job or
	// class ("Baker") -, a race ("Drake"), a nationality ("Pallassian") or
	// a place ("Pallass") of this world, so the roster never reads it as
	// someone's name: "Prognugator" is not short for "Prognugator Tersk",
	// "Drake Tesy" may be the same person as "Tesy", and a name made only
	// of roles, races and nationalities ("Drake Trader") is a role.
	Class store.WordClass `json:"class,omitempty"`
	// ProposedClass is a class the fact-finding pass proposes for a fact
	// in force that has none, waiting for the reader: accepted by a PUT
	// with it as class, turned down by .../skip.
	ProposedClass store.WordClass `json:"proposedClass,omitempty"`
	// DeclinedClass is a proposed class the reader turned down for a fact
	// in force; .../review proposes it again.
	DeclinedClass store.WordClass `json:"declinedClass,omitempty"`
	// Effect is a voice effect given to every line of a character whose
	// race is this fact's word, after the line is generated - the buzz of
	// an insect people, a voice heard through water - which a casting
	// can't make a voice model produce.
	Effect voicefx.VoiceEffect `json:"effect,omitempty"`
	// Skipped: a proposed fact the reader turned down - not in force, and
	// listed only so it can be put back for review (.../review). Only a
	// series' own fact can be.
	Skipped bool `json:"skipped,omitempty"`
	// UsedBy are, for a word with a class in force or proposed, the
	// characters of the book's series whose names use it. Only set in a
	// book's list.
	UsedBy []string `json:"usedBy,omitempty"`
	// Global: the fact holds in every book, rather than being one
	// series' own.
	Global bool `json:"global"`
	// Superseded: a global fact the book's series has its own fact for,
	// under a shared word - not in force in that series. Only set in a
	// book's list.
	Superseded bool `json:"superseded,omitempty"`
	// Proposed: found by the fact-finding pass and waiting for the reader
	// - not in force until accepted (PUT with its id) and gone once
	// skipped. Only a series' own fact can be.
	Proposed bool `json:"proposed,omitempty"`
	// Evidence is the sentence of the book a proposed fact rests on, word
	// for word, and the Evidence* fields the chapter it is in.
	Evidence             string `json:"evidence,omitempty"`
	EvidenceBookID       string `json:"evidenceBookId,omitempty"`
	EvidenceBookTitle    string `json:"evidenceBookTitle,omitempty"`
	EvidenceChapterIdx   *int   `json:"evidenceChapterIdx,omitempty"`
	EvidenceChapterTitle string `json:"evidenceChapterTitle,omitempty"`
	// Replaces is, for a proposed fact, the text of the global fact it
	// would take the place of in this series once accepted - all of it,
	// an accent rule included, so the reader may want to carry that over.
	Replaces string `json:"replaces,omitempty"`
}

type upsertWorldFactRequest struct {
	// ID rewrites that fact, key included; without it the fact is added,
	// or replaces the one already filed under Key.
	ID      string   `json:"id,omitempty"`
	Key     string   `json:"key"`
	Aliases []string `json:"aliases"`
	// Fact is required unless Class is set.
	Fact string `json:"fact"`
	// Class says what sort of word Key is (worldFactDTO.Class), "" for
	// none; saving a fact with it accepts a class proposed for it.
	Class store.WordClass `json:"class,omitempty"`
	// Effect is the kind's voice effect (worldFactDTO.Effect), "" for
	// none. Like Class it is saved as sent: leaving it out clears it.
	Effect voicefx.VoiceEffect `json:"effect,omitempty"`
}

func (s *Server) handleListGlobalFacts(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildGlobalFacts()
	writeBuilt(w, v, err)
}

func worldFactDTOFrom(f store.WorldFact) worldFactDTO {
	aliases := []string(f.Aliases)
	if aliases == nil {
		aliases = []string{}
	}
	declined, _ := f.DeclinedClass()
	return worldFactDTO{ID: f.ID, Key: f.Key, Aliases: aliases, Fact: f.Fact, Class: f.Class, Effect: f.Effect, ProposedClass: f.WaitingClass(), DeclinedClass: declined, Global: f.Scope == store.GlobalFactScope}
}

// buildGlobalFacts lists the facts that hold in every book
// (store.GlobalFactScope), alphabetical by key.
func (s *Server) buildGlobalFacts() ([]worldFactDTO, error) {
	facts, err := s.Store.ListWorldFacts(store.GlobalFactScope)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]worldFactDTO, 0, len(facts))
	for _, f := range facts {
		if f.Status == store.WorldFactActive {
			out = append(out, worldFactDTOFrom(f))
		}
	}
	return out, nil
}

func (s *Server) handleListBookFacts(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildBookFacts(r.PathValue("id"))
	writeBuilt(w, v, err)
}

// buildBookFacts lists the facts bookID's series has of its own
// (store.SeriesScope) - those in force, those Proposed and those the
// reader Skipped - then the global ones, each marked Superseded when
// a series fact shares a word with it - what store.BookWorldFacts leaves
// out for this book.
func (s *Server) buildBookFacts(bookID string) ([]worldFactDTO, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}
	series, err := s.Store.ListWorldFacts(store.SeriesScope(book))
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	global, err := s.Store.ListWorldFacts(store.GlobalFactScope)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]worldFactDTO, 0, len(series)+len(global))
	taken := map[string]bool{}
	titles := map[string]string{}
	// usedBy fills in who a classed word's names are; names is the
	// roster's, read once one needs it.
	var names []string
	usedBy := func(dto *worldFactDTO) error {
		if dto.Class == "" && dto.ProposedClass == "" {
			return nil
		}
		if names == nil {
			chars, err := s.Store.ListCharacters(store.SeriesScope(book))
			if err != nil {
				return err
			}
			names = []string{}
			for _, c := range chars {
				if !c.Invalid {
					names = append(names, c.Name)
				}
			}
		}
		for _, w := range append([]string{dto.Key}, dto.Aliases...) {
			for _, n := range speakerattr.NamesUsing(w, names) {
				if !slices.Contains(dto.UsedBy, n) {
					dto.UsedBy = append(dto.UsedBy, n)
				}
			}
		}
		return nil
	}
	for _, f := range series {
		dto := worldFactDTOFrom(f)
		if err := usedBy(&dto); err != nil {
			return nil, httpError(http.StatusInternalServerError, err.Error())
		}
		switch f.Status {
		case store.WorldFactActive:
			// A fact that only classes its word replaces no global text
			// (store.BookWorldFacts).
			if f.Fact != "" {
				for _, w := range f.Words() {
					taken[w] = true
				}
			}
		case store.WorldFactPending:
			dto.Proposed = true
			for _, g := range global {
				if f.Fact != "" && g.Status == store.WorldFactActive && slices.ContainsFunc(g.Words(), func(w string) bool { return slices.Contains(f.Words(), w) }) {
					dto.Replaces = g.Fact
					break
				}
			}
		default:
			// Kept so it isn't proposed again, and listed so the reader
			// can change their mind.
			dto.Skipped = true
		}
		if err := s.factEvidence(&dto, f, titles); err != nil {
			return nil, httpError(http.StatusInternalServerError, err.Error())
		}
		out = append(out, dto)
	}
	for _, f := range global {
		if f.Status != store.WorldFactActive {
			continue
		}
		dto := worldFactDTOFrom(f)
		dto.Superseded = slices.ContainsFunc(f.Words(), func(w string) bool { return taken[w] })
		if err := usedBy(&dto); err != nil {
			return nil, httpError(http.StatusInternalServerError, err.Error())
		}
		out = append(out, dto)
	}
	return out, nil
}

// titles is the words of book's own world that are never a person's name:
// the ones its facts in force give a class (store.ClassedWords) - titles,
// roles, races, nationalities, places - which the roster rules read beside
// their fixed titles. nil when there are none, or they can't be read.
func (s *Server) titles(book *store.Book) speakerattr.WorldWords {
	words, err := store.ClassedWords(s.Store, book)
	if err != nil {
		log.Printf("httpapi: world words for %q: %v", book.Title, err)
		return nil
	}
	return speakerattr.NewWorldWords(words)
}

// handleUpsertGlobalFact saves a fact for every book. Nothing is recast:
// a fact reaches a character the next time their voice is cast.
func (s *Server) handleUpsertGlobalFact(w http.ResponseWriter, r *http.Request) {
	s.upsertFact(w, r, store.GlobalFactScope, nil)
}

// handleUpsertBookFact saves a fact for the book's series (or the book
// alone, outside a series), where it replaces any global fact sharing a
// word with it. Nothing is recast.
func (s *Server) handleUpsertBookFact(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	s.upsertFact(w, r, store.SeriesScope(book), book)
}

// upsertFactResponse is PUT .../facts' reply.
type upsertFactResponse struct {
	ID string `json:"id"`
	// Recast is how many characters were queued to be cast again because
	// the request accepted a proposed fact about their kind.
	Recast int `json:"recast"`
}

// upsertFact saves the request's fact in scope. Saving a fact that was
// waiting for review accepts it, and that alone recasts anyone: the
// characters of book's series the fact is plainly about
// (recastForFact). A fact the reader writes or edits recasts nobody.
func (s *Server) upsertFact(w http.ResponseWriter, r *http.Request, scope string, book *store.Book) {
	var req upsertWorldFactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	fact := store.WorldFact{ID: req.ID, Key: strings.TrimSpace(req.Key), Fact: strings.TrimSpace(req.Fact), Class: req.Class, Effect: req.Effect}
	if fact.Key == "" || fact.Fact == "" && fact.Class == "" && fact.Effect == "" {
		writeError(w, http.StatusBadRequest, "key and fact are required")
		return
	}
	if !voicefx.Valid(fact.Effect) {
		writeError(w, http.StatusBadRequest, "unknown effect")
		return
	}
	if !fact.Class.Valid() {
		writeError(w, http.StatusBadRequest, "unknown class")
		return
	}
	for _, a := range req.Aliases {
		a = strings.TrimSpace(a)
		if a == "" || strings.EqualFold(a, fact.Key) || slices.ContainsFunc(fact.Aliases, func(have string) bool { return strings.EqualFold(have, a) }) {
			continue
		}
		fact.Aliases = append(fact.Aliases, a)
	}
	accepting := false
	// was is the effect and words the fact had, for a series' fact: lines
	// already narrated under another effect are stale once it changes.
	var was store.WorldFact
	if book != nil {
		existing, err := s.Store.ListWorldFacts(scope)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, f := range existing {
			if f.ID == fact.ID || fact.ID == "" && strings.EqualFold(f.Key, fact.Key) {
				was = f
			}
		}
		accepting = fact.ID != "" && was.ID == fact.ID && was.Status == store.WorldFactPending
	}
	saved, err := s.Store.UpsertWorldFact(scope, fact)
	switch {
	case errors.Is(err, store.ErrFactKeyTaken):
		writeError(w, http.StatusConflict, "another fact is already filed under "+fact.Key)
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	case saved == nil:
		writeError(w, http.StatusNotFound, "fact not found")
	default:
		resp := upsertFactResponse{ID: saved.ID}
		if book != nil && was.Effect != saved.Effect {
			if err := s.dropEffectAudio(book, append(was.Words(), saved.Words()...)); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		if accepting {
			if resp.Recast, err = s.recastForFact(book, *saved); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// dropEffectAudio deletes the narrated lines of every character of book's
// series whose race is one of words - a kind whose voice effect just
// changed, so their clips are generated again with (or without) it. Only
// a series' own fact does this; a general fact's effect reaches lines as
// they are next generated.
func (s *Server) dropEffectAudio(book *store.Book, words []string) error {
	notes, err := s.Store.ListCharacterNotes(store.SeriesScope(book))
	if err != nil {
		return err
	}
	books, err := s.booksInScope(book)
	if err != nil {
		return err
	}
	bookIDs := make([]string, len(books))
	for i, b := range books {
		bookIDs[i] = b.ID
	}
	for _, n := range notes {
		if n.Kind != store.NoteKind || n.Status != store.ProposalAccepted || !slices.Contains(words, strings.ToLower(strings.TrimSpace(n.Value))) {
			continue
		}
		char, err := s.Store.GetCharacter(n.CharacterID)
		if err != nil {
			return err
		}
		if char == nil {
			continue
		}
		if _, err := s.Store.DeleteParagraphAudioForSpeaker(bookIDs, char.Name); err != nil {
			return err
		}
	}
	return nil
}

// recastForFact queues a recast of every character of book's series that
// fact is plainly about - one whose name, aliases or the opening sentence
// of their casting uses one of its words ("Grak the Kobold", "Speak as a
// young kobold woman..."), or the reader accepted a note that they are of
// that kind - and returns how many. Deliberately narrower
// than what casting itself matches a fact against (worldNotes: the
// narration around a character's lines too): by that measure a kobold
// fact reached 78 of one series' 324 characters, mostly humans who share
// scenes with kobolds, and a recast replaces a voice and its audio.
// Someone the fact is about but whose casting never says so is recast by
// hand.
func (s *Server) recastForFact(book *store.Book, fact store.WorldFact) (int, error) {
	if s.Speaker == nil {
		return 0, nil
	}
	characters, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		return 0, err
	}
	kinds, err := s.acceptedKinds(store.SeriesScope(book))
	if err != nil {
		return 0, err
	}
	note := []speakerattr.WorldNote{{Key: fact.Key, Aliases: fact.Aliases, Fact: fact.Fact}}
	queued := 0
	for _, char := range characters {
		if char.Invalid || char.Summary == "" {
			continue
		}
		opening, _, _ := strings.Cut(char.Summary, ". ")
		texts := append(append([]string{char.Name, opening}, char.Aliases...), kinds[char.ID]...)
		if len(speakerattr.MatchWorldNotes(note, texts...)) == 0 {
			continue
		}
		s.enqueueRecast(book, char)
		queued++
	}
	return queued, nil
}

// enqueueRecast queues char to be cast again from scratch, replacing
// their voice and the audio of their lines - what the Speakers tab's
// "recharacterize" does.
func (s *Server) enqueueRecast(book *store.Book, char store.Character) {
	s.Jobs.EnqueueCharacterization(book.ID, char.ID, char.Name, func(ctx context.Context) error {
		_, _, err := s.recharacterizeAndInvalidate(ctx, book, char.ID, func(ctx context.Context) error {
			_, err := s.characterizeVoice(ctx, book, char)
			return err
		})
		return err
	})
}

func (s *Server) handleDeleteGlobalFact(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.DeleteWorldFact(store.GlobalFactScope, r.PathValue("factId")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// handleDeleteBookFact removes one of the series' own facts; a global
// fact's id is left alone.
func (s *Server) handleDeleteBookFact(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	if _, err := s.Store.DeleteWorldFact(store.SeriesScope(book), r.PathValue("factId")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// affectedCharacterDTO is one character whose casting the facts in force
// bear on: the facts their material brings up.
type affectedCharacterDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Role: a pooled role label ("Guard") rather than one person.
	Role bool `json:"role"`
	// Notes are the "key: fact" lines their casting is shown.
	Notes []string `json:"notes"`
	// Summary is their casting as it stands.
	Summary string `json:"summary"`
}

// handleFactsAffected lists the characterized characters of the book's
// series whose material brings up a fact in force - the ones a recast
// would now read differently. It searches the series once per character,
// so it is slow on a long roster, and is not a live topic.
func (s *Server) handleFactsAffected(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	books, err := s.booksInScope(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	bookIDs := make([]string, len(books))
	for i, b := range books {
		bookIDs[i] = b.ID
	}
	characters, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []affectedCharacterDTO{}
	for _, char := range characters {
		if char.Invalid || char.Summary == "" {
			continue
		}
		if r.Context().Err() != nil {
			return
		}
		evidence, err := s.describedEvidence(books, bookIDs, char)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		notes, err := s.worldNotes(book, char, evidence)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(notes) > 0 {
			out = append(out, affectedCharacterDTO{ID: char.ID, Name: char.Name, Role: char.IsRole, Notes: notes, Summary: char.Summary})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
