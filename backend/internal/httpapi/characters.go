package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// maxCharacterizeQuotes bounds how many of a character's quotes (across
// however many books in their series) get pulled for characterization -
// see Server.provisionCharacterVoice. Matches speakerattr's own internal
// cap - see its own doc comment for why this was lowered from an original
// 30 (a real, observed slow characterization task for a prominent
// character, traced to Store.QuotesForBooks' own pre-LIMIT sort cost, not
// the LLM call itself) - kept here too so the store query doesn't fetch
// more rows than will ever be used.
const maxCharacterizeQuotes = 20

// maxCharacterizeDescriptions is maxCharacterizeQuotes' counterpart for
// description paragraphs - matches speakerattr's own internal cap (see its
// own doc comment), kept here too so Store.DescriptionsForBooks doesn't
// fetch more rows than will ever be used.
const maxCharacterizeDescriptions = 10

// speakerRowDTO is one row of a book's speaker table (GET
// .../speakers): every speaker actually attributed in the book so far,
// including a synthesized "Narrator" row for unattributed/narration
// paragraphs, with how much of their dialogue has generated audio. ID is
// "" for the Narrator row (it isn't a characters table row - its voice is
// the book's own, set via PUT .../voice) and for any speaker name a
// paragraph carries that speakerattr produced but that never got
// registered as a character (shouldn't normally happen - see
// Server.attributeChapter - but this endpoint reports it rather than
// hiding it). Previewing actual generated dialogue lives in
// .../appearances now (one attempt per real paragraph, not a single
// aggregate sample here), not this row.
type speakerRowDTO struct {
	ID            string `json:"id,omitempty"`
	Name          string `json:"name"`
	VoicePresetID string `json:"voicePresetId,omitempty"`
	// Summary is the LLM's characterization of how this speaker sounds -
	// see store.Character.Summary - "" for the Narrator row (it isn't a
	// character) or a character not yet (or unsuccessfully) characterized.
	Summary string `json:"summary,omitempty"`
	// RefLine is Summary's paired reference passage - see
	// store.Character.RefLine - what this character's auto-created voice
	// preset's own reference clip is actually rendered from (in place of
	// voices.DefaultRefText), written to match Summary's own pace/energy.
	// "" alongside an empty Summary, same as Summary itself.
	RefLine        string `json:"refLine,omitempty"`
	ParagraphCount int    `json:"paragraphCount"`
	ReadyCount     int    `json:"readyCount"`
	// RefAudioURL previews this speaker's *own* voice preset specifically
	// (the same clip cloning generation itself uses) - unlike
	// SampleAudioURL, it's available immediately (rendered when the
	// preset was assigned - see voicerefs.EnsureFile), not only after real
	// paragraph audio has been generated. The Narrator row's own voice is
	// the book's (that's what Narrator means), so it's always set there;
	// for an actual character it's deliberately "" whenever
	// VoicePresetID is "" (nothing assigned to them yet), even though
	// their dialogue still narrates in the book's own voice in the
	// meantime - showing the book's clip here would look like "this is
	// what they sound like" rather than "nobody's voiced them yet", and
	// every un-provisioned character would show the exact same clip.
	RefAudioURL string `json:"refAudioUrl,omitempty"`
}

// presetAudioURL returns presetID's own reference-clip endpoint - the
// built-in path (internal/voices) or the custom one (store.VoicePreset),
// whichever namespace presetID actually belongs to. "" for "" (no preset
// resolved at all).
func presetAudioURL(presetID string) string {
	if presetID == "" {
		return ""
	}
	if _, ok := voices.PresetsByID[presetID]; ok {
		return "/api/voices/presets/" + presetID + "/audio"
	}
	return "/api/voices/custom-presets/" + presetID + "/audio"
}

// handleListSpeakers reports every speaker recognized in bookID so far -
// the Narrator plus every attributed character - and how much of each
// one's audio has actually been generated, so the reader can see which
// character voices are worth assigning/regenerating before committing to a
// full multi-voice pass. Speaker attribution and character-voice
// assignment work the same regardless of book.MultiVoice; this table just
// reports what would narrate if it's on (see internal/narration.Resolver).
func (s *Server) handleListSpeakers(w http.ResponseWriter, r *http.Request) {
	writeBuilt(w)(s.buildSpeakers(r.PathValue("id")))
}

