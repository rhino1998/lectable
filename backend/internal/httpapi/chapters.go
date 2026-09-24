package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/deliverytags"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// directionMarkDTO is one delivery tag pinned to where it was actually
// inserted in p.Text, so the frontend can render an inline caret at that
// exact point instead of just listing tags for the paragraph as a whole.
type directionMarkDTO struct {
	// Offset is a rune (Unicode code point) index into Text, not a byte
	// offset - JavaScript string indexing is UTF-16-code-unit-based, and a
	// rune index matches that exactly for every character actually found
	// in book prose (accents, curly quotes, em-dashes - all inside the
	// Basic Multilingual Plane, one UTF-16 unit each); it would only drift
	// for a character requiring a UTF-16 surrogate pair (e.g. an emoji),
	// which no delivery tag is ever anchored beside in practice.
	Offset int    `json:"offset"`
	Tag    string `json:"tag"`
}

// directionMarksFor extracts every delivery tag active on p for
// cloneModel, from both its sentence-level and positional annotations
// (see store.ParagraphDirection), each pinned to its own byte offset
// converted to a rune offset (see directionMarkDTO.Offset) - in the order
// they'd actually be spoken, mirroring the same offset-sort
// deliverytags.Merge itself uses to build the real generation text
// (sentence-level insertions ordered first at a shared offset). A stored
// annotation that fails to re-extract against p.Text (should never happen
// for anything this app itself wrote) is silently skipped, the same
// graceful-degradation treatment Paragraph.ResolveGenerationText gives it.
func directionMarksFor(p store.Paragraph, cloneModel string) []directionMarkDTO {
	d, ok := p.Tags[cloneModel]
	if !ok {
		return nil
	}
	type ranked struct {
		deliverytags.Insertion
		setIdx int
	}
	var all []ranked
	if d.SentenceText != "" {
		if ins, err := deliverytags.ExtractInsertions(p.Text, d.SentenceText); err == nil {
			for _, i := range ins {
				all = append(all, ranked{i, 0})
			}
		}
	}
	if d.InlineText != "" {
		if ins, err := deliverytags.ExtractInsertions(p.Text, d.InlineText); err == nil {
			for _, i := range ins {
				all = append(all, ranked{i, 1})
			}
		}
	}
	if len(all) == 0 {
		return nil
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Offset != all[j].Offset {
			return all[i].Offset < all[j].Offset
		}
		return all[i].setIdx < all[j].setIdx
	})
	out := make([]directionMarkDTO, len(all))
	for i, r := range all {
		out[i] = directionMarkDTO{Offset: utf8.RuneCountInString(p.Text[:r.Offset]), Tag: r.Tag}
	}
	return out
}

// pronunciationMarkDTO is one resolved pronunciation substitution pinned
// to where it applies in p.Text, so the frontend can render a caret
// directly over the exact word it replaces (see PronunciationCaret in
// ParagraphText.tsx) - the pronunciation-resolution counterpart of
// directionMarkDTO, but never clone-model-scoped (see
// store.Paragraph.Pronunciation's own doc comment).
type pronunciationMarkDTO struct {
	// Offset/Length are rune (Unicode code point) indices/counts into
	// Text, not byte ones - see directionMarkDTO.Offset's own doc comment
	// for why (JS string indexing is UTF-16-code-unit-based). Every
	// abbreviation pronunciationCandidates matches is plain ASCII, so
	// byte and rune counts are identical in practice, but converting
	// properly here costs nothing and keeps this consistent with
	// directionMarkDTO rather than relying on that coincidence.
	Offset int `json:"offset"`
	Length int `json:"length"`
	// Original is the exact text this mark covers (e.g. "Dr."), sent
	// alongside Replacement so the frontend's hover tooltip can show both
	// without needing to re-derive Original by slicing Text itself.
	Original    string `json:"original"`
	Replacement string `json:"replacement"`
}

// pronunciationMarksFor extracts every resolved pronunciation
// substitution active on p (internal/speakerattr.Client.
// ResolvePronunciation via Store.SetParagraphPronunciation), converting
// each one's byte Offset/Length into rune coordinates - see
// pronunciationMarkDTO.Offset's own doc comment. Substitutions are stored
// pre-resolved (never freely-generated text - see internal/pronounce's
// own doc comment), so there's no diff-and-validate step needed here the
// way directionMarksFor needs deliverytags.ExtractInsertions.
func pronunciationMarksFor(p store.Paragraph) []pronunciationMarkDTO {
	if len(p.Pronunciation) == 0 {
		return nil
	}
	out := make([]pronunciationMarkDTO, len(p.Pronunciation))
	for i, sub := range p.Pronunciation {
		out[i] = pronunciationMarkDTO{
			Offset:      utf8.RuneCountInString(p.Text[:sub.Offset]),
			Length:      utf8.RuneCountInString(p.Text[sub.Offset : sub.Offset+sub.Length]),
			Original:    p.Text[sub.Offset : sub.Offset+sub.Length],
			Replacement: sub.Replacement,
		}
	}
	return out
}

