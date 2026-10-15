package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/namekey"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// retagResponse is what POST .../speakers/retag changed: how many
// attributed chapters it looked at, how many it left to an attribution run
// still queued for them, and how many lines moved, by the rule that moved
// them (speakerattr.TagChange.Reason).
type retagResponse struct {
	Chapters int `json:"chapters"`
	Skipped  int `json:"skipped"`
	// Reviewed counts chapters left alone as part of a hand-corrected
	// stretch.
	Reviewed int `json:"reviewed"`
	Changed  int `json:"changed"`
	// Tag is a named speech tag ("…,” Bert said"), Beat an action beat
	// ("“…” Elijah nodded."), Continuation an untagged quote continuing its
	// paragraph's speaker, ContinuedSpeech a paragraph continuing the last
	// one's unclosed speech, ThirdPerson a first-person narrator's "he said"
	// line made Unknown.
	Tag             int `json:"tag"`
	Beat            int `json:"beat"`
	Continuation    int `json:"continuation"`
	ContinuedSpeech int `json:"continuedSpeech"`
	ThirdPerson     int `json:"thirdPerson"`
}

// handleRetagSpeakers serves POST /api/books/{id}/speakers/retag: re-runs
// attribution's deterministic speech-tag rules (speakerattr.
// RefineWithSpeechTags - named tags, action beats, paragraph continuation)
// over every attributed chapter's stored speakers, with no model call, so
// attribution stored before a rule existed (or under a roster that has
// since been cleaned up) gets what the rules say now. A line corrected by
// hand, a play's line, a chapter whose attribution is still queued, and
// every chapter of the book's hand-corrected stretch (its first corrected
// chapter to its last) are left alone. Synchronous - it reads and writes only the database, a few
// seconds for a long book - and answers what it changed.
func (s *Server) handleRetagSpeakers(w http.ResponseWriter, r *http.Request) {
	book, err := s.Store.GetBook(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	out, err := s.retagBook(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) retagBook(book *store.Book) (retagResponse, error) {
	var out retagResponse
	scope := store.SeriesScope(book)
	roster, err := s.Store.ListCharacters(scope)
	if err != nil {
		return out, err
	}
	existing, invalid := splitInvalidCharacters(roster)
	names := make([]string, len(existing))
	aliases := make(map[string][]string, len(existing))
	for i, c := range existing {
		names[i] = c.Name
		if len(c.Aliases) > 0 {
			aliases[c.Name] = c.Aliases
		}
	}
	// The resolver attribution's own tag pass uses (speakerattr.
	// Client.AttributeChapter): titles, and a first-person book's narrator.
	attrRoster := s.withFirstPerson(speakerattr.Roster{Names: names, Aliases: aliases}, book)
	resolver := speakerattr.NewSpeakerNameResolver(names, aliases).WithTitles(attrRoster.World).WithFirstPerson(attrRoster.FirstPerson)

	// A stretch someone corrected by hand - from the book's first corrected
	// chapter to its last - was read through: its uncorrected lines were
	// accepted as right, so the rules could only move them away from that.
	edits, err := s.Store.ParagraphEdits(book.ID, store.EditSpeaker)
	if err != nil {
		return out, err
	}
	reviewedFrom, reviewedTo := -1, -1
	for _, e := range edits {
		if reviewedFrom < 0 || e.ChapterIdx < reviewedFrom {
			reviewedFrom = e.ChapterIdx
		}
		reviewedTo = max(reviewedTo, e.ChapterIdx)
	}
	count, err := s.Store.CountChapters(book.ID)
	if err != nil {
		return out, err
	}
	for idx := range count {
		ch, err := s.Store.GetChapterByIdx(book.ID, idx)
		if err != nil {
			return out, err
		}
		if ch == nil || !ch.Passes.Attribution {
			continue
		}
		if reviewedFrom >= 0 && ch.Idx >= reviewedFrom && ch.Idx <= reviewedTo {
			out.Reviewed++
			continue
		}
		if s.Jobs != nil && s.Jobs.HasAttribution(ch.ID) {
			out.Skipped++
			continue
		}
		out.Chapters++
		all, err := s.Store.ListParagraphsRaw(ch.ID)
		if err != nil {
			return out, err
		}
		edited, err := s.Store.EditedParagraphIdx(ch.ID, store.EditSpeaker)
		if err != nil {
			return out, err
		}
		inputs := make([]speakerattr.ParagraphInput, len(all))
		current := map[int]string{}
		for i, p := range all {
			dialogue := p.IsQuote && !p.ScareQuote && !p.Unvoiced()
			inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: dialogue}
			if dialogue {
				current[p.Idx] = speakerLabel(p.Speaker)
			}
		}
		refined, changes := speakerattr.RefineWithSpeechTags(inputs, current, resolver)
		bySpeakerIdx := map[int]string{}
		byIdx := make(map[int]store.Paragraph, len(all))
		for _, p := range all {
			byIdx[p.Idx] = p
		}
		for _, c := range changes {
			p := byIdx[c.Idx]
			name := refined[c.Idx]
			if edited[c.Idx] || p.SourceSpeaker != "" || name == speakerLabel(p.Speaker) || nameWithin(name, p.Speaker) {
				continue
			}
			if name == "Narrator" {
				name = ""
			} else if name != "Unknown" {
				if invalid.has(name) {
					continue
				}
				char, _, err := s.Store.UpsertCharacter(scope, name, false)
				if err != nil {
					return out, err
				}
				name = char.Name
			}
			bySpeakerIdx[c.Idx] = name
			switch c.Reason {
			case "tag":
				out.Tag++
			case "beat":
				out.Beat++
			case "continuation":
				out.Continuation++
			case "continued speech":
				out.ContinuedSpeech++
			case "third-person tag":
				out.ThirdPerson++
			}
		}
		if len(bySpeakerIdx) == 0 {
			continue
		}
		// A moved line's old speaker number belonged to its old speaker.
		if err := s.Store.SetParagraphSpeakersIndexed(ch.ID, bySpeakerIdx, nil); err != nil {
			return out, fmt.Errorf("chapter %d: %w", ch.Idx, err)
		}
		out.Changed += len(bySpeakerIdx)
	}
	return out, nil
}

// nameWithin reports whether one of two speakers' names is the other's
// words and more ("Torina" / "Torina Milon") - most likely one person
// under two roster entries, where a tag naming "Torina" resolves to the
// short entry. Moving such a line changes nobody's identity, only which
// entry's voice reads it, so retag leaves it to a roster merge.
func nameWithin(a, b string) bool {
	if isSentinelSpeaker(a) || isSentinelSpeaker(b) {
		return false
	}
	words := func(s string) []string { return strings.Fields(namekey.Fold(s)) }
	wa, wb := words(a), words(b)
	if len(wa) > len(wb) {
		wa, wb = wb, wa
	}
	for _, w := range wa {
		if !slices.Contains(wb, w) {
			return false
		}
	}
	return len(wa) > 0
}

func isSentinelSpeaker(s string) bool { return s == "" || s == "Narrator" || s == "Unknown" }

// restoreSpeakersRequest lists lines to set back, each only if it still has
// From (a later change since, by a reader or a pass, wins).
type restoreSpeakersRequest struct {
	Lines []restoreSpeakerLine `json:"lines"`
}

type restoreSpeakerLine struct {
	ChapterIdx   int    `json:"chapterIdx"`
	ParagraphIdx int    `json:"paragraphIdx"`
	From         string `json:"from"`
	To           string `json:"to"`
}

// restoreSpeakersResponse counts what POST .../speakers/restore did.
type restoreSpeakersResponse struct {
	Restored int `json:"restored"`
	Skipped  int `json:"skipped"`
}

// handleRestoreSpeakers serves POST /api/books/{id}/speakers/restore: sets
// listed dialogue lines' speakers back to an earlier machine value - an
// undo for a pass that moved them (a retag run, listed by cmd/tageval
// -undo-from against a backup) - without recording a reader's edit, so
// the lines stay the passes' to judge. A line is skipped unless it is
// dialogue, still has From, has no recorded edit, and To is Narrator,
// Unknown or a valid character of the book's roster. A restored line
// loses its speaker number.
func (s *Server) handleRestoreSpeakers(w http.ResponseWriter, r *http.Request) {
	book, err := s.Store.GetBook(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	var req restoreSpeakersRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	roster, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	existing, _ := splitInvalidCharacters(roster)
	valid := map[string]bool{}
	for _, c := range existing {
		valid[c.Name] = true
	}
	byChapter := map[int][]restoreSpeakerLine{}
	for _, l := range req.Lines {
		byChapter[l.ChapterIdx] = append(byChapter[l.ChapterIdx], l)
	}
	var out restoreSpeakersResponse
	for chIdx, lines := range byChapter {
		ch, err := s.Store.GetChapterByIdx(book.ID, chIdx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ch == nil {
			out.Skipped += len(lines)
			continue
		}
		all, err := s.Store.ListParagraphsRaw(ch.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		edited, err := s.Store.EditedParagraphIdx(ch.ID, store.EditSpeaker)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		byIdx := make(map[int]store.Paragraph, len(all))
		for _, p := range all {
			byIdx[p.Idx] = p
		}
		set := map[int]string{}
		for _, l := range lines {
			p, ok := byIdx[l.ParagraphIdx]
			to := l.To
			if to == "Narrator" {
				to = ""
			}
			if !ok || !p.IsQuote || edited[l.ParagraphIdx] || speakerLabel(p.Speaker) != speakerLabel(l.From) ||
				(to != "" && to != "Unknown" && !valid[to]) {
				out.Skipped++
				continue
			}
			set[l.ParagraphIdx] = to
		}
		if err := s.Store.SetParagraphSpeakersIndexed(ch.ID, set, nil); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out.Restored += len(set)
	}
	writeJSON(w, http.StatusOK, out)
}