func (s *Server) buildSpeakers(bookID string) (any, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}

	paragraphs, err := s.Store.ListParagraphsRawForBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	characters, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	charByName := make(map[string]store.Character, len(characters))
	characterIDs := make([]string, len(characters))
	for i, c := range characters {
		charByName[c.Name] = c
		characterIDs[i] = c.ID
	}
	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	// Characters' voice assignments are per clone model (see
	// store.Character's doc comment) - this table reports whichever
	// assignment applies to the clone model this book currently narrates
	// through.
	cloneModel := narration.EffectiveCloneModel(bookVoice)
	presetIDByChar, err := s.Store.CharacterVoicesForModel(characterIDs, cloneModel)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}

	idsBySpeaker := map[string][]string{}
	for _, p := range paragraphs {
		name := p.Speaker
		if name == "" {
			name = "Narrator"
		}
		idsBySpeaker[name] = append(idsBySpeaker[name], p.ID)
	}

	resolvedVoiceCache := map[string]narration.ResolvedVoice{}
	resolveFor := func(name string) narration.ResolvedVoice {
		if !book.MultiVoice() || name == "Narrator" {
			return bookVoice
		}
		c, ok := charByName[name]
		if !ok {
			return bookVoice
		}
		if cached, ok := resolvedVoiceCache[c.ID]; ok {
			return cached
		}
		resolved, err := s.Narration.ResolveCharacterVoice(book, bookVoice, c, cloneModel, presetIDByChar[c.ID])
		if err != nil {
			return bookVoice
		}
		resolvedVoiceCache[c.ID] = resolved
		return resolved
	}

	// Narrator first, then characters in discovery order, then any
	// paragraph-carried speaker name (e.g. "Unknown") that isn't a
	// registered character.
	order := []string{"Narrator"}
	seen := map[string]bool{"Narrator": true}
	for _, c := range characters {
		if !seen[c.Name] {
			order = append(order, c.Name)
			seen[c.Name] = true
		}
	}
	for name := range idsBySpeaker {
		if !seen[name] {
			order = append(order, name)
			seen[name] = true
		}
	}

	rows := make([]speakerRowDTO, 0, len(order))
	for _, name := range order {
		ids := idsBySpeaker[name]
		row := speakerRowDTO{Name: name, ParagraphCount: len(ids)}
		if c, ok := charByName[name]; ok {
			row.ID = c.ID
			row.VoicePresetID = presetIDByChar[c.ID]
			row.Summary = c.Summary
			row.RefLine = c.RefLine
		}
		// The voice this speaker's dialogue actually generates/plays under
		// right now - their own assigned preset if they have one for this
		// book's clone model, otherwise the book's own (see resolveFor).
		// Only used below to count ready/generated audio, which really did
		// render under this voice regardless of whether it's "theirs" -
		// RefAudioURL (this speaker's *own* voice preview) is deliberately
		// NOT set from this resolution; see below.
		resolved := resolveFor(name)
		if len(ids) > 0 {
			count, err := s.Store.CountReadyAudioForSpeaker(bookID, name, resolved.VoiceID())
			if err != nil {
				return nil, httpError(http.StatusInternalServerError, err.Error())
			}
			row.ReadyCount = count
		}
		// RefAudioURL previews this speaker's *own* voice specifically -
		// the Narrator row's own voice genuinely is the book's (that's
		// what Narrator means), but a character with no voice assigned
		// yet (row.VoicePresetID == "") gets none at all here, rather than
		// silently falling back to the book's own clip the way `resolved`
		// above does for playback purposes - showing that would look like
		// "this is what they sound like" when it's really just "nobody's
		// voiced them yet", and every un-provisioned character would
		// otherwise show the exact same clip as the narrator and each
		// other.
		if name == "Narrator" {
			row.RefAudioURL = presetAudioURL(bookVoice.PresetID)
		} else if row.VoicePresetID != "" {
			row.RefAudioURL = presetAudioURL(row.VoicePresetID)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// handleDeleteBookSpeakerData clears every piece of speaker data visible
// on book's Speakers page: this book's own paragraph attribution (back
// to "" - Store.ClearBookSpeakers) and, for book's whole series scope
// (store.SeriesScope - the same scope handleListSpeakers already draws
// the roster from), every character's identity/Summary, every clone
// model's voice assignment, and every voice preset auto-created for one
// of them (+ its cached reference clip) - the Speakers page's own
// "Delete speaker data" button, for starting completely over. Like
// handleDeleteCustomVoicePreset, preset deletion here doesn't check
// whether a preset is also referenced elsewhere (a book's own voice, the
// default voice, another character happening to share it) first - same
// no-safety-net convention as deleting one preset manually from the
// Voices page, just applied in bulk. Best-effort past the first two
// steps: one character's cleanup failing is logged and skipped rather
// than aborting the rest, since a bulk delete partially failing and
// leaving the reader unsure what's left is worse than "cleaned up
// everything it could, logged what it couldn't."
func (s *Server) handleDeleteBookSpeakerData(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	if err := s.Store.ClearBookSpeakers(bookID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	characters, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, char := range characters {
		presetIDs, err := s.Store.VoicePresetIDsForCharacter(char.ID)
		if err != nil {
			writeLoggedWarning(r, "list voice presets for character %q: %v", char.Name, err)
			presetIDs = nil
		}
		for _, presetID := range presetIDs {
			if err := voicerefs.Delete(s.DataDir, presetID); err != nil {
				writeLoggedWarning(r, "delete cached reference clip for preset %s: %v", presetID, err)
			}
			if err := s.Store.DeleteVoicePreset(presetID); err != nil {
				writeLoggedWarning(r, "delete voice preset %s: %v", presetID, err)
			}
		}
		if err := s.Store.DeleteCharacter(char.ID); err != nil {
			writeLoggedWarning(r, "delete character %q: %v", char.Name, err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteCharacter removes one character entirely - a scalpel next
// to handleDeleteBookSpeakerData's sledgehammer, for cleaning up a single
// bad attribution (a name that was never really a character - a stray
// first-person pronoun, a generic descriptive phrase, a one-off
// hallucination) without resetting the whole roster. Every paragraph in
// *this* book currently attributed to them reverts to "Unknown", not ""
// (Narrator) - see Store.ReassignCharacterSpeaker's own doc comment for
// why: these paragraphs are still real quoted dialogue, just no longer
// attributed to this character, and "Narrator" is reserved exclusively
// for actual narration. Scoped to this book only; their identity/Summary,
// every clone model's voice assignment, and every voice preset
// auto-created for them (+ cached reference clip) are deleted for their
// whole series scope, same split as handleDeleteBookSpeakerData.
func (s *Server) handleDeleteCharacter(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	characterID := r.PathValue("characterId")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	char, err := s.Store.GetCharacter(characterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}

	if err := s.Store.ReassignCharacterSpeaker(bookID, char.Name, "Unknown"); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.deleteCharacterIdentity(r, *char); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deleteCharacterIdentity removes char's own identity/voice for their
// whole series scope - every clone model's voice preset assigned to them
// (+ its cached reference clip) and the character row itself. The shared
// tail of handleDeleteCharacter and handleMergeCharacter, which differ
// only in what happens to the paragraphs that used to be attributed to
// them (blanked to Narrator vs. reattributed to another name).
func (s *Server) deleteCharacterIdentity(r *http.Request, char store.Character) error {
	presetIDs, err := s.Store.VoicePresetIDsForCharacter(char.ID)
	if err != nil {
		writeLoggedWarning(r, "list voice presets for character %q: %v", char.Name, err)
		presetIDs = nil
	}
	for _, presetID := range presetIDs {
		if err := voicerefs.Delete(s.DataDir, presetID); err != nil {
			writeLoggedWarning(r, "delete cached reference clip for preset %s: %v", presetID, err)
		}
		if err := s.Store.DeleteVoicePreset(presetID); err != nil {
			writeLoggedWarning(r, "delete voice preset %s: %v", presetID, err)
		}
	}
	return s.Store.DeleteCharacter(char.ID)
}

type mergeCharacterRequest struct {
	// TargetName is who every one of this book's paragraphs currently
	// attributed to this character should be reattributed to instead -
	// "Narrator" (a sentinel, not a real character row) or another
	// character's exact name.
	TargetName string `json:"targetName"`
}

// handleMergeCharacter folds one character into another: every paragraph
// in this book currently attributed to them is reattributed to
// req.TargetName instead of reverting to blank, and their own identity/
// voice/presets are deleted for their whole series scope (see
// deleteCharacterIdentity) - for when attribution split one person into
// two rows (a variant spelling canonicalizeSpeakerNames didn't catch, or
// a misattribution like a character's own name appearing as a vocative
// in someone else's line - see backend/CLAUDE.md's "Speaker attribution"
// section), or turns out to have just been the narrator all along.
// Unlike handleDeleteCharacter (which always reassigns to "Unknown" -
// see its own doc comment), this lets the caller explicitly choose
// "Narrator" as the target too: a deliberate reader assertion ("this
// really was narration, not a real speaker") is different from simply
// removing a bad character with no replacement judgment made either way.
// Both are thin wrappers over the same two steps (Store.
// ReassignCharacterSpeaker + deleteCharacterIdentity) with a different
// fixed/caller-supplied target; merging into another named character
// additionally registers it as a real character first if it wasn't one
// already (UpsertCharacter is idempotent either way - see
// handleListSpeakers' own comment on a raw paragraph speaker that never
// got registered).
func (s *Server) handleMergeCharacter(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	characterID := r.PathValue("characterId")

	var req mergeCharacterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	targetName := strings.TrimSpace(req.TargetName)
	if targetName == "" {
		writeError(w, http.StatusBadRequest, "targetName is required")
		return
	}

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	char, err := s.Store.GetCharacter(characterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}
	if char.Name == targetName {
		writeError(w, http.StatusBadRequest, "cannot merge a character into themselves")
		return
	}

	storeTargetName := targetName
	if targetName == "Narrator" {
		storeTargetName = ""
	} else if targetName != "Unknown" {
		if _, _, err := s.Store.UpsertCharacter(store.SeriesScope(book), targetName, false); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if err := s.Store.ReassignCharacterSpeaker(bookID, char.Name, storeTargetName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.deleteCharacterIdentity(r, *char); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type reattributeSpeakerRequest struct {
	// Name is the speaker being eliminated - either a real character's
	// exact name, or literally "Unknown" (the sentinel store.Paragraph.
	// Speaker carries for a quoted line the LLM itself couldn't resolve -
	// see that field's own doc comment). Unlike merge, there's no separate
	// characterId in the path: Unknown has no characters-table row to
	// address one by, so this endpoint is reached by name, scoped to this
	// book's own speaker table, the same way it's already keyed there (see
	// handleListSpeakers).
	Name string `json:"name"`
}

// handleReattributeSpeaker is the "Auto Split" button's entry point: for a
// speaker believed not to be a real, distinct individual - a false-positive
// character attribution's own accidental character row, or the "Unknown"
// sentinel itself, e.g. after a reader manually renamed/deleted the wrong
// speaker upstream - it re-judges every one of that speaker's own
// paragraphs in this book (chapter by chapter, one KindSpeakerReattribution
// task per chapter - see jobs.Manager.EnqueueReattribution) and
// redistributes them to whichever real character/Narrator/Unknown they
// actually belong to, using the exact same chapter-wide LLM pass
// attribution itself already runs (reattributeChapterSpeaker reuses
// speakerattr.Client.AttributeChapter directly - no second prompt), just
// with req.Name withheld from "Known characters" and never accepted back
// as a result for one of its own paragraphs (see reattributeChapterSpeaker's
// own doc comment).
//
// Deliberately does NOT delete the character's own identity/voice
// afterward, even if this run ends up reattributing every one of their
// paragraphs - unlike handleDeleteCharacter/handleMergeCharacter, which are
// one synchronous action that knows the outcome immediately, this fans out
// into several fire-and-forget background tasks with no single moment
// "reattribution finished" to hang that cleanup off of. A reader can always
// follow up with the existing Delete button once they see the row is
// actually empty.
func (s *Server) handleReattributeSpeaker(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker reattribution is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")

	var req reattributeSpeakerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if name == "Narrator" {
		writeError(w, http.StatusBadRequest, "cannot reattribute the Narrator")
		return
	}

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	// speakerKey namespaces each chapter's own dedup key (see
	// EnqueueReattribution) - a real character's own id, or the "unknown"
	// sentinel, since Unknown has no characters-table row of its own to key
	// off.
	speakerKey := "unknown"
	if name != "Unknown" {
		char, err := s.Store.GetCharacterByName(store.SeriesScope(book), name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if char == nil {
			writeError(w, http.StatusNotFound, "character not found")
			return
		}
		speakerKey = char.ID
	}

	paragraphs, err := s.Store.ListParagraphsRawForBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	targetByChapter := map[string]map[int]bool{}
	for _, p := range paragraphs {
		if p.Speaker != name {
			continue
		}
		if targetByChapter[p.ChapterID] == nil {
			targetByChapter[p.ChapterID] = map[int]bool{}
		}
		targetByChapter[p.ChapterID][p.Idx] = true
	}

	queued := 0
	for chapterID, targetIdx := range targetByChapter {
		ch, err := s.Store.GetChapterByID(chapterID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ch == nil {
			continue
		}
		s.Jobs.EnqueueReattribution(book.ID, ch.ID, ch.Idx, speakerKey, name, func(ctx context.Context) (int, func(), error) {
			return s.reattributeChapterSpeaker(ctx, book, ch, speakerKey, name, targetIdx)
		})
		queued++
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued})
}

type setCharacterVoiceRequest struct {
	VoicePresetID string `json:"voicePresetId"`
}

// handleSetCharacterVoice assigns (or, with "", clears) a character's own
// narration voice - a character with no voice assigned still narrates in
// the book's own voice even when book.MultiVoice is on (see
// internal/narration.Resolver). The assignment is scoped to the *book's
// own* resolved clone model (see store.Character's doc comment) - a
// reader manually assigning a voice through this book's speaker table sets
// what that character sounds like specifically when narrated through this
// book's model, not through every clone model a character might ever be
// narrated with.
func (s *Server) handleSetCharacterVoice(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	id := r.PathValue("characterId")
	var req setCharacterVoiceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Store.SetCharacterVoice(id, narration.EffectiveCloneModel(bookVoice), req.VoicePresetID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// booksInScope returns every book sharing book's narration-voice scope
// (see store.SeriesScope): every book in the same series, in series order,
// or just book itself if it isn't part of one. Used to gather a recurring
// character's dialogue - and known-character names - across an entire
// series rather than one book at a time.
func (s *Server) booksInScope(book *store.Book) ([]store.Book, error) {
	if book.SeriesName == "" {
		return []store.Book{*book}, nil
	}
	return s.Store.ListSeriesBooks(book.SeriesName)
}

// knownCharacterDescriptionMaxChars bounds how much of a known character's
// own tagged description text (see Store.DescriptionsForBooks) gets handed
// to attribution as identifying context - a short blurb, not the whole
// paragraph, since this rides along on every single attribution batch
// (unlike characterization's own much larger sample, gathered once per
// character, ever).
const knownCharacterDescriptionMaxChars = 160

// knownCharacterDescriptions gathers a short identifying blurb for each of
// existing's characters (their own first tagged description paragraph, if
// they have one - see Store.SetParagraphDescriptions/DescriptionsForBooks),
// keyed by name, for AttributeChapter's own "Known characters" prompt (see
// its own doc comment on why this exists): a narration tag often refers to
// someone by role or epithet rather than their proper name ("the captain
// said," "the old orc grunted") - a bare name list gives the model nothing
// to connect that back to who's actually known, but a one-line "what they
// look like/are" blurb often repeats the very role/epithet the narration
// itself uses, closing that gap. A character with no tagged description
// yet (most of them, especially early in a chapter, or a book where
// description-tagging hasn't run) is simply absent from the result -
// AttributeChapter treats that the same as "no blurb available," not an
// error.
func (s *Server) knownCharacterDescriptions(book *store.Book, existing []store.Character) (map[string]string, error) {
	if len(existing) == 0 {
		return nil, nil
	}
	books, err := s.booksInScope(book)
	if err != nil {
		return nil, err
	}
	bookIDs := make([]string, len(books))
	for i := range books {
		bookIDs[i] = books[i].ID
	}
	out := make(map[string]string, len(existing))
	for _, c := range existing {
		descs, err := s.Store.DescriptionsForBooks(bookIDs, c.Name, 1)
		if err != nil {
			return nil, err
		}
		if len(descs) == 0 {
			continue
		}
		out[c.Name] = truncateForPrompt(descs[0].Text, knownCharacterDescriptionMaxChars)
	}
	return out, nil
}

// truncateForPrompt trims s to at most maxChars runes for embedding in an
// LLM prompt, breaking at the last whitespace before the cutoff when
// possible so it doesn't end mid-word, and appending "..." when actually
// truncated.
func truncateForPrompt(s string, maxChars int) string {
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	cut := string(r[:maxChars])
	if i := strings.LastIndexAny(cut, " \t\n"); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "..."
}

type speakerAppearanceDTO struct {
	BookID       string `json:"bookId"`
	BookTitle    string `json:"bookTitle"`
	ChapterIdx   int    `json:"chapterIdx"`
	ChapterTitle string `json:"chapterTitle"`
	ParagraphIdx int    `json:"paragraphIdx"`
	Text         string `json:"text"`
	// AudioURL is set only once this specific paragraph's audio is ready
	// under whatever voice it currently resolves to (its own book's
	// MultiVoice/character-voice setup, same resolution generation itself
	// uses) - "" otherwise, in which case the frontend offers to generate
	// it instead of playing it.
	AudioURL string `json:"audioUrl,omitempty"`
}

// handleCharacterAppearances lists every paragraph a character speaks,
// across every book in their series (see store.SeriesScope) - backing a
// "where does this character show up" view. bookID in the path is just
// how the character is reached (matching every other .../characters/{id}
// route); the character's own scope, not that one book, decides which
// books are actually searched.
func (s *Server) handleCharacterAppearances(w http.ResponseWriter, r *http.Request) {
	writeBuilt(w)(s.buildCharacterAppearances(r.PathValue("id"), r.PathValue("characterId")))
}

func (s *Server) buildCharacterAppearances(bookID, characterID string) (any, error) {

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}
	char, err := s.Store.GetCharacter(characterID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if char == nil {
		return nil, httpError(http.StatusNotFound, "character not found")
	}

	books, err := s.booksInScope(book)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	bookIDs := make([]string, len(books))
	bookByID := make(map[string]*store.Book, len(books))
	for i := range books {
		bookIDs[i] = books[i].ID
		bookByID[books[i].ID] = &books[i]
	}

	appearances, err := s.Store.ParagraphsForSpeaker(bookIDs, char.Name)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]speakerAppearanceDTO, len(appearances))
	for i, a := range appearances {
		dto := speakerAppearanceDTO{
			BookID: a.BookID, BookTitle: a.BookTitle,
			ChapterIdx: a.ChapterIdx, ChapterTitle: a.ChapterTitle,
			ParagraphIdx: a.ParagraphIdx, Text: a.Text,
		}
		// Individually cheap (one single-row lookup per appearance, never
		// a query keyed on a huge id list - an appearance list is at most
		// a few dozen/hundred rows, nothing like a whole book's worth) -
		// see Store.CountReadyAudioForSpeaker's own doc comment for why
		// that distinction matters here.
		if b, ok := bookByID[a.BookID]; ok {
			if v, err := s.Narration.ForParagraph(b, char.Name); err == nil {
				dto.AudioURL = s.resolveAppearanceAudioURL(a.ParagraphID, v.VoiceID())
			}
		}
		out[i] = dto
	}
	return out, nil
}

// resolveAppearanceAudioURL is handleCharacterAppearances/
// handleCharacterDescriptions' own shared audio-preview lookup - unlike
// resolveAudioID (handleGetChapter's own, which has every paragraph in the
// chapter in hand to resolve a scare-quote merge group pointer against),
// this deliberately does NOT resolve a pointer paragraph (see
// store.AudioState.PointerOffset) to its anchor's own file: doing so here
// would cost a second chapter/paragraph lookup per appearance row for a
// case rare enough (a paragraph tagged both a character's own dialogue/
// description AND part of a scare-quote merge group) not to be worth it
// for what's just a preview-clip convenience, not real playback - a
// pointer paragraph simply shows no preview clip here rather than the
// wrong one.
func (s *Server) resolveAppearanceAudioURL(paragraphID, voiceID string) string {
	states, err := s.Store.ParagraphAudioStatuses([]string{paragraphID}, voiceID)
	if err != nil {
		return ""
	}
	state, ok := states[paragraphID]
	if !ok || state.Status != store.AudioReady || state.PointerOffset != 0 {
		return ""
	}
	return "/api/paragraphs/" + paragraphID + "/audio"
}

// handleCharacterDescriptions lists every paragraph that describes a
// character's physical appearance or personality (see
// internal/speakerattr's DescribeChapter and Store.SetParagraphDescriptions),
// across every book in their series (see store.SeriesScope) -
// handleCharacterAppearances' own description-tagging counterpart, same DTO
// shape and the same "bookID in the path is just how the character is
// reached" rationale. A description paragraph's own attributed speaker is
// always "Narrator" (or "", pre-attribution) - see
// Store.ParagraphsDescribing's own doc comment - so audio resolution here
// always goes through the "Narrator" name, unlike handleCharacterAppearances'
// own char.Name: it's previewing the narration itself, not this character's
// own (nonexistent, for a paragraph they don't speak) dialogue.
func (s *Server) handleCharacterDescriptions(w http.ResponseWriter, r *http.Request) {
	writeBuilt(w)(s.buildCharacterDescriptions(r.PathValue("id"), r.PathValue("characterId")))
}

func (s *Server) buildCharacterDescriptions(bookID, characterID string) (any, error) {

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}
	char, err := s.Store.GetCharacter(characterID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if char == nil {
		return nil, httpError(http.StatusNotFound, "character not found")
	}

	books, err := s.booksInScope(book)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	bookIDs := make([]string, len(books))
	bookByID := make(map[string]*store.Book, len(books))
	for i := range books {
		bookIDs[i] = books[i].ID
		bookByID[books[i].ID] = &books[i]
	}

	descriptions, err := s.Store.ParagraphsDescribing(bookIDs, char.Name)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]speakerAppearanceDTO, len(descriptions))
	for i, a := range descriptions {
		dto := speakerAppearanceDTO{
			BookID: a.BookID, BookTitle: a.BookTitle,
			ChapterIdx: a.ChapterIdx, ChapterTitle: a.ChapterTitle,
			ParagraphIdx: a.ParagraphIdx, Text: a.Text,
		}
		if b, ok := bookByID[a.BookID]; ok {
			if v, err := s.Narration.ForParagraph(b, "Narrator"); err == nil {
				dto.AudioURL = s.resolveAppearanceAudioURL(a.ParagraphID, v.VoiceID())
			}
		}
		out[i] = dto
	}
	return out, nil
}

// attributeChapter runs speaker attribution (internal/speakerattr) over
// ch's paragraphs and persists the results: every newly-seen character
// name is registered (store.Store.UpsertCharacter, scoped to book's whole
// series - see store.SeriesScope - starting with no voice assigned) and
// every paragraph's Speaker is set. known is re-fetched from the store on
// every call (not cached across chapters or books) so a character
// discovered anywhere earlier in the series - not just earlier in this
// book - is always offered to the model as a name to reuse, keeping
// naming consistent across the whole series (see
// speakerattr.Client.AttributeChapter). Unlike an earlier version, this no
// longer characterizes or assigns a voice to newly-discovered characters
// itself - that now happens lazily, the first time a character's voice is
// actually needed for generation (see jobs.Manager.
// provisionMissingCharacterVoices/CharacterVoiceProvisioner,
// provisionCharacterVoice below), so a chapter with many brand-new
// characters attributes just as fast as one with none.
//
// onlyUnattributed restricts the paragraphs actually sent to the model to
// ones with no Speaker yet, instead of every paragraph in the chapter -
// false for every reader-triggered call (handleAttributeSpeakers: an
// explicit "Attribute speakers"/"Attribute all" click always means
// "redo the whole chapter," the same idempotent refresh it's always been,
// even for paragraphs already attributed - see SpeakersPage's own comment
// on that button), true only when this call is itself the automatic
// continuation of an earlier one that paused partway through (see
// speakerattr.Client.AttributeChapter's shouldPause and the requeue
// closure built below) - by the time that continuation runs, everything
// the first attempt actually finished is already persisted, so redoing it
// would just waste LLM calls repeating identical work.
//
// This satisfies AttributionFunc's shape directly (see jobs.Manager.
// EnqueueAttribution): requeue is non-nil exactly when speakerattr paused
// before reaching every paragraph, and calling it enqueues exactly this
// same continuation.
func (s *Server) attributeChapter(ctx context.Context, book *store.Book, ch *store.Chapter, onlyUnattributed bool) (attributed int, requeue func(), err error) {
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	paragraphs := all
	if onlyUnattributed {
		paragraphs = make([]store.Paragraph, 0, len(all))
		for _, p := range all {
			if p.Speaker == "" {
				paragraphs = append(paragraphs, p)
			}
		}
	}
	if len(paragraphs) == 0 {
		return 0, nil, nil
	}

	scope := store.SeriesScope(book)
	existing, err := s.Store.ListCharacters(scope)
	if err != nil {
		return 0, nil, err
	}
	known := make([]string, len(existing))
	knownRoles := make(map[string]bool, len(existing))
	for i, c := range existing {
		known[i] = c.Name
		if c.IsRole {
			knownRoles[c.Name] = true
		}
	}
	knownDescriptions, err := s.knownCharacterDescriptions(book, existing)
	if err != nil {
		return 0, nil, err
	}

	inputs := make([]speakerattr.ParagraphInput, len(paragraphs))
	for i, p := range paragraphs {
		// p.Inline (see store.Paragraph.Inline) means "continues the
		// immediately preceding paragraph in the chapter" - true only
		// meaningfully when that true predecessor (idx-1) is itself part
		// of inputs right next to it; onlyUnattributed can filter that
		// predecessor out (already attributed, and so already persisted,
		// in an earlier batch/dispatch of this same paused-and-resumed
		// run) while leaving this one in, which would otherwise silently
		// tell speakerattr.attributeBatch's "Lines:" formatting that this
		// paragraph continues whatever unrelated paragraph now happens to
		// precede it in the filtered list instead of starting a new
		// visible line - corrupting the model's own paragraph-boundary
		// signal for every batch after the first gap. Force it false
		// whenever the true predecessor isn't right there; a no-op for
		// the normal onlyUnattributed=false path, where paragraphs is
		// exactly all and every idx-1 predecessor is always present.
		inline := p.Inline
		if inline && (i == 0 || paragraphs[i-1].Idx != p.Idx-1) {
			inline = false
		}
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: inline}
	}

	// nil, not s.Jobs.HasHigherPriorityWork: every LLM pass (attribution,
	// direction, sfx, pronunciation, music) now runs each dispatched batch
	// through to completion instead of yielding mid-run - see this
	// package's own removal-of-pausing note by the direction/sfx/
	// pronunciation call site in directChapter for the full reasoning
	// (a whole-book preprocess phase dispatches each chapter's own pass
	// exactly once, with no automatic retry loop of its own, so a pause
	// under real contention could leave most of a bulk run's chapters
	// silently stuck half-done with nothing left in the queue to ever
	// pick them back up).
	speakers, roleNames, remaining, attrErr := s.Speaker.AttributeChapter(ctx, book.Title, ch.Title, known, knownDescriptions, knownRoles, inputs, nil)

	paragraphByIdx := make(map[int]store.Paragraph, len(all))
	for _, p := range all {
		paragraphByIdx[p.Idx] = p
	}

	bySpeakerIdx := make(map[int]string, len(speakers))
	for idx, name := range speakers {
		if name == "" {
			continue
		}
		// Only an actual quoted-dialogue span may be attributed to a
		// character - narration/description about someone isn't them
		// speaking, whatever the model itself decided. Enforced here,
		// deterministically, rather than left to the model's own prompt
		// adherence (see speakerattr's own system prompt, which already
		// asks for this but can't guarantee it).
		if name != "Narrator" {
			if p, ok := paragraphByIdx[idx]; ok && !p.IsQuote {
				name = "Narrator"
			}
		}
		bySpeakerIdx[idx] = name
		if name == "Narrator" || name == "Unknown" {
			continue
		}
		if _, _, uerr := s.Store.UpsertCharacter(scope, name, roleNames[name]); uerr != nil {
			return 0, nil, uerr
		}
	}
	// Persisted regardless of attrErr below - a batch failing partway
	// through a chapter (a real, observed failure mode: a small local
	// model occasionally blows its whole token budget on an unclosed
	// <think> trace and never produces JSON) shouldn't discard whatever
	// earlier batches in *this same call* actually did succeed at.
	if serr := s.Store.SetParagraphSpeakers(ch.ID, bySpeakerIdx); serr != nil {
		return 0, nil, serr
	}

	if attrErr != nil {
		// A genuine failure, not a pause - surfaced to the caller (logged
		// by jobs.Manager.handleResult) rather than auto-requeued, so a
		// batch that keeps failing the same way doesn't loop forever;
		// a reader can always retry explicitly.
		return len(bySpeakerIdx), nil, attrErr
	}
	if len(remaining) > 0 {
		requeue = func() {
			s.Jobs.EnqueueAttribution(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
				return s.attributeChapter(ctx, book, ch, true)
			})
		}
		return len(bySpeakerIdx), requeue, nil
	}
	// A full, uninterrupted run over the whole chapter just finished -
	// advance its pipeline progress persistently (see store.Passes' own
	// doc comment for why this is a stored fact, not derived from
	// paragraphs.speaker counts). Best-effort: a failure here shouldn't
	// undo the attribution work itself (bySpeakerIdx is already committed
	// above), just log rather than fail the call outright.
	if serr := s.Store.SetChapterAttributed(ch.ID); serr != nil {
		log.Printf("attributeChapter: could not advance chapter %s passes: %v", ch.ID, serr)
	}

	// Description-tagging (speakerattr.Client.DescribeChapter) runs only
	// once attribution has genuinely finished the whole chapter in one
	// uninterrupted go (not a paused/resumed partial run - see the
	// len(remaining) > 0 branch above, which returns before reaching here)
	// - over every paragraph in the chapter (all, not just this call's own
	// possibly-filtered paragraphs/onlyUnattributed subset), since a
	// description can appear in narration attributed in an earlier
	// dispatch of this same chapter. Best-effort and entirely separate from
	// attribution's own success: a description-tagging failure (this is a
	// smaller, separate LLM call - see DescribeChapter's own doc comment on
	// why this is split out from AttributeChapter rather than folded in) is
	// logged here, not returned, so it never undoes or blocks the speaker
	// attribution this call already committed above - unlike
	// handleRetagDescriptions' own direct call to describeChapter, an
	// explicit reader-triggered retag where a failure genuinely should
	// surface.
	if derr := s.describeChapter(ctx, book, ch, all); derr != nil {
		log.Printf("attributeChapter: describe chapter %s: %v", ch.ID, derr)
	}

	// Scare-quote tagging (speakerattr.Client.ScareQuoteChapter) is
	// describeChapter's own sibling in every way that matters here: a
	// smaller, separate LLM call, best-effort and decoupled from
	// attribution's own success, run only once attribution has genuinely
	// finished the whole chapter (this same guard - reaching past the
	// len(remaining) > 0 branch above - applies to both).
	if serr := s.scareQuoteChapter(ctx, book, ch, all); serr != nil {
		log.Printf("attributeChapter: scare-quote chapter %s: %v", ch.ID, serr)
	}

	return len(bySpeakerIdx), nil, nil
}

// reattributeChapterSpeaker re-runs speaker attribution over every
// paragraph in ch - the same full-chapter context attributeChapter itself
// sends the model (dialogue turn-taking and narration tags either side of a
// line are often what actually identifies its speaker - a batch containing
// only excludeName's own isolated lines, with no narration around them,
// would lose exactly that context) - but persists a paragraph's fresh
// judgment only when its idx is in targetIdx: the paragraphs currently
// credited to excludeName, the speaker "Auto Split" is trying to eliminate
// (see handleReattributeSpeaker). Every other paragraph in ch is re-judged
// by the model too (spending the same LLM call attributeChapter would
// anyway) but its result is simply discarded - already correctly
// attributed, it has no business changing just because this run happened
// to touch its chapter.
//
// excludeName itself is withheld from "Known characters" (existing, minus
// excludeName), so the model has no established identity to reuse for it -
// its own former paragraphs have to land on a genuinely different real
// character, Narrator, or Unknown. Also enforced defensively in Go, not
// just by omission from the prompt: if the model still emits excludeName
// verbatim for one of targetIdx's own paragraphs (e.g. because the name
// still appears elsewhere in the chapter's own text, as a vocative or a
// mention), that result is forced to "Unknown" instead - never persisted
// as a no-op reattribution back onto the exact name being eliminated.
//
// Satisfies jobs.ReattributionFunc's shape directly (see
// jobs.Manager.EnqueueReattribution): requeue is non-nil exactly when
// speakerattr paused before reaching every paragraph in ch, and calling it
// enqueues exactly this same continuation, narrowed to whichever of
// targetIdx's own paragraphs speakerattr never actually reached.
func (s *Server) reattributeChapterSpeaker(ctx context.Context, book *store.Book, ch *store.Chapter, speakerKey, excludeName string, targetIdx map[int]bool) (reattributed int, requeue func(), err error) {
	if len(targetIdx) == 0 {
		return 0, nil, nil
	}
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}

	scope := store.SeriesScope(book)
	existing, err := s.Store.ListCharacters(scope)
	if err != nil {
		return 0, nil, err
	}
	known := make([]string, 0, len(existing))
	knownRoles := make(map[string]bool, len(existing))
	for _, c := range existing {
		if c.Name == excludeName {
			continue
		}
		known = append(known, c.Name)
		if c.IsRole {
			knownRoles[c.Name] = true
		}
	}
	knownDescriptions, err := s.knownCharacterDescriptions(book, existing)
	if err != nil {
		return 0, nil, err
	}
	delete(knownDescriptions, excludeName)

	inputs := make([]speakerattr.ParagraphInput, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote}
	}

	// nil, not s.Jobs.HasHigherPriorityWork - see the first AttributeChapter
	// call site's own doc comment above.
	speakers, roleNames, remaining, attrErr := s.Speaker.AttributeChapter(ctx, book.Title, ch.Title, known, knownDescriptions, knownRoles, inputs, nil)

	paragraphByIdx := make(map[int]store.Paragraph, len(all))
	for _, p := range all {
		paragraphByIdx[p.Idx] = p
	}

	bySpeakerIdx := make(map[int]string, len(targetIdx))
	for idx := range targetIdx {
		name, ok := speakers[idx]
		if !ok {
			continue
		}
		if name != "Narrator" {
			if p, ok := paragraphByIdx[idx]; ok && !p.IsQuote {
				name = "Narrator"
			}
		}
		if name == excludeName {
			name = "Unknown"
		}
		bySpeakerIdx[idx] = name
		if name == "Narrator" || name == "Unknown" {
			continue
		}
		if _, _, uerr := s.Store.UpsertCharacter(scope, name, roleNames[name]); uerr != nil {
			return 0, nil, uerr
		}
	}
	// Persisted regardless of attrErr below - same "keep whatever actually
	// succeeded" treatment attributeChapter gives its own partial-failure
	// case.
	if serr := s.Store.SetParagraphSpeakers(ch.ID, bySpeakerIdx); serr != nil {
		return 0, nil, serr
	}

	if attrErr != nil {
		return len(bySpeakerIdx), nil, attrErr
	}
	if len(remaining) > 0 {
		stillTarget := make(map[int]bool, len(remaining))
		for _, p := range remaining {
			if targetIdx[p.Idx] {
				stillTarget[p.Idx] = true
			}
		}
		if len(stillTarget) > 0 {
			requeue = func() {
				s.Jobs.EnqueueReattribution(book.ID, ch.ID, ch.Idx, speakerKey, excludeName, func(ctx context.Context) (int, func(), error) {
					return s.reattributeChapterSpeaker(ctx, book, ch, speakerKey, excludeName, stillTarget)
				})
			}
		}
	}
	return len(bySpeakerIdx), requeue, nil
}

// describeChapter runs speakerattr.Client.DescribeChapter over every
// paragraph in chapter and persists which ones describe a character
// (Store.SetParagraphDescriptions), upserting any newly-discovered
// character it names along the way - the same deterministic is_quote gate
// attributeChapter already applies to Speaker applies here to Describes,
// since narration *about* someone isn't the same as them speaking, whatever
// the model output. Shared by attributeChapter's own automatic, best-effort
// call (which logs and swallows the error this returns) and
// handleRetagDescriptions' explicit, reader-triggered one (which surfaces
// it as a real failure) - see each call site for which treatment applies.
// Any error returned is specifically DescribeChapter's own (a genuine LLM/
// parse failure, not a pause - see its own doc comment) or a persistence
// failure; per-character UpsertCharacter failures along the way are
// individually logged and skipped rather than failing the whole call, the
// same best-effort-within-a-best-effort treatment attributeChapter's own
// speaker-upsert loop doesn't need (a single UpsertCharacter failure there
// already fails outright, but this call site never blocks anything more
// important on getting every single character registered).
func (s *Server) describeChapter(ctx context.Context, book *store.Book, ch *store.Chapter, all []store.Paragraph) error {
	scope := store.SeriesScope(book)
	existing, err := s.Store.ListCharacters(scope)
	if err != nil {
		return fmt.Errorf("list characters: %w", err)
	}
	known := make([]string, len(existing))
	for i, c := range existing {
		known[i] = c.Name
	}

	inputs := make([]speakerattr.ParagraphInput, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline}
	}

	descriptions, _, describeErr := s.Speaker.DescribeChapter(ctx, book.Title, ch.Title, known, inputs, nil)
	// Persisted below regardless of describeErr - descriptions still holds
	// whatever batches completed before a failing one, same "persist what
	// actually succeeded" treatment attributeChapter gives a partial
	// AttributeChapter error; describeErr itself is still returned at the
	// end, after that persistence, so a caller sees the failure without
	// losing the real progress this call already made.

	paragraphByIdx := make(map[int]store.Paragraph, len(all))
	for _, p := range all {
		paragraphByIdx[p.Idx] = p
	}

	byDescribesIdx := make(map[int][]string, len(descriptions))
	for idx, names := range descriptions {
		// Only an actual narration paragraph may describe someone - the
		// same is_quote gate attributeChapter applies to Speaker, and for
		// the same reason: a quoted-dialogue span can't simultaneously be
		// a description of someone else, whatever the model output. No
		// found-paragraph fallback trusting the model: a description is
		// only ever kept when this paragraph is positively confirmed
		// non-quote.
		p, found := paragraphByIdx[idx]
		if !found || p.IsQuote {
			continue
		}
		byDescribesIdx[idx] = names
		for _, name := range names {
			// A description can register a character not yet in scope -
			// see DescribeChapter's own doc comment: a character can be
			// discovered by how they're described before they ever speak.
			if _, _, uerr := s.Store.UpsertCharacter(scope, name, false); uerr != nil {
				log.Printf("describeChapter: upsert character %q for chapter %s: %v", name, ch.ID, uerr)
			}
		}
	}
	if serr := s.Store.SetParagraphDescriptions(ch.ID, byDescribesIdx); serr != nil {
		return fmt.Errorf("persist descriptions: %w", serr)
	}
	return describeErr
}

// scareQuoteChapter runs speakerattr.Client.ScareQuoteChapter over every
// quoted paragraph in chapter and persists which ones are scare quotes
// (Store.SetParagraphScareQuotes) - describeChapter's own sibling,
// gathered by attributeChapter's automatic best-effort call (logging and
// swallowing the error this returns) and handleRetagScareQuotes' explicit,
// reader-triggered one (surfacing it as a real failure).
//
// Invalidates every paragraph in the inline paragraph set (see
// expandToInlineSets) of any paragraph whose own ScareQuote flag actually
// changed this run - never the whole chapter, unlike directChapter's own
// blunt "tagged anything -> invalidate everything" choice for delivery
// tags, but not narrower than that either: see expandToInlineSets' own
// doc comment for why a changed paragraph's Inline neighbors need
// invalidating too, not just the paragraph itself.
func (s *Server) scareQuoteChapter(ctx context.Context, book *store.Book, ch *store.Chapter, all []store.Paragraph) error {
	inputs := make([]speakerattr.ParagraphInput, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote}
	}

	scareQuotes, _, scareQuoteErr := s.Speaker.ScareQuoteChapter(ctx, book.Title, ch.Title, inputs, nil)
	// Persisted below regardless of scareQuoteErr, same "keep whatever
	// batches actually completed" treatment describeChapter gives its own
	// partial-failure case.

	byIdx := make(map[int]bool, len(scareQuotes))
	var changed []store.Paragraph
	for _, p := range all {
		want := scareQuotes[p.Idx]
		if want == p.ScareQuote {
			continue
		}
		byIdx[p.Idx] = want
		p.ScareQuote = want
		changed = append(changed, p)
	}
	if serr := s.Store.SetParagraphScareQuotes(ch.ID, byIdx); serr != nil {
		return fmt.Errorf("persist scare quotes: %w", serr)
	}
	s.invalidateParagraphAudio(book, s.expandToInlineSets(ch.ID, changed))
	return scareQuoteErr
}

// expandToInlineSets expands paragraphs to the union (deduped by
// paragraph ID) of each one's own inline paragraph set
// (jobs.Manager.InlineParagraphSet) - the full run reachable by following
// Inline in either direction.
//
// A ScareQuote flag changing on any one paragraph can change whether its
// whole Inline-linked run is eligible for scare-quote merging (see
// jobs.Manager.scareQuoteMergeGroup), so every member's audio needs
// invalidating, not just the paragraph whose flag actually changed.
// Growing a merge group self-heals without this - handleMergedResult
// rewrites every member's audio the next time the changed paragraph is
// dispatched, whatever their prior status - but *shrinking* one doesn't:
// once the changed paragraph regenerates alone (its flag no longer keeps
// the group eligible), its former merge-mates are left exactly as they
// were - the anchor still holding the old, now-wrong multi-paragraph
// recording on disk with `AudioReady` status, a former pointer member
// still pointing into it - and neither is ever re-examined, since nothing
// else marks an already-`AudioReady` paragraph as needing generation
// again.
func (s *Server) expandToInlineSets(chapterID string, paragraphs []store.Paragraph) []store.Paragraph {
	seen := make(map[string]bool, len(paragraphs))
	var out []store.Paragraph
	for _, p := range paragraphs {
		for _, member := range s.Jobs.InlineParagraphSet(chapterID, p) {
			if seen[member.ID] {
				continue
			}
			seen[member.ID] = true
			out = append(out, member)
		}
	}
	return out
}

// invalidateParagraphAudio resets each of paragraphs' own already-generated
// audio (if any) back to pending under its own currently-resolved voice,
// removing its on-disk file too - scareQuoteChapter's own scoped
// invalidation and handleSetParagraphScareQuote's manual-toggle
// counterpart. Best-effort: a resolution/delete failure for one paragraph
// is logged and skipped rather than failing the whole batch, since this
// always runs after the real state change (the ScareQuote flag itself) has
// already been persisted - a paragraph whose stale audio couldn't be
// cleared here just means a reader has to notice and hit "regenerate"
// themselves, not a lost update.
func (s *Server) invalidateParagraphAudio(book *store.Book, paragraphs []store.Paragraph) {
	for _, p := range paragraphs {
		voice, err := s.Narration.ForParagraph(book, p.Speaker)
		if err != nil {
			log.Printf("invalidateParagraphAudio: resolve voice for paragraph %s: %v", p.ID, err)
			continue
		}
		voiceID := voice.VoiceID()
		if err := os.Remove(audiopath.ParagraphFile(s.DataDir, book.ID, p.ChapterID, voiceID, p.Idx)); err != nil && !os.IsNotExist(err) {
			log.Printf("invalidateParagraphAudio: remove audio file for paragraph %s: %v", p.ID, err)
		}
		if err := s.Store.ResetParagraphAudio(p.ID, voiceID); err != nil {
			log.Printf("invalidateParagraphAudio: reset paragraph %s: %v", p.ID, err)
		}
	}
}

// directChapter runs all three of speakerattr's per-paragraph
// preprocessing passes (Client.DirectChapter for sentence-level emotion/
// style/prosody, Client.TagSfx for positional sfx/pause tags,
// Client.ResolvePronunciation for ambiguous-abbreviation disambiguation)
// over chapter's paragraphs and persists each one's own independent
// annotation (Store.SetParagraphSentenceTags/SetParagraphInlineTags/
// SetParagraphPronunciation), the first two keyed to the book's currently
// resolved clone model, pronunciation not (see its own bundling note
// below). Unlike describeChapter, this is never chained automatically
// after attributeChapter - it's independently triggered
// (handleTagDirections) since it's genuinely optional/stylistic, with no
// other feature depending on its output the way Characterize depends on
// Describe, and hasn't yet been benchmarked for prompt quality against
// real chapters the way attribution/description were.
//
// Only runs for Higgs's own clone model (voices.DefaultCloneModel,
// "audiocpp-higgs-4b" today): the delivery-tag vocabulary itself
// (speakerattr.validSentenceTags/validInlineTags) is specific to
// higgs_audio_tts's own tokenizer - tagging under any other clone model
// would just have audio.cpp's tokenizer encode the tag text as literal
// characters, which the model would then try to pronounce, not silently
// ignore. Pronunciation resolution has no such restriction (plain word
// substitution reads correctly under any clone model) but is bundled
// behind this same gate anyway for v1 - see the call site's own comment.
// Returns (0, nil, nil) as a no-op for any other clone model rather than
// an error - handleTagDirections itself checks this
// synchronously before enqueueing so a reader gets an immediate, clear
// rejection instead of a task that silently does nothing; this check is a
// second, defensive layer in case the book's voice changes between
// enqueue and dispatch.
//
// onlyIdx restricts the paragraphs actually sent to the model to those
// whose Idx is in the set, instead of every paragraph in the chapter - nil
// for every reader-triggered call (handleTagDirections: an explicit
// "Tag directions"/"Tag all"/"Tag undirected" click always means "run over
// the whole chapter," the same idempotent refresh it's always been), non-
// nil only when this call is itself the automatic continuation of an
// earlier one that paused partway through (see the requeue closure built
// below). Unlike attributeChapter's own onlyUnattributed (a plain bool,
// re-derived from paragraphs.speaker on every call), this can't be
// re-derived from stored state the same way: a paragraph legitimately
// getting no tag at all is indistinguishable from one never checked, the
// same ambiguity Chapter.Directed's own doc comment describes at the
// chapter level - so the continuation instead carries forward exactly
// which paragraphs speakerattr itself reported as not yet reached.
//
// Every LLM pass below is passed nil, not s.Jobs.HasHigherPriorityWork,
// for its own shouldPause parameter - pausing used to let a long
// TierBackground run (SpeakersPage's "Tag all"/"Tag undirected" buttons,
// runDirectionAll/runDirectionUndirected, can queue a whole book's worth
// of chapters at once) yield to a just-arrived, more urgent task instead
// of making it wait out however many chapters/batches were left. Disabled
// now for a real, observed problem it caused instead: a whole-book
// preprocess phase (httpapi.preprocessDirectionPhase, the music-scoring
// pipeline's own identical Phase 5 shape) dispatches each chapter's own
// pass exactly once, with no automatic retry loop of its own - under real
// contention (many chapters' worth of these same passes all competing for
// poolLLM's single shared slot at once), most chapters would pause after
// just their first batch and never get automatically retried, so the
// queue could drain to empty while the vast majority of the book was
// still nowhere near actually tagged. Every dispatched batch now runs
// straight through to completion instead.
// clearStaleDirectionTags returns tagged with an explicit "" entry added
// for every paragraph in paragraphs that this run actually processed
// (i.e. not left in remaining for a later continuation via onlyIdx) but
// didn't itself decide to tag, and which still carries a non-empty stored
// value from some *earlier* run for the exact field current reads (either
// ParagraphDirection.SentenceText or .InlineText, whichever field this
// call is about) - see directChapter's own call site for the full
// "re-tagging should replace, not just add" reasoning. Paragraphs that
// already have no stored value, or that weren't processed this run at all
// (still in remaining), are left out entirely - the same "absent means
// untouched" contract Store.setParagraphDirectionField already relies on,
// so a genuinely no-op re-tag (nothing to clear, nothing new to tag)
// stays a no-op: no spurious write, no spurious chapter-audio
// invalidation via directChapter's own touched tracking below.
func clearStaleDirectionTags(tagged map[int]string, paragraphs []store.Paragraph, remaining []speakerattr.ParagraphInput, current func(store.Paragraph) string) map[int]string {
	remainingSet := make(map[int]bool, len(remaining))
	for _, p := range remaining {
		remainingSet[p.Idx] = true
	}
	for _, p := range paragraphs {
		if remainingSet[p.Idx] {
			continue
		}
		if _, ok := tagged[p.Idx]; ok {
			continue
		}
		if current(p) != "" {
			tagged[p.Idx] = ""
		}
	}
	return tagged
}

func (s *Server) directChapter(ctx context.Context, book *store.Book, ch *store.Chapter, onlyIdx map[int]bool) (tagged int, requeue func(), err error) {
	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		return 0, nil, err
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)
	// higgsTags gates only the two passes whose tag vocabulary is actually
	// Higgs-specific (see the doc comment further down, by the calls
	// themselves) - pronunciation resolution runs for every clone model
	// regardless (see its own call site's doc comment for why).
	higgsTags := cloneModel == voices.DefaultCloneModel

	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	paragraphs := all
	if onlyIdx != nil {
		paragraphs = make([]store.Paragraph, 0, len(onlyIdx))
		for _, p := range all {
			if onlyIdx[p.Idx] {
				paragraphs = append(paragraphs, p)
			}
		}
	}
	if len(paragraphs) == 0 {
		return 0, nil, nil
	}

	inputs := make([]speakerattr.ParagraphInput, len(paragraphs))
	for i, p := range paragraphs {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote}
	}

	// Three independent LLM passes - sentence-level (emotion/style/prosody
	// speed|pitch|expressive), positional (sfx/prosody pause|long_pause),
	// and pronunciation (ambiguous-abbreviation disambiguation) - see
	// speakerattr.Client.DirectChapter/TagSfx/ResolvePronunciation's own
	// doc comments for why these stay separate calls rather than one. All
	// three that run share the same "unconditional, an error in one
	// doesn't skip the others" treatment: each annotates its own
	// independent storage (Store.SetParagraphSentenceTags/
	// SetParagraphInlineTags/SetParagraphPronunciation), so there's
	// nothing for one pass's failure to corrupt in another's output. Each
	// also independently reports its own remaining paragraphs if it stops
	// partway on a genuine error (shouldPause is nil for all three now -
	// see this function's own doc comment above) - the passes can stop at
	// different points, so the requeue built below carries forward the
	// union of all of them rather than assuming they align.
	//
	// DirectChapter/TagSfx are Higgs-only (higgsTags): their tag vocabulary
	// is baked into Higgs's own tokenizer and means nothing to any other
	// family's decode (see store.Paragraph.Tags' own doc comment).
	// ResolvePronunciation runs regardless of clone model - a "Dr." ->
	// "Doctor" substitution reads correctly under any of them, so there's
	// no reason to gate it on higgsTags too.
	var (
		sentenceTags      map[int]string
		sentenceRemaining []speakerattr.ParagraphInput
		sentenceErr       error
		inlineTags        map[int]string
		inlineRemaining   []speakerattr.ParagraphInput
		inlineErr         error
	)
	if higgsTags {
		sentenceTags, sentenceRemaining, sentenceErr = s.Speaker.DirectChapter(ctx, book.Title, ch.Title, inputs, nil)
		inlineTags, inlineRemaining, inlineErr = s.Speaker.TagSfx(ctx, book.Title, ch.Title, inputs, nil)
	}
	pronunciation, pronunciationRemaining, pronunciationErr := s.Speaker.ResolvePronunciation(ctx, book.Title, ch.Title, inputs, nil)

	if higgsTags {
		// A paragraph absent from sentenceTags/inlineTags means "this pass
		// decided it needs no tag this run" (the overwhelmingly common case -
		// see DirectChapter/TagSfx's own doc comments) - but setParagraphDirectionField
		// treats an absent paragraph as simply untouched, not explicitly
		// cleared. Without this, re-tagging a chapter could only ever add or
		// change tags, never remove one a *previous* run set that the current
		// model logic no longer agrees with (confirmed in production: an
		// emotion tag DirectChapter had placed on a narration paragraph before
		// stripEmotionFromNonQuotes existed stayed there forever afterward,
		// since a fresh run simply never mentions that paragraph again rather
		// than actively clearing it). clearStaleDirectionTags adds an explicit
		// "" entry for exactly the paragraphs that need one - processed this
		// run (not left for a later continuation) but not retagged, and only
		// when they actually still carry a stale value from before - so a
		// genuinely no-op re-tag (nothing to clear, nothing to add) stays a
		// no-op rather than rewriting and invalidating audio for a chapter
		// that didn't actually change.
		sentenceTags = clearStaleDirectionTags(sentenceTags, paragraphs, sentenceRemaining, func(p store.Paragraph) string {
			return p.Tags[cloneModel].SentenceText
		})
		inlineTags = clearStaleDirectionTags(inlineTags, paragraphs, inlineRemaining, func(p store.Paragraph) string {
			return p.Tags[cloneModel].InlineText
		})

		// Persisted regardless of any error - a batch failing partway through
		// a chapter shouldn't discard whatever earlier batches in this same
		// call actually tagged, same "persist what succeeded" treatment
		// attributeChapter/describeChapter give their own partial errors.
		if serr := s.Store.SetParagraphSentenceTags(ch.ID, cloneModel, sentenceTags); serr != nil {
			return 0, nil, serr
		}
		if serr := s.Store.SetParagraphInlineTags(ch.ID, cloneModel, inlineTags); serr != nil {
			return 0, nil, serr
		}
	}
	if serr := s.Store.SetParagraphPronunciation(ch.ID, pronunciation); serr != nil {
		return 0, nil, serr
	}

	// touched is the union of all three passes' own touched paragraphs (a
	// line can get an entry from more than one - e.g. a sentence-level
	// emotion tag and a resolved "Dr."), counted once each for the
	// "tagged" count this returns and for deciding whether anything
	// actually changed below.
	touched := make(map[int]bool, len(sentenceTags)+len(inlineTags)+len(pronunciation))
	for idx := range sentenceTags {
		touched[idx] = true
	}
	for idx := range inlineTags {
		touched[idx] = true
	}
	for idx := range pronunciation {
		touched[idx] = true
	}

	if len(touched) > 0 {
		// A newly (or differently) tagged paragraph's already-generated
		// audio no longer reflects what it should actually say, so it's
		// stale - wipe the whole chapter's generated audio, the same
		// DB-delete/disk-delete pairing handleDeleteChapterAudio's own
		// "Clear generation" button uses, so every paragraph regenerates
		// (picking up its own tags, if any) next time this chapter is
		// read/generated. Blunt (the whole chapter, not just the
		// paragraphs that actually got a tag this run) rather than
		// surgical - simpler and more predictable than tracking which
		// paragraphs' tags actually changed value from a previous run.
		// Skipped entirely when nothing was tagged: nothing changed, so
		// nothing is actually stale, and invalidating a whole chapter's
		// audio for no reason would just force pointless regeneration.
		if serr := s.Store.DeleteChapterAudio(ch.ID); serr != nil {
			log.Printf("directChapter: invalidate audio for chapter %s: %v", ch.ID, serr)
		} else {
			_ = os.RemoveAll(fmt.Sprintf("%s/audio/%s/%s", s.DataDir, book.ID, ch.ID))
		}
	}
	if sentenceErr != nil || inlineErr != nil || pronunciationErr != nil {
		// A genuine failure partway through one or more passes, not a
		// pause - don't mark cloneModel directed for this chapter yet (same
		// "only mark on a full, uninterrupted run" rule attributeChapter's
		// own SetChapterAttributed follows), so a later re-run doesn't skip
		// it. Reports whichever error occurred first, preferring sentence
		// over inline over pronunciation if more than one failed.
		if sentenceErr != nil {
			return len(touched), nil, sentenceErr
		}
		if inlineErr != nil {
			return len(touched), nil, inlineErr
		}
		return len(touched), nil, pronunciationErr
	}
	if len(sentenceRemaining) > 0 || len(inlineRemaining) > 0 || len(pronunciationRemaining) > 0 {
		// At least one pass paused for higher-priority poolLLM work before
		// reaching every paragraph it was given - build the union of all
		// three passes' own remaining paragraphs and requeue exactly that
		// set as a follow-up task under this same chapter's dedup key (see
		// jobs.Manager.EnqueueDirection/pushTask). If only some passes
		// paused, the others (already fully finished this run) still get
		// re-invoked on the continuation, over this narrower onlyIdx set -
		// a few wasted-but-harmless calls re-covering paragraphs already
		// handled (SetParagraphSentenceTags/InlineTags/Pronunciation all
		// overwrite idempotently per-idx), traded for not having to track
		// each pass's own completion separately across a chain of
		// continuations.
		remainingIdx := make(map[int]bool, len(sentenceRemaining)+len(inlineRemaining)+len(pronunciationRemaining))
		for _, p := range sentenceRemaining {
			remainingIdx[p.Idx] = true
		}
		for _, p := range inlineRemaining {
			remainingIdx[p.Idx] = true
		}
		for _, p := range pronunciationRemaining {
			remainingIdx[p.Idx] = true
		}
		requeue = func() {
			s.Jobs.EnqueueDirection(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
				return s.directChapter(ctx, book, ch, remainingIdx)
			})
		}
		return len(touched), requeue, nil
	}
	// A full, uninterrupted run of all three passes over the whole chapter
	// just finished for cloneModel (possibly across several paused/resumed
	// continuations - see onlyIdx's own doc comment) - advance its
	// pipeline progress persistently (store.Passes), the same "stored
	// fact, not derived from paragraph state" reasoning applies. No longer
	// keyed by cloneModel (see store.Passes' own doc comment for the
	// deliberate chapter-wide simplification that replaced the old
	// per-clone-model chapters.directed map). Best-effort: a failure here
	// shouldn't undo the tagging work already committed above, just log
	// rather than fail the call outright.
	if serr := s.Store.SetChapterDirected(ch.ID); serr != nil {
		log.Printf("directChapter: could not advance chapter %s passes: %v", ch.ID, serr)
	}
	return len(touched), nil, nil
}

// characterizeVoice asks the LLM (speakerattr.Client.CharacterizeVoice) to
// describe how char actually sounds, from a sample of *only their own*
// attributed dialogue plus narration that describes them (see
// store.QuotesForBooks/DescriptionsForBooks) gathered across every book in
// their series so far (see store.SeriesScope), and persists the result -
// both the casting instruction and its paired reference passage - to
// char.Summary/RefLine - the pure LLM+DB step behind a
// jobs.KindSpeakerCharacterization task (see jobs.Manager.RunCharacterization),
// shared by provisionCharacterVoice's lazy-provisioning flow and
// handleCharacterizeSpeaker's explicit "Regenerate" button.
//
// Always actually calls the LLM now, even when quotes and descriptions
// come back empty - speakerattr.Client.CharacterizeVoice's own doc comment
// covers why (the name alone is enough for its system prompt's "no signal
// at all" fallback rules), and this is the layer that used to short-
// circuit before ever reaching that call at all when both were empty,
// returning ("", nil) as a normal "nothing to characterize from yet"
// state - which, for a character whose paragraphs were all later
// reattributed/canonicalized elsewhere, was actually permanent: every
// future characterization pass hit this same early return again, silently
// forever, never persisting a Summary and never erroring either.
func (s *Server) characterizeVoice(ctx context.Context, book *store.Book, char store.Character) (string, error) {
	books, err := s.booksInScope(book)
	if err != nil {
		return "", fmt.Errorf("list scope books: %w", err)
	}
	bookIDs := make([]string, len(books))
	for i, b := range books {
		bookIDs[i] = b.ID
	}

	quotes, err := s.Store.QuotesForBooks(bookIDs, char.Name, maxCharacterizeQuotes)
	if err != nil {
		return "", fmt.Errorf("gather quotes: %w", err)
	}
	descriptions, err := s.Store.DescriptionsForBooks(bookIDs, char.Name, maxCharacterizeDescriptions)
	if err != nil {
		return "", fmt.Errorf("gather descriptions: %w", err)
	}
	speakerQuotes := make([]speakerattr.QuoteContext, len(quotes))
	for i, q := range quotes {
		speakerQuotes[i] = speakerattr.QuoteContext{Text: q.Text, Context: q.Context}
	}
	speakerDescriptions := make([]speakerattr.DescriptionContext, len(descriptions))
	for i, d := range descriptions {
		speakerDescriptions[i] = speakerattr.DescriptionContext{Text: d.Text}
	}

	instruct, refLine, err := s.Speaker.CharacterizeVoice(ctx, char.Name, char.IsRole, speakerQuotes, speakerDescriptions)
	if err != nil {
		return "", fmt.Errorf("characterize: %w", err)
	}
	if err := s.Store.SetCharacterSummary(char.ID, instruct, refLine); err != nil {
		return "", fmt.Errorf("save characterization: %w", err)
	}
	return instruct, nil
}

// lockCharacterProvision serializes provisionCharacterVoice per
// characterID (across every clone model, and every caller/goroutine that
// might race one - a lazy provision from jobs.Manager's own background
// goroutine, another lazy provision for a different clone model, or an
// explicit "Regenerate" click) via Server.provisionLocks, a keyed mutex.
// This is what actually prevents two concurrent callers from both
// deciding "no voice yet" and each creating their own duplicate preset -
// jobs.Manager.RunCharacterization's own per-characterID dedup, used
// inside provisionCharacterVoice below, only covers the LLM
// characterization step by itself; without this, two callers that joined
// (or separately ran) that step back to back would still both fall
// through to CreateVoicePreset afterward. Returns the unlock func; call
// it (typically via defer) once done.
func (s *Server) lockCharacterProvision(characterID string) func() {
	muAny, _ := s.provisionLocks.LoadOrStore(characterID, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// characterRefText picks the reference passage a character's auto-created
// voice preset should be rendered from: characterized.RefLine, written by
// the same speakerattr.Client.CharacterizeVoice call as its Summary so its
// pace/energy actually matches the voice Summary describes (see
// store.Character.RefLine), falling back to voices.DefaultRefText only as
// a defensive default - RefLine should never legitimately be empty
// whenever Summary isn't, since CharacterizeVoice always returns both
// together.
func characterRefText(characterized store.Character) string {
	if characterized.RefLine != "" {
		return characterized.RefLine
	}
	return voices.DefaultRefText
}

// normalizeCharacterVoiceVolume rescales presetID's just-rendered
// reference clip (voicerefs.NormalizeVolume) so its overall loudness
// matches book's own narrator voice - an auto-provisioned character's
// independent VoiceDesign render can otherwise land much louder or
// quieter than the book's voice, and that difference carries straight
// into every paragraph cloned from it. Best-effort and silent on failure,
// the same tolerance provisionCharacterVoiceAttempt/regenerateCharacterVoice
// already give the render itself - a reader still gets *a* voice for the
// character even if it can't be leveled against the narrator (e.g. the
// book's own voice hasn't rendered anything yet either).
//
// Calls voicerefs.EnsureFile directly rather than s.ensureVoiceRef -
// exactly like provisionCharacterVoiceAttempt/regenerateCharacterVoice's
// own render calls just above/below each call site, and for the same
// reason (see ensureVoiceRef's own doc comment): both callers can
// themselves already be running inside a dispatched poolDesign task, and
// s.ensureVoiceRef's RunVoiceProvision would deadlock nesting a second
// poolDesign task from inside the first.
func (s *Server) normalizeCharacterVoiceVolume(ctx context.Context, book *store.Book, presetID string) {
	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		log.Printf("httpapi: normalize voice volume for preset %s: resolve book voice: %v", presetID, err)
		return
	}
	if bookVoice.PresetID == "" {
		return // fully custom instruct, no rendered clip to normalize against
	}
	if _, err := voicerefs.EnsureFile(ctx, s.TTS, s.DataDir, bookVoice.PresetID, bookVoice.Instruct, bookVoice.Seed, bookVoice.RefText, bookVoice.SpeedMultiplier, bookVoice.CloneModel, bookVoice.DesignModel); err != nil {
		log.Printf("httpapi: normalize voice volume for preset %s: ensure book voice reference clip: %v", presetID, err)
		return
	}
	if err := voicerefs.NormalizeVolume(s.DataDir, presetID, bookVoice.PresetID); err != nil {
		log.Printf("httpapi: normalize voice volume for preset %s: %v", presetID, err)
	}
}

// provisionCharacterVoice matches jobs.CharacterVoiceProvisioner's fixed
// signature - see provisionCharacterVoiceAttempt's own doc comment for what
// it actually does. Always attempt 0 (no seed bump ever possible) because
// this exact interface value is what jobs.Manager.provisionMissingCharacterVoices
// calls directly, outside of any RunVoiceProvision/EnqueueVoiceProvision
// task (so there's no t.attempt to plumb through even if this path did
// retry) - and, separately, provisionCharacterVoiceAttempt's own reference-
// clip render errors are logged and swallowed rather than surfaced/retried
// on this path already (see its own doc comment), so attempt would be inert
// here regardless. A caller that *does* run inside a KindVoiceProvision
// task (characters.go's own handlers, pipeline.go) calls
// provisionCharacterVoiceAttempt directly instead, passing the task's real
// attempt through.
func (s *Server) provisionCharacterVoice(ctx context.Context, bookID string, char store.Character, cloneModel string) (string, error) {
	return s.provisionCharacterVoiceAttempt(ctx, bookID, char, cloneModel, 0)
}

// provisionCharacterVoiceAttempt ensures char has an assigned voice for
// cloneModel specifically (a voice preset clones through one specific
// model and isn't portable to another - see store.Character's doc
// comment), characterizing them first (characterizeVoice, above) if
// they aren't yet and then creating + assigning a freshly-cloned preset.
// attempt is the calling KindVoiceProvision task's own retry count (0 on
// the first try) - threaded through to renderWithSeedBump so a retry after
// a numerically-unstable VoiceDesign crash ("no finite logits") perturbs
// the seed instead of reproducing the exact same crash on every attempt;
// see renderWithSeedBump's own doc comment.
// Characterization itself is shared across every clone model, so it only
// runs once, ever, per character (when char.Summary is still empty) - a
// character already characterized under one model reuses that same
// instruct text when a voice is created for another, rather than
// re-describing them (and potentially drifting) on every new model
// encountered.
func (s *Server) provisionCharacterVoiceAttempt(ctx context.Context, bookID string, char store.Character, cloneModel string, attempt int) (string, error) {
	if presetID, err := s.Store.CharacterVoiceForModel(char.ID, cloneModel); err != nil {
		return "", err
	} else if presetID != "" {
		return presetID, nil // already provisioned - the common case after the first call
	}

	unlock := s.lockCharacterProvision(char.ID)
	defer unlock()

	// Re-check now that this call actually holds the lock: a concurrent
	// caller (any clone model) may have provisioned this exact
	// (character, cloneModel) pair between the check above and acquiring
	// the lock.
	if presetID, err := s.Store.CharacterVoiceForModel(char.ID, cloneModel); err != nil {
		return "", err
	} else if presetID != "" {
		return presetID, nil
	}

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return "", err
	}
	if book == nil {
		return "", fmt.Errorf("book %s not found", bookID)
	}

	characterized, err := s.ensureCharacterized(ctx, book, char)
	if err != nil {
		return "", err
	}
	if characterized.Summary == "" {
		return "", nil // defensive only now - see ensureCharacterized
	}
	if (book.InstructCharacterVoices() || book.InstructAllCharacterVoices()) && cloneModel == voices.InstructedCloneModel {
		// This book resolves this character straight to its own narrator
		// voice for this clone model, styled by their own characterization,
		// rather than a dedicated auto-provisioned preset (see
		// internal/narration.Resolver.ResolveCharacterVoice's own
		// InstructUnassigned/InstructAll branches) - characterizing them
		// (above) is still real, needed work (their own Summary/RefLine now
		// exist for the Speakers page and for the resolver to read),
		// there's just no preset left to create or assign here. "" is this
		// function's own established "no preset for this character/model"
		// return - see provisionMissingCharacterVoices' own matching check,
		// which skips calling this at all once Summary is already set, so
		// this branch only actually runs once per character, the same as
		// preset creation below would.
		return "", nil
	}

	preset, err := s.Store.CreateVoicePreset(char.Name, characterized.Summary, characterRefText(characterized), store.RandomSeed(), 1.0, cloneModel, voices.DefaultDesignModel)
	if err != nil {
		return "", fmt.Errorf("create voice preset: %w", err)
	}
	// Render its reference clip now so the preset is immediately usable;
	// if this fails (e.g. the worker is briefly down), it's still assigned
	// below and self-heals on first actual generation - see
	// jobs.Manager.generate's own voicerefs.EnsureFile call.
	if _, err := s.renderWithSeedBump(preset.ID, preset.Seed, attempt, func(effectiveSeed int) (string, error) {
		return voicerefs.EnsureFile(ctx, s.TTS, s.DataDir, preset.ID, preset.Instruct, effectiveSeed, preset.RefText, preset.SpeedMultiplier, preset.CloneModel, preset.DesignModel)
	}); err != nil {
		log.Printf("httpapi: render reference clip for character %q: %v", char.Name, err)
	} else {
		s.normalizeCharacterVoiceVolume(ctx, book, preset.ID)
	}
	if err := s.Store.SetCharacterVoice(char.ID, cloneModel, preset.ID); err != nil {
		return "", fmt.Errorf("assign auto-created voice: %w", err)
	}
	return preset.ID, nil
}

// characterizeCharacterForJob matches jobs.CharacterCharacterizer's
// signature, wired in as such from NewRouter so jobs.Manager
// (characterizationBlockerFor) can characterize a speaker lazily the
// moment a clone/design/provision task first discovers they aren't
// characterized yet, rather than only ever via an explicit "Characterize"/
// "Regenerate" click (handleCharacterizeSpeakers) or provisionCharacterVoice's
// own lazy-provisioning path (ensureCharacterized). Calls
// recharacterizeAndInvalidate/characterizeVoice directly, the same pair
// handleCharacterizeSpeakers itself calls - NOT ensureCharacterized, which
// goes through the blocking jobs.Manager.RunCharacterization task-queue
// call: this function runs *as* a KindSpeakerCharacterization task's own
// runLLM body (see characterizationBlockerFor), so routing back through
// RunCharacterization here would have that task block on itself, waiting
// for a second characterization of the same character to join and
// complete a task that can't finish until this call returns - a
// self-deadlock. invalidation is a harmless no-op the first time a
// character is ever characterized (recharacterizeAndInvalidate only finds
// existing voice presets to invalidate, and a never-characterized
// character has none yet - see provisionCharacterVoice, which only creates
// one after characterization succeeds).
func (s *Server) characterizeCharacterForJob(ctx context.Context, bookID string, char store.Character) error {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return err
	}
	if book == nil {
		return fmt.Errorf("book %s not found", bookID)
	}
	_, _, err = s.recharacterizeAndInvalidate(ctx, book, char.ID, func(ctx context.Context) error {
		_, err := s.characterizeVoice(ctx, book, char)
		return err
	})
	return err
}

