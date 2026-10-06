package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// speakerDTO is one row of a book's speaker table (GET
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
type speakerDTO struct {
	ID            string `json:"id"` // "" for the Narrator row
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
	// Invalid mirrors store.Character.Invalid - this name has been ruled
	// out as a real speaker, and attribution will no longer assign it.
	Invalid bool `json:"invalid,omitempty"`
	// Role mirrors store.Character.IsRole - a generic role label ("Guard",
	// "Woman") that may cover several unnamed people, not one individual.
	Role bool `json:"role,omitempty"`
	// Aliases mirrors store.Character.Aliases - other names this speaker
	// goes by.
	Aliases []string `json:"aliases,omitempty"`
	// Pronouns mirrors store.Character.Pronouns - "he/him", "she/her",
	// "they/them", "it/its", or omitted when not established.
	Pronouns string `json:"pronouns,omitempty"`
	// Emotions is every emotion this speaker's dialogue in this book
	// actually uses (store.Paragraph.EffectiveEmotion), in internal/emotions
	// order, each with its line count and the state of the emotion-variant
	// reference clip those lines clone from - the voice they generate under
	// right now (their own preset, or the book's). Omitted when they have
	// no emotional lines, or their voice has no preset to vary.
	Emotions []speakerEmotionDTO `json:"emotions,omitempty"`
	// Variants is every numbered member of this role in this book
	// (store.Paragraph.SpeakerIndex - "Guard 1", "Guard 2"), in order - the
	// Speakers page lists each as a speaker of its own. Emotions above then
	// covers the role's unnumbered lines only. Omitted for a speaker that
	// isn't a role, or has no numbered lines, and when the book narrates in
	// one voice.
	Variants []speakerVariantDTO `json:"variants,omitempty"`
}

// speakerVariantDTO is one numbered member of a role - see
// speakerDTO.Variants. Every number reads the same way, whichever voice
// narration.MemberVoice gives it (number 1 shares the role's own; a later
// one has a sibling of it).
type speakerVariantDTO struct {
	Number         int `json:"number"`
	ParagraphCount int `json:"paragraphCount"`
	// RefAudioURL is the reference clip this number's lines clone from,
	// omitted until it's been rendered (it renders the first time one of
	// these lines generates) or when the voice has no clip at all.
	RefAudioURL string `json:"refAudioUrl,omitempty"`
	// Emotions is speakerDTO.Emotions for this number's lines alone.
	Emotions []speakerEmotionDTO `json:"emotions,omitempty"`
}

