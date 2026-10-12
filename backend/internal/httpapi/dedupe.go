package httpapi

import (
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/namekey"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
)

// characterMergeDTO is one merge dedupeCharacters made: From's lines now
// belong to To.
type characterMergeDTO struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// dedupeResponse is POST .../characters/dedupe's reply.
type dedupeResponse struct {
	Merges []characterMergeDTO `json:"merges"`
}

// mergeCharacterInto folds from into to across book's whole series scope:
// every book's lines attributed to from become to's (and so do the
// narration paragraphs tagged as describing from), from's other names
// become to's aliases when alias (a real alternative name - "Lars" for
// "Lars Machmüller" - not a mislabel like "Protagonist"), to inherits
// from's pronouns if it has none, and from's identity (voice presets
// included) is deleted. What dedupe and absorbShortNames merge with;
// handleMergeCharacter is the reader's own.
func (s *Server) mergeCharacterInto(book *store.Book, from, to store.Character, alias bool) error {
	if from.ID == to.ID {
		return nil
	}
	books, err := s.booksInScope(book)
	if err != nil {
		return err
	}
	if err := s.renameSpeakerEverywhere(books, from.Name, to.Name); err != nil {
		return err
	}
	if alias {
		if clean, _ := speakerattr.SanitizeSpeaker(from.Name); clean == from.Name {
			if err := s.Store.AddCharacterAliases(to.ID, append([]string{from.Name}, from.Aliases...)...); err != nil {
				return err
			}
		}
	}
	if to.Pronouns == "" && from.Pronouns != "" && !to.IsRole {
		if err := s.Store.SetCharacterPronouns(to.ID, from.Pronouns); err != nil {
			return err
		}
	}
	presetIDs, err := s.Store.VoicePresetIDsForCharacter(from.ID)
	if err != nil {
		log.Printf("httpapi: merge %q: list voice presets: %v", from.Name, err)
	}
	for _, presetID := range presetIDs {
		if err := voicerefs.Delete(s.DataDir, presetID); err != nil {
			log.Printf("httpapi: merge %q: delete reference clip %s: %v", from.Name, presetID, err)
		}
		if err := s.Store.DeleteVoicePreset(presetID); err != nil {
			log.Printf("httpapi: merge %q: delete voice preset %s: %v", from.Name, presetID, err)
		}
	}
	return s.Store.DeleteCharacter(from.ID)
}

// renameCharacter renames c to name across book's series scope - its row,
// every line attributed to it and every description tagged with it.
func (s *Server) renameCharacter(book *store.Book, c store.Character, name string) error {
	books, err := s.booksInScope(book)
	if err != nil {
		return err
	}
	if err := s.Store.RenameCharacter(c.ID, name); err != nil {
		return err
	}
	if err := s.renameSpeakerEverywhere(books, c.Name, name); err != nil {
		return err
	}
	// Auto-created voice presets carry the character's name
	// (provisionCharacterVoice); a reader-named one keeps its own.
	presetIDs, err := s.Store.VoicePresetIDsForCharacter(c.ID)
	if err != nil {
		return err
	}
	for _, id := range presetIDs {
		p, err := s.Store.GetVoicePreset(id)
		if err != nil {
			return err
		}
		if p != nil && p.Name == c.Name {
			if err := s.Store.UpdateVoicePreset(p.ID, name, p.Instruct, p.RefText, p.Seed, p.SpeedMultiplier, p.DesignModel); err != nil {
				return err
			}
		}
	}
	return nil
}

// renameSpeakerEverywhere re-points every record of the speaker name from
// at to, across books: paragraph speakers, speaker corrections, the summary notes' cast and scenes, and the book setting's
// narrator (store.RenameSpeakerRecords) - shared by rename and merge.
func (s *Server) renameSpeakerEverywhere(books []store.Book, from, to string) error {
	for _, b := range books {
		if err := s.Store.ReassignCharacterSpeaker(b.ID, from, to); err != nil {
			return err
		}
		if err := s.Store.RenameSpeakerRecords(b.ID, from, to); err != nil {
			return err
		}
	}
	return nil
}

// shortNameTarget is the one other character whose multi-word name
// contains short as a whole word ("Lars" -> "Lars Machmüller"), or nil
// when none or several do (two Lars' - ambiguous) or short is a title word
// ("Lord", or one of titles, the world's own: "Prognugator" is not short
// for "Prognugator Tersk") or too short to mean anything.
func shortNameTarget(short store.Character, chars []store.Character, titles speakerattr.WorldWords) *store.Character {
	name := strings.TrimSpace(short.Name)
	if short.IsRole || short.Invalid || strings.Contains(name, " ") || len([]rune(name)) < 3 || titles.Has(name) {
		return nil
	}
	var found *store.Character
	for i := range chars {
		c := &chars[i]
		if c.ID == short.ID || c.Invalid || c.IsRole {
			continue
		}
		words := strings.Fields(c.Name)
		if len(words) < 2 || !slices.ContainsFunc(words, func(w string) bool { return namekey.Equal(strings.Trim(w, ".,"), name) }) {
			continue
		}
		if found != nil {
			return nil // ambiguous
		}
		found = c
	}
	return found
}

// absorbShortNames folds any first-name-only character into full, a
// multi-word name attribution just created ("Lars" into "Lars Machmüller")
// - the short form was made before the full name was known, and nothing
// else would ever join them. Only unambiguous ones (shortNameTarget);
// best-effort, logged.
func (s *Server) absorbShortNames(book *store.Book, full store.Character) {
	if full.IsRole || !strings.Contains(strings.TrimSpace(full.Name), " ") {
		return
	}
	chars, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		return
	}
	titles := s.titles(book)
	for _, c := range chars {
		if t := shortNameTarget(c, chars, titles); t != nil && t.ID == full.ID {
			if err := s.mergeCharacterInto(book, c, full, true); err != nil {
				log.Printf("httpapi: fold %q into %q: %v", c.Name, full.Name, err)
			} else {
				log.Printf("httpapi: folded %q into %q", c.Name, full.Name)
			}
		}
	}
}