// directChapterForJob matches jobs.ChapterDirector's signature, wired in as
// such from NewRouter so jobs.Manager (speechDirectionBlockerFor) can
// direction-tag a chapter lazily the moment a clone/design task first
// discovers it isn't tagged yet, rather than only ever via an explicit
// "Tag directions" click (handleTagDirections). onlyIdx is always nil - a
// fresh, full-chapter run, never itself a paused continuation - directChapter's
// own pause/resume handling (its own requeue closure, re-invoking
// EnqueueDirection directly) works identically regardless of who queued
// the task it's running as.
func (s *Server) directChapterForJob(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
	return s.directChapter(ctx, book, ch, nil)
}

// attributeChapterForJob matches jobs.ChapterAttributor's signature, wired
// in as such from NewRouter so jobs.Manager (attributionOrderDependency)
// can attribute an earlier, not-yet-attributed chapter lazily the moment a
// later chapter in the same book first discovers it needs to wait on it -
// see attributionOrderDependency's own doc comment for why chapter order
// matters here, rather than only ever via an explicit "Attribute speakers"
// click (handleAttributeSpeakers). onlyUnattributed is always false - a
// fresh, full-chapter run, never itself a paused continuation -
// attributeChapter's own pause/resume handling (its own requeue closure,
// re-invoking EnqueueAttribution directly) works identically regardless of
// who queued the task it's running as.
func (s *Server) attributeChapterForJob(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
	return s.attributeChapter(ctx, book, ch, false)
}

