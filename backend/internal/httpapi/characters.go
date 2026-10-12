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
	"github.com/rhino1998/lectable/backend/internal/namekey"
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
	// Race is the kind of being this speaker is ("drake") and Nationality
	// where they are from or whose they are ("Pallassian") - an accepted
	// character note of each (store.NoteKind, store.NoteNationality), set
	// here by hand or accepted from fact finding. Their casting is shown
	// both, with the facts about them. Omitted when not established.
	Race        string `json:"race,omitempty"`
	Nationality string `json:"nationality,omitempty"`
	// Age is what the text states of their age, from the accepted
	// character notes (store.NoteAge): "12 as of Chapter 3", several
	// joined with "; " in the order found. Omitted when none is accepted.
	Age string `json:"age,omitempty"`
	// AgeYears is the last of those ages, in years - what the Speakers
	// page sorts by and edits. 0 when none is accepted.
	AgeYears int `json:"ageYears,omitempty"`
	// Voices is every voice this speaker's lines in this book are spoken
	// in, each with its samples: their own first, then each numbered member
	// of a role (store.Paragraph.SpeakerIndex - "Guard 1", "Guard 2"), then
	// each age (store.CharacterStage) in story order. A speaker with one
	// voice has one entry.
	Voices []speakerVoiceDTO `json:"voices"`
}

// speakerVoiceDTO is one of the voices a speaker's lines are spoken in
// (speakerDTO.Voices): the reference clip its lines clone from - the
// neutral sample - and its emotion variants. Every voice reads the same
// way, whichever it is: Number and Stage say which.
type speakerVoiceDTO struct {
	// Number is the role member this voice belongs to ("Guard 2"), 0 for
	// the speaker's unnumbered lines.
	Number int `json:"number,omitempty"`
	// Stage is the age this voice is the speaker's from (omitted for the
	// voice they have before any stage). One that starts in a later book
	// of the series is listed with no lines here.
	Stage *speakerStageDTO `json:"stage,omitempty"`
	// ParagraphCount is how many of the speaker's lines in this book are
	// spoken in this voice, NeutralCount how many of those carry no
	// emotion.
	ParagraphCount int `json:"paragraphCount"`
	NeutralCount   int `json:"neutralCount"`
	// RefAudioURL is the clip the voice's lines clone from, omitted until
	// it's been rendered (a numbered member's or a stage's renders the
	// first time one of its lines generates) and for a speaker with no
	// voice of their own - the Narrator, or a character nobody has voiced.
	RefAudioURL string `json:"refAudioUrl,omitempty"`
	// CanRegenerate says the neutral clip can be rendered afresh (POST
	// .../speakers/variants/regenerate with no emotion): the speaker's own
	// preset, or a stage's voice. A numbered member's is fixed by its
	// role's.
	CanRegenerate bool `json:"canRegenerate,omitempty"`
	// Emotions is every emotion this voice's lines use
	// (store.Paragraph.EffectiveEmotion), in internal/emotions order, each
	// with its line count and the state of the variant clip those lines
	// clone from. Omitted when it has no emotional lines, or no preset to
	// vary.
	Emotions []speakerEmotionDTO `json:"emotions,omitempty"`
}

// voiceSamples fills a voice's counts and emotion list from lines (how
// many are spoken in it) and counts (those carrying each emotion), for
// lines generating under presetID.
func (s *Server) voiceSamples(v *speakerVoiceDTO, presetID string, lines int, counts map[string]int) {
	v.ParagraphCount, v.NeutralCount = lines, lines
	for _, n := range counts {
		v.NeutralCount -= n
	}
	v.NeutralCount = max(v.NeutralCount, 0)
	v.Emotions = s.speakerEmotions(presetID, counts)
}