type paragraphDTO struct {
	Idx             int     `json:"idx"`
	Text            string  `json:"text"`
	AudioStatus     string  `json:"audioStatus"`
	AudioError      string  `json:"audioError,omitempty"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	AudioURL        string  `json:"audioUrl,omitempty"`
	// AudioPointerSeconds is set only when this paragraph is a scare-quote
	// merge group pointer (store.AudioState.PointerOffset > 0): the time,
	// in seconds within AudioURL's own shared clip, where this paragraph's
	// own content actually begins - AudioURL itself already resolves to
	// whichever paragraph holds that shared recording (see resolveAudioID).
	// The frontend player uses this to know when playback (which it never
	// seeks or restarts at a merge group's own internal boundaries - see
	// usePlayback.ts) has reached this paragraph's own span, for
	// highlighting/position-reporting purposes only. Omitted (0) for a
	// paragraph with its own real file, which always begins at the very
	// start of AudioURL's clip.
	AudioPointerSeconds float64 `json:"audioPointerSeconds,omitempty"`
	// Speaker is "" (never attributed) or "Narrator", or a character name -
	// see store.Paragraph.Speaker. Only actually changes this paragraph's
	// narration voice when the book has MultiVoice on (see
	// internal/narration.Resolver) and the named character has a voice
	// assigned - otherwise purely informational.
	Speaker string `json:"speaker,omitempty"`
	// Inline marks this paragraph as a split-out continuation of the
	// previous one, not the start of a new visual paragraph - see
	// store.Paragraph.Inline. The frontend should render consecutive
	// Inline paragraphs (and contentItemDTO's own Inline, below) joined
	// into one paragraph block rather than with a break between them,
	// while still treating each as its own playback/highlight unit.
	Inline bool `json:"inline,omitempty"`
	// IsQuote marks this as an actual quoted-dialogue span, as opposed to
	// narration/description - see store.Paragraph.IsQuote. This, not
	// Speaker being non-empty, is what tells the frontend's annotations
	// view a segment is dialogue: an unattributed quote still has
	// Speaker == "" but is dialogue all the same.
	IsQuote bool `json:"isQuote,omitempty"`
	// ScareQuote marks an IsQuote paragraph as NOT actually spoken dialogue
	// despite looking like it structurally - see store.Paragraph.ScareQuote.
	// Always false for a non-IsQuote paragraph.
	ScareQuote bool `json:"scareQuote,omitempty"`
	// DescribesCharacters is which characters (if any) this paragraph's
	// narration describes - see store.Paragraph.DescribesCharacters.
	// Always empty when IsQuote is true.
	DescribesCharacters []string `json:"describesCharacters,omitempty"`
	// DirectionMarks is every inline delivery tag (e.g. "<|emotion:anger|>",
	// "<|sfx:laughter|>") currently active on this paragraph for whichever
	// clone model it's resolved to narrate through, each pinned to where
	// it was actually inserted in Text (see directionMarkDTO.Offset) so
	// the frontend can render an inline caret there - both from the
	// sentence-level pass and the positional one (see
	// store.ParagraphDirection), in the order they'd actually be spoken.
	// Empty for the common case (no annotation, or a clone model that
	// doesn't understand this vocabulary at all - see
	// internal/speakerattr.validSentenceTags/validInlineTags).
	DirectionMarks []directionMarkDTO `json:"directionMarks,omitempty"`
	// PronunciationMarks is every resolved pronunciation substitution
	// active on this paragraph (e.g. "Dr." -> "Doctor"), each pinned to
	// the word it replaces (see pronunciationMarkDTO.Offset) so the
	// frontend can render a caret directly over it. Unlike DirectionMarks,
	// not clone-model-scoped - see store.Paragraph.Pronunciation's own
	// doc comment. Empty for the common case (no ambiguous abbreviation
	// found in this paragraph).
	PronunciationMarks []pronunciationMarkDTO `json:"pronunciationMarks,omitempty"`
	// Words is a [{text,start,end}] array from the ttsworker's
	// forced alignment (see ttsworker.Manager.Align), or "[]" before
	// alignment has run - always present (unlike the websocket push's
	// Words, which is omitted for updates that aren't about alignment)
	// since this is a full paragraph snapshot, not a patch.
	Words json.RawMessage `json:"words"`
	// SFX* mirror store.Paragraph's own sound-effect test fields - see
	// its doc comments. SFXAudioURL is set only once SFXStatus ==
	// store.AudioReady, the same "URL only when actually fetchable"
	// convention AudioURL follows.
	SFXPrompt          string  `json:"sfxPrompt,omitempty"`
	SFXStatus          string  `json:"sfxStatus,omitempty"`
	SFXError           string  `json:"sfxError,omitempty"`
	SFXAudioURL        string  `json:"sfxAudioUrl,omitempty"`
	SFXDurationSeconds float64 `json:"sfxDurationSeconds,omitempty"`
	// SFXTriggerWord mirrors store.Paragraph.SFXTriggerWord - which word
	// (a 0-based index into Words above) playback should start the sound
	// effect at, once ready.
	SFXTriggerWord int `json:"sfxTriggerWord,omitempty"`
	// ContentHash/AudioHash are this paragraph's leaves in the offline-sync
	// hash tree (see manifest.go): ContentHash covers what an offline copy
	// stores about the paragraph's text and annotations, AudioHash what
	// identifies its audio (resolved voice, status, backing clip, duration,
	// pointer) - so a client can tell "refresh the metadata" apart from
	// "re-download the .wav".
	ContentHash string `json:"contentHash"`
	AudioHash   string `json:"audioHash"`
}

// resolveAudioID returns which paragraph's own audio file actually backs a
// ready paragraph's audioUrl: itself, unless state marks it a scare-quote
// merge group pointer (see store.AudioState.PointerOffset/
// jobs.Manager.handleMergedResult), in which case it's the paragraph
// PointerOffset positions earlier by Idx in the same chapter+voice - the
// one that actually holds the shared, unsplit recording. byIdx must map
// every paragraph in p's own chapter by its own Idx (ListParagraphsRaw's
// own result, keyed once by the caller).
//
// Returning the SAME id string both paragraphs would report is what lets
// the frontend player recognize a merge group's own internal boundaries
// and keep playing straight through them instead of reloading/restarting
// audio that's already mid-playback (see usePlayback.ts) - a redirect or
// any other mechanism that serves identical bytes from a different URL
// wouldn't give it anything to compare against.
func resolveAudioID(p store.Paragraph, state store.AudioState, byIdx map[int]string) string {
	if state.PointerOffset == 0 {
		return p.ID
	}
	if anchorID, ok := byIdx[p.Idx-state.PointerOffset]; ok {
		return anchorID
	}
	return p.ID
}

// contentItemDTO gives the frontend chapter content in original document
// order (text interleaved with images). Text items point back into
// `paragraphs` by index rather than duplicating its fields, since
// `paragraphs` is also what playback/position logic indexes into directly.
type contentItemDTO struct {
	Kind string `json:"kind"` // "text" | "image" | "break"
	// No omitempty: 0 is a valid, common paragraphIdx (the first paragraph
	// of a chapter), and omitempty would silently drop it.
	ParagraphIdx int    `json:"paragraphIdx"`
	ImageURL     string `json:"imageUrl,omitempty"`
	// Inline mirrors the referenced paragraph's own Inline (see
	// paragraphDTO.Inline) - duplicated here so the frontend's
	// content-ordering pass can decide paragraph breaks without a second
	// lookup into `paragraphs`.
	Inline bool `json:"inline,omitempty"`
}

type chapterDetailDTO struct {
	Idx        int              `json:"idx"`
	Title      string           `json:"title"`
	Generating bool             `json:"generating"`
	Paragraphs []paragraphDTO   `json:"paragraphs"`
	Content    []contentItemDTO `json:"content"`
	// Hash is this chapter's node in the offline-sync hash tree - see
	// chapterHash.
	Hash string `json:"hash"`
}

func (s *Server) handleGetChapter(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	writeBuilt(w)(s.buildChapter(r.PathValue("id"), idx))
}

func (s *Server) buildChapter(bookID string, idx int) (any, error) {

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}

	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if ch == nil {
		return nil, httpError(http.StatusNotFound, "chapter not found")
	}

	paragraphs, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	images, err := s.Store.ListImages(ch.ID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	breaks, err := s.Store.ListBreaks(ch.ID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}

	// Resolve each paragraph's own effective voice (a character's assigned
	// voice can override the book's own when book.MultiVoice is on - see
	// internal/narration.Resolver) and batch-fetch audio status grouped by
	// the resulting voice_id, since a chapter's paragraphs no longer
	// necessarily share one voice the way a single JOIN could assume.
	bookVoice, err := s.Narration.BookVoice(book)
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
	// A character's assigned voice is per clone model (see
	// store.Character's doc comment) - resolve against whichever model this
	// book's own voice currently narrates through.
	effectiveCloneModel := narration.EffectiveCloneModel(bookVoice)
	presetIDByChar, err := s.Store.CharacterVoicesForModel(characterIDs, effectiveCloneModel)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	resolvedVoiceCache := map[string]narration.ResolvedVoice{} // character id -> resolved, avoids repeat lookups
	idsByVoice := map[string][]string{}
	voiceIDByParagraph := make(map[string]string, len(paragraphs))
	// cloneModelByParagraph mirrors voiceIDByParagraph, one step further -
	// which clone_model key of this paragraph's own Tags map is actually
	// active for it right now (see paragraphDTO.DirectionTag below).
	cloneModelByParagraph := make(map[string]string, len(paragraphs))
	for _, p := range paragraphs {
		v := bookVoice
		if book.MultiVoice() && p.Speaker != "" && p.Speaker != "Narrator" {
			if c, ok := charByName[p.Speaker]; ok {
				if cached, ok := resolvedVoiceCache[c.ID]; ok {
					v = cached
				} else if resolved, err := s.Narration.ResolveCharacterVoice(book, bookVoice, c, effectiveCloneModel, presetIDByChar[c.ID]); err != nil {
					return nil, httpError(http.StatusInternalServerError, err.Error())
				} else {
					resolvedVoiceCache[c.ID] = resolved
					v = resolved
				}
			}
		}
		vid := v.VoiceID()
		voiceIDByParagraph[p.ID] = vid
		idsByVoice[vid] = append(idsByVoice[vid], p.ID)
		cloneModelByParagraph[p.ID] = narration.EffectiveCloneModel(v)
	}
	audioStates := make(map[string]store.AudioState, len(paragraphs))
	for vid, ids := range idsByVoice {
		states, err := s.Store.ParagraphAudioStatuses(ids, vid)
		if err != nil {
			return nil, httpError(http.StatusInternalServerError, err.Error())
		}
		for id, st := range states {
			audioStates[id] = st
		}
	}

	idByIdx := make(map[int]string, len(paragraphs))
	paragraphIDs := make([]string, len(paragraphs))
	for i, p := range paragraphs {
		idByIdx[p.Idx] = p.ID
		paragraphIDs[i] = p.ID
	}
	sfxStates, err := s.Store.ParagraphSFXStates(paragraphIDs)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}

	dto := chapterDetailDTO{Idx: ch.Idx, Title: ch.Title, Generating: s.Jobs.IsGenerating(ch.ID)}
	for _, p := range paragraphs {
		state, ok := audioStates[p.ID]
		if !ok {
			state = store.AudioState{Status: store.AudioPending}
		}
		// Absent from sfxStates (the overwhelmingly common case) resolves
		// to the zero SFXState{} - see ParagraphSFXStates' own doc comment.
		sfxState := sfxStates[p.ID]
		pd := paragraphDTO{
			Idx:                 p.Idx,
			Text:                p.Text,
			Speaker:             p.Speaker,
			Inline:              p.Inline,
			IsQuote:             p.IsQuote,
			ScareQuote:          p.ScareQuote,
			DescribesCharacters: p.DescribesCharacters,
			DirectionMarks:      directionMarksFor(p, cloneModelByParagraph[p.ID]),
			PronunciationMarks:  pronunciationMarksFor(p),
			AudioStatus:         state.Status,
			AudioError:          state.Error,
			DurationSeconds:     state.DurationSeconds,
			Words:               json.RawMessage(state.WordTimings),
			SFXPrompt:           sfxState.Prompt,
			SFXStatus:           sfxState.Status,
			SFXError:            sfxState.Error,
			SFXDurationSeconds:  sfxState.DurationSeconds,
			SFXTriggerWord:      sfxState.TriggerWord,
		}
		if sfxState.Status == store.AudioReady {
			pd.SFXAudioURL = "/api/paragraphs/" + p.ID + "/sfx-audio"
		}
		if len(pd.Words) == 0 {
			// Not just the nil case: a paragraph with no audio row yet
			// (state defaulted above to the AudioPending zero value) has
			// WordTimings == "" too, and json.RawMessage("") is a non-nil
			// but empty - and so invalid - JSON value, which fails to
			// marshal at all ("unexpected end of JSON input") rather than
			// silently encoding as anything.
			pd.Words = json.RawMessage("[]")
		}
		if state.Status == store.AudioReady {
			pd.AudioURL = "/api/paragraphs/" + resolveAudioID(p, state, idByIdx) + "/audio"
			pd.AudioPointerSeconds = state.PointerSeconds
		}
		pd.ContentHash = paragraphContentHash(pd)
		pd.AudioHash = paragraphAudioHash(pd, voiceIDByParagraph[p.ID])
		dto.Paragraphs = append(dto.Paragraphs, pd)
	}

	type positioned struct {
		position int
		item     contentItemDTO
	}
	items := make([]positioned, 0, len(paragraphs)+len(images)+len(breaks))
	for _, p := range paragraphs {
		items = append(items, positioned{p.Position, contentItemDTO{Kind: "text", ParagraphIdx: p.Idx, Inline: p.Inline}})
	}
	for _, img := range images {
		items = append(items, positioned{img.Position, contentItemDTO{Kind: "image", ImageURL: "/api/images/" + img.ID}})
	}
	for _, brk := range breaks {
		items = append(items, positioned{brk.Position, contentItemDTO{Kind: "break"}})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].position < items[j].position })
	dto.Content = make([]contentItemDTO, len(items))
	for i, it := range items {
		dto.Content[i] = it.item
	}
	dto.Hash = chapterHash(dto)

	return dto, nil
}

// handleGenerateChapter enqueues background TTS generation for one
// chapter's not-yet-generated paragraphs, as a cancelable poolPipeline
// task - see jobs.Manager.EnqueueChapter's own doc comment.
func (s *Server) handleGenerateChapter(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
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

	s.Jobs.EnqueueChapter(bookID, ch.ID, ch.Idx)
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

// handleRegenerateParagraph re-renders one already-generated paragraph
// from scratch - the reader didn't like how a specific line came out, not
// the normal "this hasn't been generated yet" path handleGenerateChapter
// covers.
func (s *Server) handleRegenerateParagraph(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	chapterIdx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	paragraphIdx, err := strconv.Atoi(r.PathValue("pidx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid paragraph index")
		return
	}

	ch, err := s.Store.GetChapterByIdx(bookID, chapterIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}

	paragraphID, err := s.Store.GetParagraphIDByIdx(ch.ID, paragraphIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if paragraphID == "" {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}
	p, err := s.Store.GetParagraph(paragraphID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}

	s.Jobs.EnqueueParagraphRegenerate(bookID, ch.ID, ch.Idx, *p)
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

// paragraphByChapterIdx is the shared bookID+chapterIdx+paragraphIdx ->
// (chapter, paragraph) lookup every one of this file's per-paragraph
// handlers repeats (handleRegenerateParagraph, handleSetParagraphSpeaker,
// handleSetParagraphScareQuote, and now the SFX handlers below) -
// factored out here rather than in this file's earlier handlers only
// because those predate this refactor; ok is false (and a response
// already written) on any lookup failure, so callers can just `if !ok {
// return }`.
func (s *Server) paragraphByChapterIdx(w http.ResponseWriter, bookID string, chapterIdx, paragraphIdx int) (ch *store.Chapter, p *store.Paragraph, ok bool) {
	var err error
	ch, err = s.Store.GetChapterByIdx(bookID, chapterIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return nil, nil, false
	}
	paragraphID, err := s.Store.GetParagraphIDByIdx(ch.ID, paragraphIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	if paragraphID == "" {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return nil, nil, false
	}
	p, err = s.Store.GetParagraph(paragraphID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return nil, nil, false
	}
	return ch, p, true
}

type setParagraphSFXPromptRequest struct {
	Prompt string `json:"prompt"`
	// TriggerWord, when non-nil, also sets which word (a 0-based index
	// into this paragraph's own forced-alignment Words) playback should
	// start the sound effect at - see store.Paragraph.SFXTriggerWord.
	// Folded into this same request rather than a separate endpoint since
	// the frontend's SFX panel always edits both together (see
	// ChapterSection.tsx's own word-picker).
	TriggerWord *int `json:"triggerWord,omitempty"`
}

// handleSetParagraphSFXPrompt saves (without generating) one paragraph's
// sound-effect test prompt and/or trigger word - a plumbing-only test
// surface for now (see backend/CLAUDE.md's "SFX sound effects" section on
// why the prompt is reader-entered rather than automatically detected).
// Doesn't touch any already-generated clip or status.
func (s *Server) handleSetParagraphSFXPrompt(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	chapterIdx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	paragraphIdx, err := strconv.Atoi(r.PathValue("pidx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid paragraph index")
		return
	}
	var req setParagraphSFXPromptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	_, p, ok := s.paragraphByChapterIdx(w, bookID, chapterIdx, paragraphIdx)
	if !ok {
		return
	}
	if err := s.Store.SetParagraphSFXPrompt(p.ID, strings.TrimSpace(req.Prompt)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.TriggerWord != nil {
		if err := s.Store.SetParagraphSFXTriggerWord(p.ID, *req.TriggerWord); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleGenerateParagraphSFX renders (or re-renders) one paragraph's sound
// effect - via Stable Audio's own SFX checkpoint (StableAudioSFX) - from
// its own saved sfx_prompt, or from a prompt sent inline in this same
// request (persisted first, same as if handleSetParagraphSFXPrompt had
// just been called) - a convenience so the test UI can save+generate in
// one click. Dispatched through jobs.Manager.EnqueueSFXGeneration
// (KindSFXGeneration, poolSFX) rather than calling s.TTS.StableAudioSFX
// directly and blocking this request on it: it's a multi-step diffusion
// call, materially slower than an ordinary clone, and this way it
// properly shares poolSFX's concurrency limit and TTS/LLM-pool mutual
// exclusion with every other real generation task instead of racing the
// worker uncoordinated - returns 202 immediately, same shape as
// handleRegenerateParagraph. The frontend picks up completion via
// chapterQueryOptions' own poll (see api/queries.ts) rather than a
// dedicated push.
func (s *Server) handleGenerateParagraphSFX(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	chapterIdx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	paragraphIdx, err := strconv.Atoi(r.PathValue("pidx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid paragraph index")
		return
	}
	var req struct {
		Prompt          string   `json:"prompt"`
		DurationSeconds *float64 `json:"durationSeconds"`
	}
	// Body is optional - a caller that already saved a prompt via
	// handleSetParagraphSFXPrompt can just POST with no body.
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	ch, p, ok := s.paragraphByChapterIdx(w, bookID, chapterIdx, paragraphIdx)
	if !ok {
		return
	}
	sfxState, err := s.Store.GetParagraphSFXState(p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(sfxState.Prompt)
	} else if prompt != sfxState.Prompt {
		if err := s.Store.SetParagraphSFXPrompt(p.ID, prompt); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "sfx prompt required")
		return
	}
	if s.TTS == nil {
		writeError(w, http.StatusServiceUnavailable, "tts worker not configured")
		return
	}

	if err := s.Store.SetParagraphSFXGenerating(p.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var durationSeconds float64
	if req.DurationSeconds != nil {
		durationSeconds = *req.DurationSeconds
	}
	s.Jobs.EnqueueSFXGeneration(ch.BookID, ch.ID, ch.Idx, *p, func(ctx context.Context, attempt int) error {
		wavBytes, err := s.TTS.StableAudioSFX(ctx, ttsproto.StableAudioRequest{Prompt: prompt, DurationSeconds: durationSeconds})
		if err != nil {
			_ = s.Store.SetParagraphSFXError(p.ID, err.Error())
			return fmt.Errorf("sfx generation failed: %w", err)
		}
		if err := audiopath.EnsureSFXDir(s.DataDir, ch.BookID, ch.ID); err != nil {
			_ = s.Store.SetParagraphSFXError(p.ID, err.Error())
			return err
		}
		if err := os.WriteFile(audiopath.SFXFile(s.DataDir, ch.BookID, ch.ID, p.Idx), wavBytes, 0o644); err != nil {
			_ = s.Store.SetParagraphSFXError(p.ID, err.Error())
			return err
		}
		dur, err := wav.Duration(wavBytes)
		if err != nil {
			_ = s.Store.SetParagraphSFXError(p.ID, err.Error())
			return err
		}
		return s.Store.SetParagraphSFXReady(p.ID, dur.Seconds())
	})

	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

// handleGetParagraphSFXAudio serves a ready sound-effect clip -
// handleGetAudio's own counterpart, simpler since SFX audio isn't
// voice-scoped.
func (s *Server) handleGetParagraphSFXAudio(w http.ResponseWriter, r *http.Request) {
	paragraphID := r.PathValue("id")
	p, err := s.Store.GetParagraph(paragraphID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}
	sfxState, err := s.Store.GetParagraphSFXState(paragraphID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sfxState.Status != store.AudioReady {
		writeError(w, http.StatusNotFound, "sfx audio not ready")
		return
	}
	ch, err := s.chapterByID(p.ChapterID)
	if err != nil || ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	http.ServeFile(w, r, audiopath.SFXFile(s.DataDir, ch.BookID, ch.ID, p.Idx))
}

type setParagraphSpeakerRequest struct {
	Speaker string `json:"speaker"`
}

// handleSetParagraphSpeaker corrects one paragraph's speaker attribution
// directly, addressed the same way handleRegenerateParagraph is (book +
// chapter idx + paragraph idx) - the Speakers page's per-character
// "appearances" list uses this to fix one individually misattributed line
// (a model mistake, or a line that's actually someone else's) without
// touching any other paragraph currently sharing that same (wrong)
// speaker, unlike handleMergeCharacter's whole-character reassignment
// (Store.ReassignCharacterSpeaker). "" and "Narrator" are equivalent (see
// store's own paragraphs.speaker column comment) and both normalize to the
// "" stored value handleMergeCharacter's own Narrator case already uses. A
// non-empty, non-Narrator name is registered as a real character first if
// it wasn't one already (UpsertCharacter is idempotent), same as
// handleMergeCharacter, so reassigning to a brand-new name here works
// exactly like discovering it via attribution. Refuses to attribute a
// non-Narrator speaker to a paragraph that isn't actually quoted dialogue
// (IsQuote) - the same invariant httpapi.attributeChapter enforces
// (narration *about* a character isn't them speaking), still worth
// guarding here even though every paragraph reachable through an
// "appearances" list already passed it once at attribution time.
func (s *Server) handleSetParagraphSpeaker(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	chapterIdx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	paragraphIdx, err := strconv.Atoi(r.PathValue("pidx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid paragraph index")
		return
	}
	var req setParagraphSpeakerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	speaker := strings.TrimSpace(req.Speaker)

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, chapterIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	paragraphID, err := s.Store.GetParagraphIDByIdx(ch.ID, paragraphIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if paragraphID == "" {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}
	p, err := s.Store.GetParagraph(paragraphID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}

	if speaker == "Narrator" {
		speaker = ""
	}
	if speaker != "" && !p.IsQuote {
		writeError(w, http.StatusBadRequest, "only quoted dialogue can be attributed to a character")
		return
	}
	if speaker != "" {
		if _, _, err := s.Store.UpsertCharacter(store.SeriesScope(book), speaker, false); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if err := s.Store.SetParagraphSpeaker(paragraphID, speaker); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type setParagraphDescriptionRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// handleSetParagraphDescription reassigns one paragraph's description
// attribution from one character to another - the Speakers page's
// per-character "descriptions" list's own counterpart to
// handleSetParagraphSpeaker for actual dialogue lines. A narration
// paragraph the Describe pass (internal/speakerattr.Client.DescribeChapter)
// tagged as describing From gets From removed from its
// describes_characters list and To added instead - any *other* character
// this same paragraph also describes (DescribesCharacters is a list, not a
// single value - a paragraph can describe more than one person) is left
// untouched. "" and "Narrator" for To both mean "remove this description
// entirely, describing no one now" (mirrors handleSetParagraphSpeaker's own
// Narrator-means-empty convention) - there's no "To must be non-Narrator"
// restriction the way speaker attribution requires IsQuote, since narration
// describing a character is exactly what this feature already is. A
// non-empty, non-Narrator To is registered as a real character first if it
// wasn't one already (UpsertCharacter is idempotent), same as
// handleSetParagraphSpeaker. Refuses to attach a description to a quoted
// paragraph (mirrors DescribesCharacters' own doc comment: only narration
// is ever tagged as a description, never a quoted line).
func (s *Server) handleSetParagraphDescription(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	chapterIdx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	paragraphIdx, err := strconv.Atoi(r.PathValue("pidx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid paragraph index")
		return
	}
	var req setParagraphDescriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	from := strings.TrimSpace(req.From)
	to := strings.TrimSpace(req.To)
	if to == "Narrator" {
		to = ""
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
	ch, err := s.Store.GetChapterByIdx(bookID, chapterIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	paragraphID, err := s.Store.GetParagraphIDByIdx(ch.ID, paragraphIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if paragraphID == "" {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}
	p, err := s.Store.GetParagraph(paragraphID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}
	if to != "" && p.IsQuote {
		writeError(w, http.StatusBadRequest, "only narration paragraphs can describe a character")
		return
	}
	if to != "" {
		if _, _, err := s.Store.UpsertCharacter(store.SeriesScope(book), to, false); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	names := make([]string, 0, len(p.DescribesCharacters)+1)
	for _, name := range p.DescribesCharacters {
		if name == from {
			continue
		}
		names = append(names, name)
	}
	if to != "" && !slices.Contains(names, to) {
		names = append(names, to)
	}

	if err := s.Store.SetParagraphDescriptions(ch.ID, map[int][]string{paragraphIdx: names}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type setParagraphScareQuoteRequest struct {
	ScareQuote bool `json:"scareQuote"`
}

// handleSetParagraphScareQuote lets a reader directly mark (or unmark) one
// quoted paragraph as a scare quote - handleSetParagraphSpeaker/
// handleSetParagraphDescription's own counterpart for
// speakerattr.Client.ScareQuoteChapter's judgment, for the case a reader
// spots one it missed (or wrongly flagged) live while reading, rather than
// waiting on - or overriding - the automatic pass. Refuses a non-quoted
// paragraph the same way handleSetParagraphSpeaker refuses attributing a
// non-quoted one to a character - see store.Paragraph.ScareQuote's own
// "always false for a non-IsQuote paragraph" invariant.
//
// Unlike handleSetParagraphSpeaker/handleSetParagraphDescription (which
// leave any already-generated audio untouched, requiring a separate
// explicit "regenerate" call), this immediately invalidates this
// paragraph's own audio - a deliberate, live correction is exactly the
// "the reader didn't like this line" shape EnqueueParagraphRegenerate
// already treats as urgent, not a passive attribute edit - and, via
// expandToInlineSets, every other paragraph in its own inline paragraph
// set too, since toggling this one flag can change whether that whole
// Inline-linked run is eligible for scare-quote merging - see
// expandToInlineSets' own doc comment for why a merge-mate's already-
// generated audio can otherwise go stale forever. Also re-enqueues every
// invalidated member at urgent priority (the same EnqueueParagraphRegenerate
// call handleRegenerateParagraph itself uses) rather than just invalidating
// and leaving them AudioPending - a reader correcting a scare quote live
// wants to hear the fix, not stumble onto silent/missing audio the next
// time they reach this line, which is all invalidation alone would do
// (nothing scans for AudioPending rows on its own - see
// EnqueueLookahead/EnqueueChapter, the only other paths that dispatch
// generation). Calling it once per inline-set member, not just the toggled
// paragraph, is deliberately redundant with pushResolvedTask/
// scareQuoteMergeGroup's own merge-group expansion - EnqueueParagraphRegenerate
// only expands the one paragraph passed to it, so a merge-mate that isn't
// itself re-enqueued here would otherwise sit invalidated with nothing to
// generate it; pushTask's dedup (keyed off a merge group's own first
// member) makes the resulting duplicate pushes collapse harmlessly.
func (s *Server) handleSetParagraphScareQuote(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	chapterIdx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	paragraphIdx, err := strconv.Atoi(r.PathValue("pidx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid paragraph index")
		return
	}
	var req setParagraphScareQuoteRequest
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
	ch, err := s.Store.GetChapterByIdx(bookID, chapterIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	paragraphID, err := s.Store.GetParagraphIDByIdx(ch.ID, paragraphIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if paragraphID == "" {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}
	p, err := s.Store.GetParagraph(paragraphID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}
	if !p.IsQuote {
		writeError(w, http.StatusBadRequest, "only quoted dialogue can be marked a scare quote")
		return
	}
	if req.ScareQuote == p.ScareQuote {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	if err := s.Store.SetParagraphScareQuotes(ch.ID, map[int]bool{paragraphIdx: req.ScareQuote}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.ScareQuote = req.ScareQuote
	inlineSet := s.expandToInlineSets(ch.ID, []store.Paragraph{*p})
	s.invalidateParagraphAudio(book, inlineSet)
	for _, member := range inlineSet {
		s.Jobs.EnqueueParagraphRegenerate(bookID, ch.ID, ch.Idx, member)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type lookaheadRequest struct {
	ChapterIdx   int `json:"chapterIdx"`
	ParagraphIdx int `json:"paragraphIdx"`
	// Optional: how many paragraphs ahead to generate. Omitted/0 means
	// jobs.LookaheadParagraphCount; clamped to jobs.MaxLookaheadParagraphCount.
	ParagraphCount int `json:"paragraphCount,omitempty"`
}

// handleLookahead keeps some runway of generated audio ahead of wherever
// the reader currently is, spanning into later chapters as needed rather
// than stopping at the current chapter's end - see
// jobs.Manager.EnqueueLookahead.
func (s *Server) handleLookahead(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")

	var req lookaheadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	s.Jobs.EnqueueLookahead(bookID, req.ChapterIdx, req.ParagraphIdx, req.ParagraphCount)
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

func (s *Server) handleGetAudio(w http.ResponseWriter, r *http.Request) {
	paragraphID := r.PathValue("id")
	// Structural lookup only - which voice applies isn't known until the
	// paragraph's book (and so its current voice) is found below.
	p, err := s.Store.GetParagraph(paragraphID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}
	ch, err := s.chapterByID(p.ChapterID)
	if err != nil || ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	book, err := s.Store.GetBook(ch.BookID)
	if err != nil || book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	v, err := s.Narration.ForParagraph(book, p.Speaker)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	voiceID := v.VoiceID()

	status, err := s.Store.GetParagraphAudioStatus(paragraphID, voiceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if status != store.AudioReady {
		writeError(w, http.StatusNotFound, "audio not ready")
		return
	}
	http.ServeFile(w, r, s.paragraphAudioPath(ch.BookID, ch.ID, voiceID, p.Idx))
}

func (s *Server) handleGetImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	img, err := s.Store.GetImage(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if img == nil {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	ch, err := s.chapterByID(img.ChapterID)
	if err != nil || ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	http.ServeFile(w, r, audiopath.ImageFile(s.DataDir, ch.BookID, ch.ID, img.ID, img.Ext))
}