// ensureCharacterized returns char with its own Summary/RefLine populated,
// characterizing it first (via jobs.Manager.RunCharacterization, same as an
// explicit "Regenerate" click - see characterizeVoice) if Summary is still
// empty. Shared by provisionCharacterVoiceAttempt and regenerateCharacterVoice
// below, both of which need a character's instruct text (and its paired
// refLine - see store.Character.RefLine) before they can create or render a
// voice from it. characterizeVoice now always actually characterizes (name
// alone is enough - see its own doc comment), so a zero store.Character
// (empty Summary), nil return here is no longer the normal "no material
// yet" case it used to be - both callers below still check for it
// defensively, but it should only ever fire on a genuine, already-logged
// characterization failure now.
//
// Always re-checks char.ID against the store first, rather than trusting
// the caller-supplied char.Summary directly - every real caller reaches
// this from inside a dispatched KindVoiceProvision task's own runProvision
// closure (provisionCharacterVoiceAttempt, this function's only caller),
// which captured its own char snapshot back when handleGenerateCharacterVoice(s)/
// handleRegenerateCharacterVoices/provisionMissingCharacterVoices first
// enqueued it - not when it actually dispatched. jobs.Manager's own
// characterizationDependency already blocks a KindVoiceProvision task from
// dispatching at all until a character's characterization genuinely
// finishes (see that function's own doc comment), so by the time this
// runs, the store row is normally already up to date even when the
// snapshot handed in isn't. Trusting the stale snapshot instead would call
// RunCharacterization a second time here - fatally: that's a blocking call
// pushing a poolLLM task from *inside* an already-dispatched, in-flight
// poolGeneration task, which can never actually dispatch while this task
// holds poolGeneration active and un-drained (see taskqueue.Queue's own
// "one pool at a time" doc comment) - a real deadlock, the same class
// referenceClipDependency's own doc comment describes fixing for
// poolDesign, just reintroduced here via stale caller data bypassing that
// same dependency mechanism instead of a missing one.
func (s *Server) ensureCharacterized(ctx context.Context, book *store.Book, char store.Character) (store.Character, error) {
	if fresh, err := s.Store.GetCharacter(char.ID); err != nil {
		return store.Character{}, err
	} else if fresh != nil && fresh.Summary != "" {
		return *fresh, nil
	}
	if err := s.Jobs.RunCharacterization(ctx, book.ID, char.ID, char.Name, func(ctx context.Context) error {
		_, err := s.characterizeVoice(ctx, book, char)
		return err
	}); err != nil {
		return store.Character{}, err
	}
	fresh, err := s.Store.GetCharacter(char.ID)
	if err != nil {
		return store.Character{}, err
	}
	if fresh == nil {
		return store.Character{}, nil
	}
	return *fresh, nil
}