// dedupeCharacters merges book's series roster's duplicate names, in order:
//  1. a first-person placeholder ("Protagonist", "Main character") into
//     the book's first-person narrator (its setting's Narrator);
//  2. case and apostrophe variants ("woman"/"Woman", "Ve'delina"/
//     "Ve’delina" - namekey.Fold) into one - preferring a valid,
//     capitalized, older row;
//  3. a description without an article ("bearded man", "Older man",
//     "doctor") into its role label (speakerattr.RoleLabel), made a role;
//  4. a first-name-only character into the one multi-word name containing
//     it ("Lars" into "Lars Machmüller"), when only one does.
//
// Numbered labels ("Neighbor 1", "Neighbor 2") are left alone - separate
// people in one scene.
func (s *Server) dedupeCharacters(book *store.Book) ([]characterMergeDTO, error) {
	scope := store.SeriesScope(book)
	var merges []characterMergeDTO
	load := func() ([]store.Character, error) { return s.Store.ListCharacters(scope) }
	merge := func(from, to store.Character, alias bool, reason string) error {
		if err := s.mergeCharacterInto(book, from, to, alias); err != nil {
			return fmt.Errorf("merge %q into %q: %w", from.Name, to.Name, err)
		}
		merges = append(merges, characterMergeDTO{From: from.Name, To: to.Name, Reason: reason})
		return nil
	}

	// 1. First-person placeholders.
	if st, err := s.Store.GetBookSetting(book.ID); err == nil && st != nil && st.Setting.Narrator != "" {
		chars, err := load()
		if err != nil {
			return merges, err
		}
		for _, c := range chars {
			if !speakerattr.IsFirstPersonLabel(c.Name) {
				continue
			}
			target, _, err := s.Store.UpsertCharacter(scope, st.Setting.Narrator, false)
			if err != nil {
				return merges, err
			}
			if err := merge(c, target, false, "first-person narrator"); err != nil {
				return merges, err
			}
		}
	}

	// 2. Case and apostrophe variants.
	chars, err := load()
	if err != nil {
		return merges, err
	}
	groups := map[string][]store.Character{}
	var order []string
	for _, c := range chars {
		k := namekey.Fold(c.Name)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], c)
	}
	for _, k := range order {
		g := groups[k]
		if len(g) < 2 {
			continue
		}
		slices.SortStableFunc(g, func(a, b store.Character) int {
			rank := func(c store.Character) int {
				r := 0
				if c.Invalid {
					r += 4
				}
				if !startsUpperRune(c.Name) {
					r += 2
				}
				return r
			}
			if d := rank(a) - rank(b); d != 0 {
				return d
			}
			return int(a.CreatedAt - b.CreatedAt)
		})
		for _, c := range g[1:] {
			reason := "same name, different apostrophe"
			if strings.EqualFold(c.Name, g[0].Name) {
				reason = "same name, different case"
			}
			if err := merge(c, g[0], false, reason); err != nil {
				return merges, err
			}
		}
	}

	// 3. Descriptions without an article.
	if chars, err = load(); err != nil {
		return merges, err
	}
	for _, c := range chars {
		if c.Invalid || !speakerattr.DescriptorWithoutArticle(c.Name) {
			continue
		}
		label, isTitle, ok := speakerattr.RoleLabel(c.Name)
		if !ok || label == c.Name {
			continue
		}
		existing, err := s.Store.GetCharacterByName(scope, label)
		if err != nil {
			return merges, err
		}
		if existing == nil || existing.ID == c.ID {
			// Nothing to merge into: just rename it to its label.
			if err := s.renameCharacter(book, c, label); err != nil {
				return merges, err
			}
			if !isTitle && !c.IsRole {
				if err := s.Store.SetCharacterRole(c.ID, true); err != nil {
					return merges, err
				}
			}
			merges = append(merges, characterMergeDTO{From: c.Name, To: label, Reason: "description renamed to its role"})
			continue
		}
		if !isTitle && !existing.IsRole {
			if err := s.Store.SetCharacterRole(existing.ID, true); err != nil {
				return merges, err
			}
		}
		if err := merge(c, *existing, false, "description folded into its role"); err != nil {
			return merges, err
		}
	}

	// 4. First names into full names.
	if chars, err = load(); err != nil {
		return merges, err
	}
	titles := s.titles(book)
	for _, c := range chars {
		if t := shortNameTarget(c, chars, titles); t != nil {
			if err := merge(c, *t, true, "first name of a full name"); err != nil {
				return merges, err
			}
		}
	}
	return merges, nil
}

func startsUpperRune(s string) bool {
	for _, r := range s {
		return r >= 'A' && r <= 'Z' || r > 127 && strings.ToUpper(string(r)) == string(r)
	}
	return false
}

// handleDedupeCharacters serves POST /api/books/{id}/characters/dedupe - the
// Speakers page's "merge duplicate names" action (see dedupeCharacters),
// replying with every merge it made. Attribution already avoids making new
// duplicates (lookup by name key, descriptions to roles, first names
// folded into full ones as they appear); this cleans up a roster built
// before that, or across chapters attributed out of order.
func (s *Server) handleDedupeCharacters(w http.ResponseWriter, r *http.Request) {
	book, err := s.Store.GetBook(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	merges, err := s.dedupeCharacters(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if merges == nil {
		merges = []characterMergeDTO{}
	}
	writeJSON(w, http.StatusOK, dedupeResponse{Merges: merges})
}