// speakerVariants builds a speakerDTO.Variants list from counts (speaker
// number -> line count) for the role char, whose own voice is resolved.
// emotionCounts is each number's emotional lines (number -> emotion id ->
// line count).
func (s *Server) speakerVariants(resolved narration.ResolvedVoice, char store.Character, counts map[int]int, emotionCounts map[int]map[string]int) []speakerVariantDTO {
	if !char.IsRole {
		return nil
	}
	var out []speakerVariantDTO
	for n := 1; n <= store.MaxSpeakerIndex; n++ {
		if counts[n] == 0 {
			continue
		}
		member := narration.MemberVoice(resolved, char, n)
		dto := speakerVariantDTO{Number: n, ParagraphCount: counts[n], Emotions: s.speakerEmotions(member.PresetID, emotionCounts[n])}
		if path := audiopath.VoicePresetRefFile(s.DataDir, member.PresetID); member.PresetID != "" && fileExists(path) {
			if member.PresetID == resolved.PresetID {
				dto.RefAudioURL = s.versionedPresetAudioURL(member.PresetID)
			} else {
				dto.RefAudioURL = withClipVersion("/api/voices/role-variants/"+member.PresetID+"/audio", path)
			}
		}
		out = append(out, dto)
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// speakerEmotionDTO is one emotion a speaker uses - see
// speakerDTO.Emotions.
type speakerEmotionDTO struct {
	Emotion string `json:"emotion"`
	Label   string `json:"label"`
	Count   int    `json:"count"`
	// Status is "ready" (variant rendered - AudioURL plays it),
	// "pending" (not rendered yet: it renders lazily the first time one of
	// these lines generates), or "failed" (couldn't be rendered; these
	// lines clone from the base clip instead).
	Status   string `json:"status" tstype:"'ready' | 'pending' | 'failed'"`
	AudioURL string `json:"audioUrl,omitempty"`
}

// speakerEmotions builds a speakerDTO.Emotions list from counts
// (emotion id -> line count) for lines generating under presetID.
func (s *Server) speakerEmotions(presetID string, counts map[string]int) []speakerEmotionDTO {
	if presetID == "" || len(counts) == 0 {
		return nil
	}
	var out []speakerEmotionDTO
	for _, e := range emotions.All {
		n := counts[e.ID]
		if n == 0 {
			continue
		}
		dto := speakerEmotionDTO{Emotion: e.ID, Label: e.Label, Count: n, Status: "pending"}
		switch {
		case voicerefs.VariantExists(s.DataDir, presetID, e.ID):
			dto.Status = "ready"
			dto.AudioURL = withClipVersion("/api/voices/variants/"+presetID+"/"+e.ID+"/audio", audiopath.VoicePresetVariantFile(s.DataDir, presetID, e.ID))
		case voicerefs.VariantFailed(s.DataDir, presetID, e.ID):
			dto.Status = "failed"
		}
		out = append(out, dto)
	}
	return out
}

// withClipVersion tags a voice-clip URL with path's modification time, so a
// clip re-rendered in place (voicerefs.Regenerate keeps the preset id, and
// so the URL) gets a new URL and a player already holding the old one
// reloads - the speakers topic rebuilds when the render's job finishes.
// url unchanged when path doesn't exist yet.
func withClipVersion(url, path string) string {
	if url == "" {
		return url
	}
	info, err := os.Stat(path)
	if err != nil {
		return url
	}
	return url + "?v=" + strconv.FormatInt(info.ModTime().UnixNano(), 36)
}

// presetAudioURL returns presetID's own reference-clip endpoint - the
// built-in path (internal/voices) or the custom one (store.VoicePreset),
// whichever namespace presetID actually belongs to. "" for "" (no preset
// resolved at all).
// versionedPresetAudioURL is presetAudioURL tagged with the clip's current
// version - see withClipVersion.
func (s *Server) versionedPresetAudioURL(presetID string) string {
	return withClipVersion(presetAudioURL(presetID), audiopath.VoicePresetRefFile(s.DataDir, presetID))
}

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
	v, err := s.buildSpeakers(r.PathValue("id"))
	writeBuilt(w, v, err)
}

func (s *Server) buildSpeakers(bookID string) ([]speakerDTO, error) {
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
	// Emotional lines per speaker and speaker number (0 for a line with
	// none): a role's numbered lines clone from their own voice's variants.
	emotionsBySpeaker := map[string]map[int]map[string]int{}
	numbersBySpeaker := map[string]map[int]int{}
	for _, p := range paragraphs {
		name := p.Speaker
		if name == "" {
			name = "Narrator"
		}
		idsBySpeaker[name] = append(idsBySpeaker[name], p.ID)
		if p.SpeakerIndex > 0 {
			if numbersBySpeaker[name] == nil {
				numbersBySpeaker[name] = map[int]int{}
			}
			numbersBySpeaker[name][p.SpeakerIndex]++
		}
		if e := p.EffectiveEmotion(); e != emotions.Neutral {
			if emotionsBySpeaker[name] == nil {
				emotionsBySpeaker[name] = map[int]map[string]int{}
			}
			if emotionsBySpeaker[name][p.SpeakerIndex] == nil {
				emotionsBySpeaker[name][p.SpeakerIndex] = map[string]int{}
			}
			emotionsBySpeaker[name][p.SpeakerIndex][e]++
		}
	}

	readyCounts, err := s.Store.ReadyAudioCountsBySpeaker(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
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

	rows := make([]speakerDTO, 0, len(order))
	for _, name := range order {
		ids := idsBySpeaker[name]
		row := speakerDTO{Name: name, ParagraphCount: len(ids)}
		if c, ok := charByName[name]; ok {
			row.ID = c.ID
			row.VoicePresetID = presetIDByChar[c.ID]
			row.Summary = c.Summary
			row.RefLine = c.RefLine
			row.Invalid = c.Invalid
			row.Role = c.IsRole
			row.Aliases = c.Aliases
			row.Pronouns = c.Pronouns
		}
		// The voice this speaker's dialogue actually generates/plays under
		// right now - their own assigned preset if they have one for this
		// book's clone model, otherwise the book's own (see resolveFor).
		// Only used below to count ready/generated audio, which really did
		// render under this voice regardless of whether it's "theirs" -
		// RefAudioURL (this speaker's *own* voice preview) is deliberately
		// NOT set from this resolution; see below.
		resolved := resolveFor(name)
		row.ReadyCount = readyCounts[name][resolved.VoiceID()]
		// A role's numbered lines render under their own voices
		// (narration.MemberVoice).
		if c, ok := charByName[name]; ok && book.MultiVoice() {
			for n := 2; n <= store.MaxSpeakerIndex; n++ {
				if member := narration.MemberVoice(resolved, c, n); member.PresetID != resolved.PresetID {
					row.ReadyCount += readyCounts[name][member.VoiceID()]
				}
			}
			row.Variants = s.speakerVariants(resolved, c, numbersBySpeaker[name], emotionsBySpeaker[name])
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
			row.RefAudioURL = s.versionedPresetAudioURL(bookVoice.PresetID)
		} else if row.VoicePresetID != "" {
			row.RefAudioURL = s.versionedPresetAudioURL(row.VoicePresetID)
		}
		// Unlike RefAudioURL, emotion variants follow `resolved`: they're
		// cloned from whichever preset these lines actually generate under.
		// A role's numbered members list their own (speakerVariants).
		ownEmotions := map[string]int{}
		for n, counts := range emotionsBySpeaker[name] {
			if n > 0 && len(row.Variants) > 0 {
				continue
			}
			for e, k := range counts {
				ownEmotions[e] += k
			}
		}
		row.Emotions = s.speakerEmotions(resolved.PresetID, ownEmotions)
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

	writeNoContent(w)
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
// for actual narration. Paragraphs tagged as describing them lose that
// tag. Scoped to this book only; their identity/Summary,
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

	writeNoContent(w)
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
	// TargetName is who every one of the series' paragraphs currently
	// attributed to this character should be reattributed to instead -
	// "Narrator" (a sentinel, not a real character row) or another
	// character's exact name.
	TargetName string `json:"targetName"`
	// AsRole makes the target a generic role label (store.Character.
	// IsRole, created as one or turned into one) and skips adding the
	// merged-away name as its alias - for folding a descriptor-named
	// "character" ("the older man") into its kind ("Man"), where the
	// description is no name the target goes by.
	AsRole bool `json:"asRole,omitempty"`
}

// handleMergeCharacter folds one character into another: every paragraph
// in their series currently attributed to them is reattributed to
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
	if speakerattr.IsGroupSpeaker(targetName) {
		writeError(w, http.StatusBadRequest, "cannot merge into a group - pick one character")
		return
	}

	storeTargetName := targetName
	if targetName == "Narrator" {
		storeTargetName = ""
	} else if targetName != "Unknown" {
		target, _, err := s.Store.UpsertCharacter(store.SeriesScope(book), targetName, req.AsRole)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if target.ID == char.ID {
			// The lookup ignores case, so "woman" -> "Woman" with no
			// separate "Woman" finds the character itself.
			writeError(w, http.StatusBadRequest, "that's this character's own name (names are matched ignoring case)")
			return
		}
		storeTargetName = target.Name
		if req.AsRole && !target.IsRole {
			if err := s.Store.SetCharacterRole(target.ID, true); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		// The merged-away name (and anything it was already called) is
		// another name for the target from now on - so attribution folds
		// it back in, reads "Albert said" as the target, and the same
		// split doesn't come back. Not for a junk name ("Fritz and
		// Bert", "No one"), which was never anyone's name.
		if clean, _ := speakerattr.SanitizeSpeaker(char.Name); clean == char.Name && !req.AsRole {
			if err := s.Store.AddCharacterAliases(target.ID, append([]string{char.Name}, char.Aliases...)...); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}

	// Every book of the series, not only this one: the identity below is
	// deleted series-wide, and lines left under the old name in the other
	// books would belong to nobody (Jax speaks in two Underverse books).
	books, err := s.booksInScope(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if storeTargetName == "" || storeTargetName == "Unknown" {
		for _, b := range books {
			if err := s.Store.ReassignCharacterSpeaker(b.ID, char.Name, storeTargetName); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	} else if err := s.renameSpeakerEverywhere(books, char.Name, storeTargetName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.deleteCharacterIdentity(r, *char); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeNoContent(w)
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
// task per chapter, all grouped under one pipeline_auto_split task - see
// jobs.Manager.EnqueueBulk/RunReattribution) and
// redistributes them to whichever real character/Narrator/Unknown they
// actually belong to, using the exact same chapter-wide LLM pass
// attribution itself already runs (reattributeChapterSpeaker reuses
// speakerattr.Client.AttributeChapter directly - no second prompt), just
// with req.Name withheld from "Known characters" and never accepted back
// as a result for one of its own paragraphs (see reattributeChapterSpeaker's
// own doc comment).
//
// A real character (not Unknown) is marked invalid (store.Character.
// Invalid) before anything is queued, so the name isn't recreated or
// reassigned by this or any later attribution pass.
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
		writeError(w, http.StatusServiceUnavailable, "speaker reattribution is not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
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
		// Auto Split is a reader's judgment that this name isn't a real,
		// distinct speaker - mark it invalid up front, before any chapter
		// is reattributed, so neither this run nor any concurrent/later
		// attribution pass hands its lines straight back to it (see
		// store.Character.Invalid). Unknown has no row to flag, and is a
		// legitimate result regardless.
		if !char.Invalid {
			if err := s.Store.SetCharacterInvalid(char.ID, true); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
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

	type chapterTarget struct {
		ch        *store.Chapter
		targetIdx map[int]bool
	}
	var targets []chapterTarget
	for chapterID, targetIdx := range targetByChapter {
		ch, err := s.Store.GetChapterByID(chapterID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ch == nil {
			continue
		}
		targets = append(targets, chapterTarget{ch: ch, targetIdx: targetIdx})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ch.Idx < targets[j].ch.Idx })

	// One pipeline_auto_split task wraps every chapter's own
	// KindSpeakerReattribution task, so the whole click shows up (and can
	// be canceled/promoted) as a single Jobs-dashboard row - see
	// jobs.BulkGroup.
	if len(targets) > 0 {
		s.Jobs.EnqueueBulk(book.ID, jobs.BulkGroup{
			Kind:     "pipeline_auto_split",
			Key:      "auto_split:" + speakerKey,
			Label:    name,
			Tier:     jobs.TierBackground,
			Children: &jobs.ChildFilter{Kinds: []jobs.Kind{jobs.KindSpeakerReattribution}, Label: name},
		}, func(ctx context.Context, tier func() int) error {
			var wg sync.WaitGroup
			for _, t := range targets {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := s.Jobs.RunReattribution(ctx, book.ID, t.ch.ID, t.ch.Idx, tier(), speakerKey, name, func(ctx context.Context) (int, func(), error) {
						return s.reattributeChapterSpeaker(ctx, book, t.ch, speakerKey, name, t.targetIdx)
					}); err != nil {
						log.Printf("httpapi: auto split %q in book %s: chapter %d: %v", name, book.ID, t.ch.Idx, err)
					}
				}()
			}
			wg.Wait()
			return nil
		})
	}

	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: len(targets)})
}

type setCharacterAliasesRequest struct {
	Aliases []string `json:"aliases"`
}

// renameCharacterRequest is PUT .../characters/{characterId}/name's body.
type renameCharacterRequest struct {
	Name string `json:"name"`
}

// handleRenameCharacter renames one character series-wide
// (renameCharacter: their lines, description tags, speaker corrections,
// summary notes, the setting's narrator, auto-named voice presets), keeping
// their voice, casting and audio. The old name becomes an alias, so
// attribution folds it back in if the text or the model still uses it -
// unless it names no single person ("Protagonist", "the other noble,
// Charles"). 400 for a name that isn't one person's (a group, a
// non-answer, Narrator/Unknown); 409 when another character in the series
// already has it as a name or alias (merge them instead).
func (s *Server) handleRenameCharacter(w http.ResponseWriter, r *http.Request) {
	var req renameCharacterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.Join(strings.Fields(req.Name), " ")
	book, err := s.Store.GetBook(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	char, err := s.Store.GetCharacter(r.PathValue("characterId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil || char.Scope != store.SeriesScope(book) {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}
	switch clean, _ := speakerattr.SanitizeSpeaker(name); {
	case name == "" || len(name) > 100:
		writeError(w, http.StatusBadRequest, "a name is 1-100 characters")
		return
	case strings.EqualFold(name, "Narrator") || strings.EqualFold(name, "Unknown"):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is reserved", name))
		return
	case clean != name || speakerattr.IsGroupSpeaker(name):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%q isn't one person's name", name))
		return
	case name == char.Name:
		writeNoContent(w)
		return
	}
	roster, err := s.Store.ListCharacters(char.Scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, c := range roster {
		if c.ID == char.ID {
			continue
		}
		if strings.EqualFold(c.Name, name) {
			if c.Invalid {
				writeError(w, http.StatusConflict, fmt.Sprintf("%q is a character marked as not a speaker - delete it first", c.Name))
			} else {
				writeError(w, http.StatusConflict, fmt.Sprintf("%q is already a character - merge the two instead", c.Name))
			}
			return
		}
		if !c.Invalid && slices.ContainsFunc(c.Aliases, func(a string) bool { return strings.EqualFold(a, name) }) {
			writeError(w, http.StatusConflict, fmt.Sprintf("%q is another name for %s - merge the two instead", name, c.Name))
			return
		}
	}
	if err := s.renameCharacter(book, *char, name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if clean, _ := speakerattr.SanitizeSpeaker(char.Name); clean == char.Name && !speakerattr.IsFirstPersonLabel(char.Name) {
		if err := s.Store.AddCharacterAliases(char.ID, char.Name); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else if err := s.Store.AddCharacterAliases(char.ID); err != nil { // drops the new name if it was an alias
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// handleSetCharacterAliases replaces one character's aliases (store.
// Character.Aliases) - other names they go by, which attribution folds
// back into their name and reads in speech tags ("Albert said"). 400 for
// an alias that names no single person (a group like "Bert and Sid", a
// non-answer like "No one"); 409 for one that's already another valid
// character's name or alias in this series (merge them instead).
func (s *Server) handleSetCharacterAliases(w http.ResponseWriter, r *http.Request) {
	var req setCharacterAliasesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	char, err := s.Store.GetCharacter(r.PathValue("characterId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}
	roster, err := s.Store.ListCharacters(char.Scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	taken := map[string]string{} // lower-cased name/alias -> owning character's name
	for _, c := range roster {
		if c.ID == char.ID || c.Invalid {
			continue
		}
		taken[strings.ToLower(c.Name)] = c.Name
		for _, a := range c.Aliases {
			taken[strings.ToLower(a)] = c.Name
		}
	}
	for _, a := range req.Aliases {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if clean, _ := speakerattr.SanitizeSpeaker(a); clean != a {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%q isn't one person's name", a))
			return
		}
		if owner, ok := taken[strings.ToLower(a)]; ok {
			writeError(w, http.StatusConflict, fmt.Sprintf("%q already belongs to %s - merge the two characters instead", a, owner))
			return
		}
	}
	if err := s.Store.SetCharacterAliases(char.ID, req.Aliases); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated, err := s.Store.GetCharacter(char.ID)
	if err != nil || updated == nil {
		writeError(w, http.StatusInternalServerError, "aliases saved but could not be reloaded")
		return
	}
	writeJSON(w, http.StatusOK, aliasesResponse{Aliases: updated.Aliases})
}

type setCharacterPronounsRequest struct {
	// Pronouns is "he/him", "she/her", "they/them", "it/its" (or a
	// recognizable variant, see speakerattr.NormalizePronouns), or "" to
	// clear them.
	Pronouns string `json:"pronouns"`
}

// handleSetCharacterPronouns sets one character's pronouns (store.
// Character.Pronouns), which attribution shows in "Known characters" and
// characterization takes as its gender signal. Doesn't re-characterize
// on its own - the Speakers page's "Regenerate" does that. 400 for a
// value that isn't one of the known sets.
func (s *Server) handleSetCharacterPronouns(w http.ResponseWriter, r *http.Request) {
	var req setCharacterPronounsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	pronouns := speakerattr.NormalizePronouns(req.Pronouns)
	if pronouns == "" && strings.TrimSpace(req.Pronouns) != "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unrecognized pronouns %q", req.Pronouns))
		return
	}
	id := r.PathValue("characterId")
	char, err := s.Store.GetCharacter(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}
	if err := s.Store.SetCharacterPronouns(id, pronouns); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pronounsResponse{Pronouns: pronouns})
}

type setCharacterRoleRequest struct {
	Role bool `json:"role"`
}

// handleSetCharacterRole marks (or unmarks) a character as a generic role
// label (store.Character.IsRole) - several unnamed people of one kind
// ("Guard", "Woman") rather than one individual. Attribution lists it as
// "[role]", characterization casts the type, and pronouns aren't tracked
// for it. Nothing else changes: its lines, voice and aliases stay.
func (s *Server) handleSetCharacterRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("characterId")
	var req setCharacterRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	char, err := s.Store.GetCharacter(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}
	if err := s.Store.SetCharacterRole(id, req.Role); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

type setCharacterInvalidRequest struct {
	Invalid bool `json:"invalid"`
}

// handleSetCharacterInvalid marks (or unmarks) a character as invalid -
// not a real speaker, so attribution stops creating/assigning the name
// (see store.Character.Invalid). Non-destructive: the paragraphs already
// attributed to them, their summary, and their voice assignments are all
// left alone, so unmarking restores them exactly - Auto Split
// (handleReattributeSpeaker, which also sets this flag) is what actually
// moves their existing lines elsewhere.
func (s *Server) handleSetCharacterInvalid(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("characterId")
	var req setCharacterInvalidRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	char, err := s.Store.GetCharacter(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}
	if err := s.Store.SetCharacterInvalid(id, req.Invalid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
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
	writeNoContent(w)
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
	v, err := s.buildCharacterAppearances(r.PathValue("id"), r.PathValue("characterId"))
	writeBuilt(w, v, err)
}

func (s *Server) buildCharacterAppearances(bookID, characterID string) ([]speakerAppearanceDTO, error) {

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
			if v, err := s.Narration.ForParagraph(b, char.Name, a.SpeakerIndex); err == nil {
				dto.AudioURL = s.resolveAppearanceAudioURL(a.ParagraphID, v.VoiceID())
			}
		}
		out[i] = dto
	}
	return out, nil
}

// resolveAppearanceAudioURL is handleCharacterAppearances' audio-preview
// lookup - unlike
// resolveAudioID (handleGetChapter's own, which has every paragraph in the
// chapter in hand to resolve a scare-quote merge group pointer against),
// this deliberately does NOT resolve a pointer paragraph (see
// store.AudioState.PointerOffset) to its anchor's own file: doing so here
// would cost a second chapter/paragraph lookup per appearance row for a
// case rare enough (a character's own dialogue that is also part of a
// scare-quote merge group) not to be worth it
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
	if state.ClipKey != "" {
		return "/api/clips/" + state.ClipKey
	}
	return "/api/paragraphs/" + paragraphID + "/audio"
}

// characterNameDTO is one entry of a book's series roster, names only - what
// a reassign picker needs, without buildSpeakers' per-speaker voice and audio
// resolution (seconds on a big book).
type characterNameDTO struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Invalid bool   `json:"invalid"`
}

func (s *Server) handleListCharacterNames(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildCharacterNames(r.PathValue("id"))
	writeBuilt(w, v, err)
}

// buildCharacterNames lists bookID's series roster (store.ListCharacters -
// cached), alphabetically (case-insensitive).
func (s *Server) buildCharacterNames(bookID string) ([]characterNameDTO, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}
	characters, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]characterNameDTO, len(characters))
	for i, c := range characters {
		out[i] = characterNameDTO{ID: c.ID, Name: c.Name, Invalid: c.Invalid}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}