// regenerateCharacterVoice force-rerenders char's reference clip for
// cloneModel via voicerefs.Regenerate, creating a voice for them first (via
// provisionCharacterVoice's same characterize-then-create path) if they
// don't have one yet for this model. Unlike provisionCharacterVoice, this
// never takes the "already provisioned, return unchanged" fast path - it's
// SpeakersPage's "Regenerate all voices" button, for "give every character
// a fresh take on their existing voice" regardless of whether they already
// have a cached ref clip, not "make sure everyone has *a* voice." Never
// touches char.Summary/Instruct itself - see recharacterizeAndInvalidate
// for changing what a voice sounds like; this only asks for a new render
// of however it's already described.
func (s *Server) regenerateCharacterVoice(ctx context.Context, bookID string, char store.Character, cloneModel string, attempt int) (string, error) {
	unlock := s.lockCharacterProvision(char.ID)
	defer unlock()

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return "", err
	}
	if book == nil {
		return "", fmt.Errorf("book %s not found", bookID)
	}

	presetID, err := s.Store.CharacterVoiceForModel(char.ID, cloneModel)
	if err != nil {
		return "", err
	}

	var preset *store.VoicePreset
	if presetID == "" {
		characterized, err := s.ensureCharacterized(ctx, book, char)
		if err != nil {
			return "", err
		}
		if characterized.Summary == "" {
			return "", nil // defensive only now - see ensureCharacterized
		}
		created, err := s.Store.CreateVoicePreset(char.Name, characterized.Summary, characterRefText(characterized), store.RandomSeed(), 1.0, cloneModel, voices.DefaultDesignModel)
		if err != nil {
			return "", fmt.Errorf("create voice preset: %w", err)
		}
		preset = &created
		if err := s.Store.SetCharacterVoice(char.ID, cloneModel, preset.ID); err != nil {
			return "", fmt.Errorf("assign auto-created voice: %w", err)
		}
	} else {
		preset, err = s.Store.GetVoicePreset(presetID)
		if err != nil {
			return "", err
		}
		if preset == nil {
			return "", fmt.Errorf("voice preset %s not found", presetID)
		}
	}

	if _, err := s.renderWithSeedBump(preset.ID, preset.Seed, attempt, func(effectiveSeed int) (string, error) {
		return voicerefs.Regenerate(ctx, s.TTS, s.DataDir, preset.ID, preset.Instruct, effectiveSeed, preset.RefText, preset.SpeedMultiplier, preset.CloneModel, preset.DesignModel)
	}); err != nil {
		return "", fmt.Errorf("regenerate reference clip: %w", err)
	}
	s.normalizeCharacterVoiceVolume(ctx, book, preset.ID)
	return preset.ID, nil
}