// numberedVoices lists the voice of each numbered member of the role char
// with lines in the book (speakerDTO.Voices), from counts (speaker number
// -> line count) and emotionCounts (number -> emotion id -> line count).
// resolved is the role's own voice: number 1 shares it, a later one has a
// sibling of it (narration.MemberVoice).
func (s *Server) numberedVoices(resolved narration.ResolvedVoice, char store.Character, counts map[int]int, emotionCounts map[int]map[string]int) []speakerVoiceDTO {
	if !char.IsRole {
		return nil
	}
	var out []speakerVoiceDTO
	for n := 1; n <= store.MaxSpeakerIndex; n++ {
		if counts[n] == 0 {
			continue
		}
		member := narration.MemberVoice(resolved, char, n)
		dto := speakerVoiceDTO{Number: n}
		s.voiceSamples(&dto, member.PresetID, counts[n], emotionCounts[n])
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
// speakerVoiceDTO.Emotions.
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

	// Every stage of the roster (store.CharacterStage), with the series'
	// books to say where each starts, and - for counting each stage's
	// lines apart from the speaker's own - where they fall in this book.
	stages, err := s.Store.ListCharacterStages(store.SeriesScope(book))
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	var scopeBooks []store.Book
	var bookStages narration.Stages
	var chapterIdx map[string]int
	if len(stages) > 0 {
		if scopeBooks, err = s.booksInScope(book); err != nil {
			return nil, httpError(http.StatusInternalServerError, err.Error())
		}
		if bookStages, err = s.Narration.BookStages(book); err != nil {
			return nil, httpError(http.StatusInternalServerError, err.Error())
		}
		if chapterIdx, err = s.Store.ChapterIdxByID(bookID); err != nil {
			return nil, httpError(http.StatusInternalServerError, err.Error())
		}
	}
	// Lines, and emotional lines by emotion, per stage id.
	stageLines := map[string]int{}
	stageEmotions := map[string]map[string]int{}

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
		// A line past one of its speaker's stages is voiced, and counted,
		// under that stage.
		if c, ok := charByName[name]; ok && len(bookStages[c.ID]) > 0 && !c.IsRole {
			if st := bookStages.At(c.ID, chapterIdx[p.ChapterID]); st != nil {
				stageLines[st.ID]++
				if e := p.EffectiveEmotion(); e != emotions.Neutral {
					if stageEmotions[st.ID] == nil {
						stageEmotions[st.ID] = map[string]int{}
					}
					stageEmotions[st.ID][e]++
				}
				continue
			}
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

	// people is each character's accepted race and nationality.
	people := map[string]map[store.CharacterNoteKind]string{}
	accepted, err := s.Store.ListCharacterNotes(store.SeriesScope(book))
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	// ages is each character's accepted ages, as "12 as of <chapter>".
	ages := map[string][]string{}
	ageYears := map[string]int{}
	for _, n := range accepted {
		if n.Status != store.ProposalAccepted {
			continue
		}
		if n.Kind == store.NoteAge {
			age := n.Value
			if ch, err := s.Store.GetChapterByID(n.ChapterID); err == nil && ch != nil && strings.TrimSpace(ch.Title) != "" {
				age += " as of " + strings.TrimSpace(ch.Title)
			}
			ages[n.CharacterID] = append(ages[n.CharacterID], age)
			if years, err := strconv.Atoi(n.Value); err == nil {
				ageYears[n.CharacterID] = years
			}
			continue
		}
		if people[n.CharacterID] == nil {
			people[n.CharacterID] = map[store.CharacterNoteKind]string{}
		}
		if people[n.CharacterID][n.Kind] == "" {
			people[n.CharacterID][n.Kind] = n.Value
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
			row.Race, row.Nationality = people[c.ID][store.NoteKind], people[c.ID][store.NoteNationality]
			row.Age, row.AgeYears = strings.Join(ages[c.ID], "; "), ageYears[c.ID]
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
		var numbered, staged []speakerVoiceDTO
		// A role's numbered lines render under their own voices
		// (narration.MemberVoice).
		if c, ok := charByName[name]; ok && book.MultiVoice() {
			for n := 2; n <= store.MaxSpeakerIndex; n++ {
				if member := narration.MemberVoice(resolved, c, n); member.PresetID != resolved.PresetID {
					row.ReadyCount += readyCounts[name][member.VoiceID()]
				}
			}
			numbered = s.numberedVoices(resolved, c, numbersBySpeaker[name], emotionsBySpeaker[name])
		}
		if c, ok := charByName[name]; ok && len(stages) > 0 {
			staged = s.stageVoices(book, scopeBooks, bookVoice, resolved, c, stages, stageLines, stageEmotions)
			// Lines past a stage render under its voice
			// (narration.StageVoice).
			counted := map[string]bool{resolved.VoiceID(): true}
			for _, st := range stages {
				if st.CharacterID != c.ID || !book.MultiVoice() {
					continue
				}
				if id := narration.StageVoice(resolved, bookVoice, c, &st).VoiceID(); !counted[id] {
					counted[id] = true
					row.ReadyCount += readyCounts[name][id]
				}
			}
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
		// Their own voice: the lines no numbered member or stage speaks.
		// Unlike RefAudioURL, emotion variants follow `resolved`: they're
		// cloned from whichever preset these lines actually generate under.
		own := speakerVoiceDTO{CanRegenerate: row.VoicePresetID != ""}
		if name != "Narrator" {
			own.RefAudioURL = row.RefAudioURL
		}
		ownLines := len(ids)
		ownEmotions := map[string]int{}
		for n, counts := range emotionsBySpeaker[name] {
			if n > 0 && len(numbered) > 0 {
				continue
			}
			for e, k := range counts {
				ownEmotions[e] += k
			}
		}
		for _, v := range numbered {
			ownLines -= v.ParagraphCount
		}
		for _, v := range staged {
			ownLines -= v.ParagraphCount
		}
		s.voiceSamples(&own, resolved.PresetID, max(ownLines, 0), ownEmotions)
		row.Voices = append(append([]speakerVoiceDTO{own}, numbered...), staged...)
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
	// NoAlias merges without making the merged-away name (or its own
	// aliases) another name for the target - for a name that was never
	// theirs (a mislabel, someone else's name on their lines), which
	// attribution would otherwise keep folding into them.
	NoAlias bool `json:"noAlias,omitempty"`
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
	// slot is the speaker number the merged lines take: a role stored with
	// its number in its name ("Drake 8") folded into the plain role keeps
	// that number on its lines (speakerattr.SplitRoleIndex wraps it into
	// the slots there are), instead of all of them becoming the role's
	// one unnumbered voice.
	slot := 0
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
		if target.IsRole || req.AsRole {
			if label, n := speakerattr.SplitRoleIndex(char.Name); n > 0 && strings.EqualFold(label, target.Name) {
				slot = n
			}
		}
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
		if clean, _ := speakerattr.SanitizeSpeaker(char.Name); clean == char.Name && !req.AsRole && !req.NoAlias {
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
	} else {
		if slot > 0 {
			for _, b := range books {
				if err := s.Store.NumberSpeakerLines(b.ID, char.Name, slot); err != nil {
					writeError(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
		}
		if err := s.renameSpeakerEverywhere(books, char.Name, storeTargetName); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
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
// paragraphs in every book of the series (in this book only, for
// "Unknown"), chapter by chapter - one KindSpeakerReattribution
// task per chapter, grouped under one pipeline_auto_split task a book - see
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

	// A real character is split wherever they speak: the roster is the
	// series', and marking the name invalid leaves their lines in the other
	// books orphaned otherwise. "Unknown" is every book's own catch-all,
	// so only the open book's is re-judged.
	books := []store.Book{*book}
	if name != "Unknown" {
		if books, err = s.booksInScope(book); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	type chapterTarget struct {
		ch        *store.Chapter
		targetIdx map[int]bool
	}
	queued := 0
	for i := range books {
		b := &books[i]
		paragraphs, err := s.Store.ListParagraphsRawForBook(b.ID)
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
		if len(targets) == 0 {
			continue
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].ch.Idx < targets[j].ch.Idx })
		queued += len(targets)

		// One pipeline_auto_split task a book wraps every chapter's own
		// KindSpeakerReattribution task, so the click shows up (and can
		// be canceled/promoted) as one Jobs-dashboard row per book - see
		// jobs.BulkGroup.
		s.Jobs.EnqueueBulk(b.ID, jobs.BulkGroup{
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
					if _, err := s.Jobs.RunReattribution(ctx, b.ID, t.ch.ID, t.ch.Idx, tier(), speakerKey, name, func(ctx context.Context) (int, func(), error) {
						return s.reattributeChapterSpeaker(ctx, b, t.ch, speakerKey, name, false, t.targetIdx)
					}); err != nil {
						log.Printf("httpapi: auto split %q in book %s: chapter %d: %v", name, b.ID, t.ch.Idx, err)
					}
				}()
			}
			wg.Wait()
			return nil
		})
	}

	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: queued})
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
		if namekey.Equal(c.Name, name) {
			if c.Invalid {
				writeError(w, http.StatusConflict, fmt.Sprintf("%q is a character marked as not a speaker - delete it first", c.Name))
			} else {
				writeError(w, http.StatusConflict, fmt.Sprintf("%q is already a character - merge the two instead", c.Name))
			}
			return
		}
		if !c.Invalid && slices.ContainsFunc(c.Aliases, func(a string) bool { return namekey.Equal(a, name) }) {
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
	taken := map[string]string{} // folded (namekey.Fold) name/alias -> owning character's name
	for _, c := range roster {
		if c.ID == char.ID || c.Invalid {
			continue
		}
		taken[namekey.Fold(c.Name)] = c.Name
		for _, a := range c.Aliases {
			taken[namekey.Fold(a)] = c.Name
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
		if owner, ok := taken[namekey.Fold(a)]; ok {
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

// splitCharacterAliasRequest is POST .../characters/{characterId}/aliases/split's body.
type splitCharacterAliasRequest struct {
	// Alias is the one of the character's other names that is really
	// somebody else.
	Alias string `json:"alias,omitempty"`
	// Aliases is several of them at once, each somebody else and nobody the
	// same as another: one pass over the character's lines sorts them among
	// everyone, rather than a pass per name each looking at the same lines.
	Aliases []string `json:"aliases,omitempty"`
}

// handleSplitCharacterAlias makes one of a character's aliases (or
// several, each their own person) a character of its own - for a name
// that was merged or folded into the wrong person. The alias comes off
// the character (so attribution stops
// folding it back), a character of that name is created if the series has
// none, and the lines that may be theirs are looked over again: in every
// chapter of the series whose text writes the alias, the character's lines
// are re-judged among them (and anyone else) by the same per-chapter
// reattribution Auto Split runs, one pipeline_auto_split row per book.
// Lines the reader set by hand stay, as does every line in a chapter that
// never writes the alias.
//
// Answers 202 with the chapters queued - 0 when no chapter writes the
// alias, or when the LLM isn't configured (the roster change is still
// made). 400 for a name that isn't one of the character's aliases; 409
// when the series has that name as a character marked not a speaker.
func (s *Server) handleSplitCharacterAlias(w http.ResponseWriter, r *http.Request) {
	var req splitCharacterAliasRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
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
	aliases := req.Aliases
	if req.Alias != "" {
		aliases = append([]string{req.Alias}, aliases...)
	}
	queued, err := s.splitCharacterAliases(book, char, aliases)
	if err != nil {
		writeBuildError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: queued})
}

// splitCharacterAliases is handleSplitCharacterAlias's change, shared with
// a roster-review proposal accepted as a split: each alias off char, a
// character of each name, and char's lines re-judged in the chapters that
// write any of them - one pass however many names, so a line is weighed
// between all of them at once. It returns how many chapters were queued; a
// refusal is an httpError, made before anything is changed.
func (s *Server) splitCharacterAliases(book *store.Book, char *store.Character, aliases []string) (int, error) {
	remaining := slices.Clone([]string(char.Aliases))
	var names []string
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if slices.ContainsFunc(names, func(n string) bool { return namekey.Equal(n, alias) }) {
			continue
		}
		at := slices.IndexFunc(remaining, func(a string) bool { return namekey.Equal(a, alias) })
		if alias == "" || at < 0 {
			return 0, httpError(http.StatusBadRequest, fmt.Sprintf("%q isn't another name of %s", alias, char.Name))
		}
		alias = remaining[at]
		existing, err := s.Store.GetCharacterByName(char.Scope, alias)
		if err != nil {
			return 0, err
		}
		if existing != nil && existing.Invalid {
			return 0, httpError(http.StatusConflict, fmt.Sprintf("%q is a character marked as not a speaker - delete it first", existing.Name))
		}
		if existing != nil && existing.ID == char.ID {
			// A case variant of the character's own name, not another person.
			return 0, httpError(http.StatusBadRequest, fmt.Sprintf("%q is this character's own name (names are matched ignoring case)", alias))
		}
		remaining = slices.Delete(remaining, at, at+1)
		names = append(names, alias)
	}
	if len(names) == 0 {
		return 0, httpError(http.StatusBadRequest, "name the alias to split off")
	}

	if err := s.Store.SetCharacterAliases(char.ID, remaining); err != nil {
		return 0, err
	}
	splitIDs := make([]string, 0, len(names))
	for i, name := range names {
		split, _, err := s.Store.UpsertCharacter(char.Scope, name, false)
		if err != nil {
			return 0, err
		}
		names[i] = split.Name
		splitIDs = append(splitIDs, split.ID)
	}
	if s.Speaker == nil {
		return 0, nil
	}

	books, err := s.booksInScope(book)
	if err != nil {
		return 0, err
	}
	// Its own key, so an Auto Split of the same character isn't taken for
	// this (see EnqueueReattribution's dedup key).
	sort.Strings(splitIDs)
	speakerKey := char.ID + ":alias:" + strings.Join(splitIDs, "+")
	label := strings.Join(names, ", ") + " from " + char.Name
	queued := 0
	for _, b := range books {
		paragraphs, err := s.Store.ListParagraphsRawForBook(b.ID)
		if err != nil {
			return 0, err
		}
		// Only chapters that write one of the aliases: nobody speaks under a name
		// their chapter never uses, and a lead's every line in every
		// chapter is a lot to ask about again.
		written := map[string]bool{}
		targetByChapter := map[string]map[int]bool{}
		for _, p := range paragraphs {
			if !written[p.ChapterID] && slices.ContainsFunc(names, func(n string) bool { return speakerattr.MentionsWord(p.Text, n) }) {
				written[p.ChapterID] = true
			}
			if p.Speaker != char.Name {
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
			if !written[chapterID] {
				continue
			}
			ch, err := s.Store.GetChapterByID(chapterID)
			if err != nil {
				return 0, err
			}
			if ch == nil {
				continue
			}
			targets = append(targets, chapterTarget{ch: ch, targetIdx: targetIdx})
		}
		if len(targets) == 0 {
			continue
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].ch.Idx < targets[j].ch.Idx })
		queued += len(targets)
		s.Jobs.EnqueueBulk(b.ID, jobs.BulkGroup{
			Kind:     "pipeline_auto_split",
			Key:      "auto_split:" + speakerKey,
			Label:    label,
			Tier:     jobs.TierBackground,
			Children: &jobs.ChildFilter{Kinds: []jobs.Kind{jobs.KindSpeakerReattribution}, Label: label},
		}, func(ctx context.Context, tier func() int) error {
			var wg sync.WaitGroup
			for _, t := range targets {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := s.Jobs.RunReattribution(ctx, b.ID, t.ch.ID, t.ch.Idx, tier(), speakerKey, label, func(ctx context.Context) (int, func(), error) {
						return s.reattributeChapterSpeaker(ctx, &b, t.ch, speakerKey, char.Name, true, t.targetIdx)
					}); err != nil {
						log.Printf("httpapi: split %q from %q in book %s: chapter %d: %v", names, char.Name, b.ID, t.ch.Idx, err)
					}
				}()
			}
			wg.Wait()
			return nil
		})
	}
	return queued, nil
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

// speakerLinesDTO is one speaker's dialogue lines across a series.
type speakerLinesDTO struct {
	Name  string `json:"name"`
	Lines int    `json:"lines"`
}

// seriesSpeakerLinesResponse is GET .../speakers/series-lines' reply.
type seriesSpeakerLinesResponse struct {
	// Speakers are the series' speakers with at least one line of
	// dialogue, most lines first.
	Speakers []speakerLinesDTO `json:"speakers"`
}

// handleSeriesSpeakerLines serves GET /api/books/{id}/speakers/series-lines:
// how many lines of dialogue each speaker has across every book of the
// book's series - what the Speakers page's whole-series view counts and
// sorts by (the speakers topic counts one book's). Not a live topic: it
// reads every book of the series.
func (s *Server) handleSeriesSpeakerLines(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	books, err := s.booksInScope(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	lines, err := s.rosterLineCounts(books)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]speakerLinesDTO, 0, len(lines))
	for name, n := range lines {
		out = append(out, speakerLinesDTO{Name: name, Lines: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lines != out[j].Lines {
			return out[i].Lines > out[j].Lines
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, seriesSpeakerLinesResponse{Speakers: out})
}

type setCharacterPeopleRequest struct {
	// Race is the kind of being they are ("drake") and Nationality where
	// they are from or whose they are ("Pallassian"); "" clears one.
	Race        string `json:"race"`
	Nationality string `json:"nationality"`
	// Age is their age in years, as the reader states it; 0 clears it.
	// Setting one replaces the ages the text gave (they stay listed as
	// skipped notes).
	Age int `json:"age"`
}

// handleSetCharacterPeople serves PUT
// /api/books/{id}/characters/{characterId}/people: sets one character's
// race, nationality and age by hand - each the one accepted character note
// of its kind (store.SetCharacterNoteValue), replacing whatever fact finding
// proposed or the reader accepted before. Their casting is shown both,
// and the world facts filed under those words. Like pronouns, it doesn't
// re-characterize on its own - the Speakers page's "Regenerate" does.
func (s *Server) handleSetCharacterPeople(w http.ResponseWriter, r *http.Request) {
	var req setCharacterPeopleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
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
	if req.Age < 0 || req.Age > 1_000_000 {
		writeError(w, http.StatusBadRequest, "age must be a number of years")
		return
	}
	age := ""
	if req.Age > 0 {
		age = strconv.Itoa(req.Age)
	}
	for kind, value := range map[store.CharacterNoteKind]string{store.NoteKind: req.Race, store.NoteNationality: req.Nationality, store.NoteAge: age} {
		if err := s.Store.SetCharacterNoteValue(id, kind, value); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeNoContent(w)
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
			if v, err := s.Narration.ForParagraph(b, char.Name, a.SpeakerIndex, a.ChapterIdx); err == nil {
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
