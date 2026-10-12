package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// characterNoteDTO is one entry of a character's dossier
// (store.CharacterNote): a single thing the text states about them, with
// the sentence it rests on.
type characterNoteDTO struct {
	ID            string `json:"id"`
	CharacterID   string `json:"characterId"`
	CharacterName string `json:"characterName"`
	// Kind says what Value is: the kind of being they are ("goblin"), or
	// their age in years at the evidence's chapter ("12").
	Kind  store.CharacterNoteKind `json:"kind"`
	Value string                  `json:"value"`
	// Proposed marks a note fact finding found and the reader hasn't ruled
	// on; casting is shown accepted notes only.
	Proposed bool `json:"proposed,omitempty"`
	// Skipped marks a note the reader turned down: not shown to casting,
	// and listed only so it can be put back for review (rule with
	// review).
	Skipped bool `json:"skipped,omitempty"`
	// Evidence is the sentence of the book the note rests on, word for
	// word, and the Evidence* fields the chapter it is in.
	Evidence             string `json:"evidence,omitempty"`
	EvidenceBookID       string `json:"evidenceBookId,omitempty"`
	EvidenceBookTitle    string `json:"evidenceBookTitle,omitempty"`
	EvidenceChapterIdx   *int   `json:"evidenceChapterIdx,omitempty"`
	EvidenceChapterTitle string `json:"evidenceChapterTitle,omitempty"`
}

func (s *Server) handleListCharacterNotes(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildCharacterNotes(r.PathValue("id"))
	writeBuilt(w, v, err)
}

// buildCharacterNotes lists the notes about the characters of a book's
// series - the ones waiting for review, the ones accepted and the ones
// skipped - kinds before ages, then by value and name, so the notes that
// say the same thing sit together.
func (s *Server) buildCharacterNotes(bookID string) ([]characterNoteDTO, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}
	scope := store.SeriesScope(book)
	notes, err := s.Store.ListCharacterNotes(scope)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	characters, err := s.Store.ListCharacters(scope)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	names := map[string]string{}
	for _, c := range characters {
		names[c.ID] = c.Name
	}
	titles := map[string]string{}
	out := []characterNoteDTO{}
	for _, n := range notes {
		dto := characterNoteDTO{ID: n.ID, CharacterID: n.CharacterID, CharacterName: names[n.CharacterID], Kind: n.Kind, Value: n.Value,
			Proposed: n.Status == store.ProposalPending, Skipped: n.Status == store.ProposalSkipped}
		// The same lookup a proposed fact's evidence gets.
		var where worldFactDTO
		if err := s.factEvidence(&where, store.WorldFact{Evidence: n.Evidence, EvidenceChapterID: n.ChapterID}, titles); err != nil {
			return nil, httpError(http.StatusInternalServerError, err.Error())
		}
		dto.Evidence, dto.EvidenceBookID, dto.EvidenceBookTitle = where.Evidence, where.EvidenceBookID, where.EvidenceBookTitle
		dto.EvidenceChapterIdx, dto.EvidenceChapterTitle = where.EvidenceChapterIdx, where.EvidenceChapterTitle
		out = append(out, dto)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind == store.NoteKind
		}
		if a.Kind == store.NoteKind && a.Value != b.Value {
			return a.Value < b.Value
		}
		return strings.ToLower(a.CharacterName) < strings.ToLower(b.CharacterName)
	})
	return out, nil
}

type ruleCharacterNotesRequest struct {
	// IDs are the notes ruled on - several at once, so every goblin the
	// pass found can be accepted together.
	IDs []string `json:"ids"`
	// Accept puts them in force; false turns them down (a proposed note)
	// or removes them (an accepted one) for good.
	Accept bool `json:"accept"`
	// Review puts them back in front of the reader instead, whatever
	// Accept says: a skipped (or accepted) note is proposed again.
	Review bool `json:"review,omitempty"`
}

type ruleCharacterNotesResponse struct {
	// Recast is how many characters were queued to be cast again: those
	// an accepted kind brought a world fact their casting had not been
	// shown.
	Recast int `json:"recast"`
}

// handleRuleCharacterNotes serves POST
// /api/books/{id}/character-notes/rule: accepts or turns down notes about
// the series' characters. Accepting that someone is of a kind recasts
// them when a world fact in force is about that kind and nothing else of
// theirs - name, alias, their casting's opening, an earlier note - had
// brought it to their casting already, and turns down the other kinds
// proposed or accepted for them (they stay listed as skipped); an age
// changes nothing by itself
// (casting and time jumps read it from then on). Notes that aren't this
// series' are passed over.
func (s *Server) handleRuleCharacterNotes(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req ruleCharacterNotesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	scope := store.SeriesScope(book)
	characters, err := s.Store.ListCharacters(scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := map[string]store.Character{}
	for _, c := range characters {
		byID[c.ID] = c
	}
	facts, err := store.BookWorldFacts(s.Store, book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	worldNotes := make([]speakerattr.WorldNote, len(facts))
	for i, f := range facts {
		worldNotes[i] = speakerattr.WorldNote{Key: f.Key, Aliases: f.Aliases, Fact: f.Fact}
	}
	recast := map[string]bool{}
	for _, id := range req.IDs {
		note, err := s.Store.GetCharacterNote(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if note == nil {
			continue
		}
		char, mine := byID[note.CharacterID]
		if !mine {
			continue
		}
		status := store.ProposalSkipped
		switch {
		case req.Review:
			status = store.ProposalPending
		case req.Accept:
			status = store.ProposalAccepted
		}
		if note.Status == status {
			continue
		}
		if status == store.ProposalAccepted && note.Kind == store.NoteKind && s.Speaker != nil && char.Summary != "" && !char.Invalid {
			had, err := s.acceptedKinds(scope)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			opening, _, _ := strings.Cut(char.Summary, ". ")
			before := append(append([]string{char.Name, opening}, char.Aliases...), had[char.ID]...)
			if len(speakerattr.MatchWorldNotes(worldNotes, append(before, note.Value)...)) > len(speakerattr.MatchWorldNotes(worldNotes, before...)) {
				recast[char.ID] = true
			}
		}
		if err := s.Store.SetCharacterNoteStatus(note.ID, status); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Someone is of one race and one nationality: accepting one
		// settles the others proposed (or accepted before) for them. An
		// age doesn't - they are other ages at other chapters.
		if status == store.ProposalAccepted && note.Kind != store.NoteAge {
			if err := s.Store.SkipOtherCharacterNotes(*note); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	for id := range recast {
		s.enqueueRecast(book, byID[id])
	}
	writeJSON(w, http.StatusOK, ruleCharacterNotesResponse{Recast: len(recast)})
}