// handleGenerateCharacterVoice force-provisions a voice for one character
// right now, rather than waiting for the lazy path
// (jobs.Manager.provisionMissingCharacterVoices) to trigger it the next
// time their dialogue is actually generated - the Speakers page's own
// "Generate voice" button, for previewing/setting up a character's voice
// ahead of time. Idempotent: if they already have one for this book's
// clone model, provisionCharacterVoice's own fast path just returns it
// unchanged. 409 if there still isn't enough dialogue to characterize
// them from yet (provisionCharacterVoice returns "", nil in that case,
// same as the lazy path silently no-ops on).
func (s *Server) handleGenerateCharacterVoice(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	characterID := r.PathValue("characterId")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	char, err := s.Store.GetCharacter(characterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}

	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)

	// Routed through jobs.Manager.RunVoiceProvision (a KindVoiceProvision
	// task), not called directly - this used to bypass the job queue
	// entirely, so an explicit "Generate voice" click competed for the
	// ttsworker process uncoordinated with (and outside the concurrency
	// limit protecting) actual paragraph generation and every other kind of
	// voice work, same class of bug handleTestVoiceDesign's own doc comment
	// describes fixing for the "Test" button. Same "generate" mode/dedup
	// key handleGenerateCharacterVoices (the batch button) and the lazy
	// path (provisionMissingCharacterVoices) already use, so all three
	// correctly join into one queued provision instead of racing.
	presetID, err := s.Jobs.RunVoiceProvision(r.Context(), book.ID, char.ID, "generate", char.Name, func(ctx context.Context, attempt int) (string, error) {
		return s.provisionCharacterVoiceAttempt(ctx, book.ID, *char, cloneModel, attempt)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not generate voice: "+err.Error())
		return
	}
	if presetID == "" {
		writeError(w, http.StatusConflict, "not enough dialogue attributed to this character yet to characterize a voice")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"voicePresetId": presetID, "audioUrl": presetAudioURL(presetID)})
}

// handleGenerateCharacterVoices batch-provisions a voice for every given
// character in one request - SpeakersPage's "Generate all voices",
// fire-and-forget (202 Accepted, no result body), the same rationale as
// handleCharacterizeSpeakers: one request that survives the page closing,
// not N blocking requests capped by the browser's own per-origin
// connection limit.
//
// Each character's provisionCharacterVoice call runs as its own
// jobs.Manager KindVoiceProvision task (see EnqueueVoiceProvision) rather
// than a bare detached goroutine (an earlier version of this handler used
// one): a goroutine calling straight into the TTS worker bypasses
// poolGeneration's own concurrency limit and, more importantly, the
// worker's poolGeneration/poolLLM mutual exclusion (see worker's own
// fill) that keeps this app's two GPU-bound subsystems - the TTS worker
// and the in-process speakerattr LLM - from running at the same time and
// competing for VRAM. EnqueueVoiceProvision's own doc comment covers why
// this is poolGeneration rather than poolLLM despite
// provisionCharacterVoice itself enqueuing a KindSpeakerCharacterization
// (poolLLM) task internally for an uncharacterized character. Already-
// provisioned characters no-op harmlessly, same as the single-character
// button - see provisionCharacterVoice's own doc comment; a character with
// no dialogue/description attributed yet now still gets characterized
// (from its name alone) and provisioned like any other, rather than
// silently no-opping. Unknown/already-deleted
// character ids are skipped, not an error, same reasoning as
// handleCharacterizeSpeakers.
func (s *Server) handleGenerateCharacterVoices(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)

	var body struct {
		CharacterIDs []string `json:"characterIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	queued := 0
	for _, characterID := range body.CharacterIDs {
		found, err := s.Store.GetCharacter(characterID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if found == nil {
			continue
		}
		char := *found
		s.Jobs.EnqueueVoiceProvision(book.ID, char.ID, "generate", char.Name, func(ctx context.Context, attempt int) (string, error) {
			return s.provisionCharacterVoiceAttempt(ctx, book.ID, char, cloneModel, attempt)
		})
		queued++
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued})
}

// handleRegenerateCharacterVoices batch-forces a fresh reference-clip
// render for every given character in one request - SpeakersPage's
// "Regenerate all voices", distinct from handleGenerateCharacterVoices:
// that one only provisions characters with no voice yet (idempotent,
// no-ops on an already-provisioned one), this one re-renders regardless of
// whether a cached ref already exists - see regenerateCharacterVoice. Same
// EnqueueVoiceProvision/KindVoiceProvision shape and rationale as
// handleGenerateCharacterVoices (its own doc comment covers the
// poolGeneration-not-poolLLM reasoning, which applies identically here -
// regenerateCharacterVoice can also enqueue a KindSpeakerCharacterization
// task internally, for a character with no voice yet at all).
func (s *Server) handleRegenerateCharacterVoices(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)

	var body struct {
		CharacterIDs []string `json:"characterIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	queued := 0
	for _, characterID := range body.CharacterIDs {
		found, err := s.Store.GetCharacter(characterID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if found == nil {
			continue
		}
		char := *found
		s.Jobs.EnqueueVoiceProvision(book.ID, char.ID, "regenerate", char.Name, func(ctx context.Context, attempt int) (string, error) {
			return s.regenerateCharacterVoice(ctx, book.ID, char, cloneModel, attempt)
		})
		queued++
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued})
}

// handleAttributeSpeakers enqueues speaker attribution for one chapter,
// explicitly triggered by the reader (not automatic) - see
// backend/CLAUDE.md's "Speaker attribution" section. Fire-and-forget
// (202 Accepted, no result body): attribution runs the LLM over a whole
// chapter in batches and can take minutes, and SpeakersPage's "Attribute
// all" fires one of these per unattributed chapter in a book at once, so
// blocking this request on the actual run (as an earlier version did)
// meant holding up to one HTTP connection open per chapter in the book for
// as long as the whole book took to attribute - see
// jobs.Manager.EnqueueAttribution's own doc comment for the full story.
// Progress is observable via GET /api/jobs and by refetching this book's
// speaker table once a chapter's task clears.
func (s *Server) handleAttributeSpeakers(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker attribution is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}

	s.Jobs.EnqueueAttribution(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
		return s.attributeChapter(ctx, book, ch, false)
	})
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

// handleTagDirections enqueues speech-direction tagging for one chapter,
// explicitly triggered by the reader - see directChapter's own doc comment
// for why this isn't chained after attribution automatically the way
// describeChapter is. Fire-and-forget (202 Accepted, no result body),
// mirroring handleAttributeSpeakers exactly: tagging runs the LLM over a
// whole chapter in batches and can take a while, so this shouldn't hold a
// request open for it - see jobs.Manager.EnqueueAttribution's own doc
// comment for the fuller story behind that shape. Progress is observable
// via GET /api/jobs.
func (s *Server) handleTagDirections(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speech direction tagging is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	// Checked synchronously, before enqueueing, so a reader gets an
	// immediate, clear rejection instead of a 202 for a task that silently
	// tags nothing - see directChapter's own doc comment for why this
	// feature only applies to Higgs's own clone model.
	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if narration.EffectiveCloneModel(bookVoice) != voices.DefaultCloneModel {
		writeError(w, http.StatusBadRequest, "speech direction tags are only supported for the "+voices.DefaultCloneModel+" clone model")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}

	s.Jobs.EnqueueDirection(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
		return s.directChapter(ctx, book, ch, nil)
	})
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

// handleRetagDescriptions force re-runs description-tagging for one
// chapter - the Speakers page's own explicit "Retag descriptions" button,
// distinct from attributeChapter's own automatic call to describeChapter
// (which only ever runs as a side effect of a full, uninterrupted
// attribution pass - see its own doc comment). Always re-runs, even if the
// chapter was already described, the same "redo this with the latest
// prompt/model" idempotent-refresh rationale handleAttributeSpeakers'
// "Attribute all" already has. Unlike attributeChapter's automatic call,
// this blocks on the actual run and surfaces a real failure to the reader
// - describeChapter here is a single chapter's worth of small batches (see
// describeBatchParagraphs), fast enough that holding the request open for
// it is fine, the same reasoning handleCharacterizeSpeaker's own blocking
// single-character "Regenerate" button already relies on (also called
// directly, not through jobs.Manager - speakerattr.Client's own internal
// mutex already serializes this against any concurrently-running
// attribution/characterization call, so a reader clicking this while a
// background attribution run is mid-chapter just waits its turn rather
// than racing it).
func (s *Server) handleRetagDescriptions(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}

	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.describeChapter(r.Context(), book, ch, all); err != nil {
		writeError(w, http.StatusBadGateway, "description tagging failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRetagScareQuotes force re-runs scare-quote tagging for one chapter -
// handleRetagDescriptions' own exact counterpart (see its doc comment for
// the shared blocking-is-fine reasoning), distinct from attributeChapter's
// automatic call to scareQuoteChapter. Always re-runs, even if the chapter
// was already tagged, the same idempotent-refresh rationale every other
// explicit "redo this" button here already has.
func (s *Server) handleRetagScareQuotes(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}

	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.scareQuoteChapter(r.Context(), book, ch, all); err != nil {
		writeError(w, http.StatusBadGateway, "scare-quote tagging failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCharacterizeSpeaker force re-runs characterization for one
// character - the Speakers page's own (and the reader-view annotation
// legend's own) explicit "Regenerate" button, distinct from
// provisionCharacterVoice's own lazy, characterize-once-ever behavior.
// Unlike that path, this always re-runs even if char.Summary is already
// set (a deliberate "redo this with a fuller sample now that there's more
// dialogue" ask - see backend/CLAUDE.md's "Speaker attribution" section).
//
// Dispatched through jobs.Manager.RunCharacterization - same
// KindSpeakerCharacterization task shape (and same TierUrgent/poolLLM
// admission, and dedup-by-characterID against an already-in-flight run)
// its own batch sibling handleCharacterizeSpeakers and the lazy path
// (ensureCharacterized) both already use - rather than calling
// characterizeVoice directly against the request's own context, which an
// earlier version of this handler did: that bypassed the job queue
// entirely, so a manual "Regenerate" click showed up nowhere on the Jobs
// dashboard, couldn't join (or be joined by) a concurrent run for the same
// character, and - the sharper bug - never respected poolGeneration/
// poolLLM's mutual exclusion (see internal/jobs' own "Layout" section in
// backend/CLAUDE.md), so it could run the speakerattr LLM at the exact
// same time as ordinary TTS generation, the two GPU-bound subsystems this
// app is otherwise careful to never let contend for VRAM at once.
// RunCharacterization still blocks this request until the task finishes
// (a single character's own characterization is one short LLM call, the
// same "fine to hold the connection open" reasoning EnqueueAttribution's
// own doc comment gives for why *that* one doesn't), so the response body
// (summary/voiceInvalidated) is unchanged.
//
// See recharacterizeAndInvalidate for what happens after a successful
// re-characterization (every clone model's assigned voice preset for the
// character gets invalidated, not just rebuilt eagerly).
func (s *Server) handleCharacterizeSpeaker(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")
	characterID := r.PathValue("characterId")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	char, err := s.Store.GetCharacter(characterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}

	var summary string
	var invalidated bool
	if err := s.Jobs.RunCharacterization(r.Context(), book.ID, char.ID, char.Name, func(ctx context.Context) error {
		var err error
		summary, invalidated, err = s.recharacterizeAndInvalidate(ctx, book, char.ID, func(ctx context.Context) error {
			_, err := s.characterizeVoice(ctx, book, *char)
			return err
		})
		return err
	}); err != nil {
		writeError(w, http.StatusBadGateway, "characterization failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"summary": summary, "voiceInvalidated": invalidated})
}

// recharacterizeAndInvalidate runs runFn (a CharacterizationFunc closing
// over the character being re-characterized - see characterizeVoice) and,
// if it produced a non-empty Summary, invalidates every clone model's
// assigned voice preset for that character (see handleCharacterizeSpeaker's
// original doc comment on why every clone model, not just one book's own
// resolved one - Summary is shared series/model-wide, see store.Character)
// so each is rebuilt fresh (voicerefs.EnsureFile) the next time it's
// actually needed - not re-rendered eagerly here. Each preset's own
// Instruct and RefText are both updated from the fresh characterization
// (fresh.Summary/characterRefText(*fresh)) - RefLine is Summary's own
// paired reference passage (see store.Character.RefLine), so a
// recharacterize that changes Summary needs the preset's RefText to catch
// up too, not just Instruct, or the rebuilt clip would keep reading the
// character's previous (or default) reference passage against their new
// casting instruction.
//
// Deliberately called *from inside* the CharacterizationFunc passed to
// jobs.RunCharacterization/EnqueueCharacterization, not after it returns:
// that closure runs against the worker's own long-lived context regardless
// of whether the triggering HTTP request is still around (see
// EnqueueCharacterization's own doc comment), but code living outside it -
// as this invalidation step originally did, directly in
// handleCharacterizeSpeaker - only ever ran if RunCharacterization returned
// normally to a still-listening caller. A request cancelled between
// characterizeVoice finishing and that step running used to leave a
// character with an up-to-date Summary but a stale cached voice preset
// until the next successful re-run; running it in here instead closes that
// gap for both the single-character and batch paths.
func (s *Server) recharacterizeAndInvalidate(ctx context.Context, book *store.Book, characterID string, runFn func(ctx context.Context) error) (summary string, invalidated bool, err error) {
	if err := runFn(ctx); err != nil {
		return "", false, err
	}

	fresh, err := s.Store.GetCharacter(characterID)
	if err != nil {
		return "", false, err
	}
	if fresh == nil {
		return "", false, fmt.Errorf("character %s no longer exists", characterID)
	}
	if fresh.Summary == "" {
		return "", false, nil
	}

	presetIDs, err := s.Store.VoicePresetIDsForCharacter(fresh.ID)
	if err != nil {
		return fresh.Summary, false, err
	}
	for _, presetID := range presetIDs {
		preset, err := s.Store.GetVoicePreset(presetID)
		if err != nil {
			return fresh.Summary, invalidated, err
		}
		// nil for a built-in preset (GetVoicePreset only knows custom,
		// DB-backed ones - see store.go's voice_presets table comment, and
		// a reader can manually assign a character a built-in via
		// handleSetCharacterVoice) - nothing to invalidate there.
		if preset == nil {
			continue
		}
		if err := s.Store.UpdateVoicePreset(preset.ID, preset.Name, fresh.Summary, characterRefText(*fresh), preset.Seed, preset.SpeedMultiplier, preset.CloneModel, preset.DesignModel); err != nil {
			return fresh.Summary, invalidated, err
		}
		if err := voicerefs.Delete(s.DataDir, preset.ID); err != nil {
			log.Printf("httpapi: delete cached reference clip for character %q's voice (clone model %q): %v", fresh.Name, preset.CloneModel, err)
			continue
		}
		invalidated = true
	}

	// A custom preset's own instruct just changed (invalidated), so every
	// paragraph already generated in this character's old voice - across
	// however many books in their series they've spoken in, not just this
	// one, since character voices are series-wide (see store.SeriesScope)
	// - no longer reflects who they actually sound like now. Best-effort:
	// logged, not surfaced as a request failure, since the characterization
	// itself (this function's actual job) already succeeded by this point;
	// worst case some stale audio lingers reachable until manually cleared,
	// same as before this existed.
	if invalidated {
		bookScope, serr := s.booksInScope(book)
		if serr != nil {
			log.Printf("httpapi: could not resolve series scope to invalidate %q's existing audio: %v", fresh.Name, serr)
			return fresh.Summary, invalidated, nil
		}
		bookIDs := make([]string, len(bookScope))
		for i, b := range bookScope {
			bookIDs[i] = b.ID
		}
		refs, derr := s.Store.DeleteParagraphAudioForSpeaker(bookIDs, fresh.Name)
		if derr != nil {
			log.Printf("httpapi: could not invalidate %q's existing audio: %v", fresh.Name, derr)
			return fresh.Summary, invalidated, nil
		}
		for _, ref := range refs {
			if rerr := os.Remove(s.paragraphAudioPath(ref.BookID, ref.ChapterID, ref.VoiceID, ref.Idx)); rerr != nil && !os.IsNotExist(rerr) {
				log.Printf("httpapi: remove stale audio file for %q: %v", fresh.Name, rerr)
			}
		}
	}

	// Every characterization run - not just one that actually invalidated
	// an existing preset - is followed by a KindVoiceProvision task in
	// "regenerate" mode (not "generate": that mode no-ops the instant a
	// preset already exists - see provisionCharacterVoice - which would
	// skip re-rendering the very clip this function just deleted above).
	// regenerateCharacterVoice covers both cases in one call: creates and
	// assigns a fresh preset for a character who's never had one, or
	// re-renders the existing one's now-stale reference clip - so this
	// eagerly pre-warms the character's voice instead of leaving it to be
	// lazily rendered inline the next time generation actually needs it
	// (voicerefs.EnsureFile's own self-heal, still there as a fallback if
	// this fails). taskBlockedBySpeakerCharacterization/kindPriority (see
	// internal/jobs) already make sure this waits behind - and loses
	// same-tier ties to - the characterization task it's queued from, so
	// it can't itself race ahead and provision from a stale Summary.
	// Resolved against this book's own clone model, same as the Speakers
	// page's own explicit "Generate/Regenerate voice" buttons - a
	// character voiced under another clone model too stays on its own
	// lazy self-heal path.
	if bookVoice, verr := s.Narration.BookVoice(book); verr != nil {
		log.Printf("httpapi: could not resolve book voice to provision %q: %v", fresh.Name, verr)
	} else {
		cloneModel := narration.EffectiveCloneModel(bookVoice)
		char := *fresh
		s.Jobs.EnqueueVoiceProvision(book.ID, char.ID, "regenerate", char.Name, func(ctx context.Context, attempt int) (string, error) {
			return s.regenerateCharacterVoice(ctx, book.ID, char, cloneModel, attempt)
		})
	}

	return fresh.Summary, invalidated, nil
}

// handleCharacterizeSpeakers batch-enqueues re-characterization for a list
// of characters in one request - SpeakersPage's "Recharacterize all",
// fire-and-forget (202 Accepted, no result body) via
// jobs.Manager.EnqueueCharacterization rather than one blocking
// POST .../characterize per character (see that method's own doc comment
// for why: the browser's per-origin connection cap plus a page reload
// mid-burst could otherwise silently drop whichever characters' requests
// hadn't gone out yet). Unknown/already-deleted character ids are skipped,
// not an error - a stale id in the client's own list (e.g. a character
// merged/deleted by another tab moments earlier) shouldn't fail the rest
// of the batch. Progress is observable the same way as any other
// characterization - GET /api/jobs, and refetching this book's speaker
// table once each one's task clears.
func (s *Server) handleCharacterizeSpeakers(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	var body struct {
		CharacterIDs []string `json:"characterIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	queued := 0
	for _, characterID := range body.CharacterIDs {
		found, err := s.Store.GetCharacter(characterID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if found == nil {
			continue
		}
		char := *found
		s.Jobs.EnqueueCharacterization(book.ID, char.ID, char.Name, func(ctx context.Context) error {
			_, _, err := s.recharacterizeAndInvalidate(ctx, book, char.ID, func(ctx context.Context) error {
				_, err := s.characterizeVoice(ctx, book, char)
				return err
			})
			return err
		})
		queued++
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued})
}
