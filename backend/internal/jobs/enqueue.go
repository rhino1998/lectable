package jobs

import (
	"context"
	"log"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/taskqueue"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
	"github.com/rhino1998/lectable/backend/internal/voices"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// EnqueueChapter now lives in pipeline.go, as a cancelable poolPipeline
// bulk group (see EnqueueChapter's own doc comment).

// EnqueueSample generates only enough leading paragraphs of the chapter to
// cover roughly wordLimit words (whole paragraphs, so it may run a bit
// over), rather than the whole thing. Used right after upload to get a
// couple of real narrated paragraphs fast, which calibrates the book's
// duration estimate without committing to generating an entire chapter the
// reader might never open. Deliberately still a bare goroutine, not a
// pipeline task like EnqueueChapter: this is an internal calibration step
// triggered right after upload, not a user-facing "generate" action with
// anything worth showing/canceling on the Jobs dashboard.
func (m *Manager) EnqueueSample(bookID, chapterID string, chapterIdx, wordLimit int) {
	go m.enqueueChapter(m.ctx, bookID, chapterID, chapterIdx, wordLimit)
}

// EnqueueLengthEstimate renders a short, throwaway sample of chapterID's
// leading text through voices.FastPresetID (cloning via PocketTTS - see
// that preset's own doc comment) and stores the resulting seconds-per-
// character on bookID itself (Store.SetLengthEstimate), completely outside
// this package's normal per-voice paragraph_audio cache/pool machinery:
// the sample is measured and discarded, never written to disk or exposed
// to a reader.
//
// This exists so httpapi.estimateTotalSeconds has a real, generated-audio-
// calibrated number to show right after upload - regardless of how slow
// the book's own actually-selected engine is, and without waiting on
// EnqueueSample's own real narration (which still separately renders in
// the book's own voice for early listening, and still wins once it has
// enough ready audio to calibrate from - see estimateTotalSeconds's own
// doc comment for that tiering).
func (m *Manager) EnqueueLengthEstimate(bookID, chapterID string, wordLimit int) {
	go m.estimateLength(bookID, chapterID, wordLimit)
}

func (m *Manager) estimateLength(bookID, chapterID string, wordLimit int) {
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	paragraphs, err := m.store.ListParagraphsRaw(chapterID)
	if err != nil {
		log.Printf("jobs: length estimate for book %s: list paragraphs: %v", bookID, err)
		return
	}
	var sample strings.Builder
	chars := 0
	for _, p := range paragraphs {
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		if sample.Len() > 0 {
			sample.WriteString("\n\n")
		}
		sample.WriteString(p.Text)
		chars += len(p.Text)
		if len(strings.Fields(sample.String())) >= wordLimit {
			break
		}
	}
	if chars == 0 {
		return
	}

	fast := voices.PresetsByID[voices.FastPresetID]
	refPath, err := voicerefs.EnsureFile(ctx, m.tts, m.dataDir, fast.ID, fast.Instruct, fast.Seed, fast.RefText, fast.SpeedMultiplier, fast.DesignModel)
	if err != nil {
		log.Printf("jobs: length estimate for book %s: ensure fast preset reference clip: %v", bookID, err)
		return
	}
	refAudio, err := os.ReadFile(refPath)
	if err != nil {
		log.Printf("jobs: length estimate for book %s: read reference clip: %v", bookID, err)
		return
	}
	audio, err := m.generateClone(ctx, voices.FastCloneModel, refAudio, fast.RefText, "", sample.String(), "")
	if err != nil {
		log.Printf("jobs: length estimate for book %s: generate: %v", bookID, err)
		return
	}
	dur, err := wav.Duration(audio.WAV)
	if err != nil || dur <= 0 {
		log.Printf("jobs: length estimate for book %s: parse wav duration: %v", bookID, err)
		return
	}
	if err := m.store.SetLengthEstimate(bookID, dur.Seconds()/float64(chars)); err != nil {
		log.Printf("jobs: length estimate for book %s: save: %v", bookID, err)
	}
}

// resolvedParagraph pairs a paragraph with its own effective narration
// voice (internal/narration.Resolver) - a character's assigned voice can
// now override the book's own per paragraph, so unlike the book-wide
// voiceID this package used before, different paragraphs in one chapter
// can resolve to different voices.
type resolvedParagraph struct {
	paragraph store.Paragraph
	voice     narration.ResolvedVoice
}

// paragraphsNeedingGeneration resolves each of chapterID's paragraphs to
// its own effective voice (internal/narration.Resolver) and returns those
// whose audio isn't ready yet under that voice, in paragraph order -
// different paragraphs in the same chapter can resolve to different
// voices, so this can't be a single chapter-wide SQL query any more.
// Before resolving, lazily provisions a voice for any of this chapter's
// speakers who don't have one yet (see provisionMissingCharacterVoices) -
// this is the actual "first requested" moment for most paragraphs, since
// it's called from every Enqueue* path (enqueueChapter/enqueueLookahead).
//
// Voice resolution and the audio-status lookup are both batched across the
// whole chapter (mirroring httpapi.handleGetChapter's own batching, per
// narration.Resolver.ForParagraph's doc comment) rather than resolved one
// paragraph at a time: an earlier version called ForParagraph plus
// GetParagraphAudioStatus per paragraph here, 2-3 single-row store round
// trips each - individually cheap, but this runs from background
// enqueueChapter/enqueueLookahead goroutines right after every attribution
// run (attribution flips many paragraphs onto brand-new character speakers
// at once), so a long chapter meant O(paragraphs) serialized round trips on
// this package's single shared DuckDB connection, visibly slowing down
// every other concurrent request while it ran.
func (m *Manager) paragraphsNeedingGeneration(ctx context.Context, book *store.Book, chapterID string) (resolved []resolvedParagraph, all []store.Paragraph, err error) {
	all, err = m.store.ListParagraphsRaw(chapterID)
	if err != nil {
		return nil, nil, err
	}

	bookVoice, err := m.narration.BookVoice(book)
	if err != nil {
		return nil, nil, err
	}
	speakers := make([]string, len(all))
	for i, p := range all {
		speakers[i] = p.Speaker
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)
	m.provisionMissingCharacterVoices(ctx, book, speakers, cloneModel)

	var presetIDByChar map[string]string
	var charByName map[string]store.Character
	if book.MultiVoice() {
		characters, err := m.store.ListCharacters(store.SeriesScope(book))
		if err != nil {
			return nil, nil, err
		}
		charByName = make(map[string]store.Character, len(characters))
		characterIDs := make([]string, len(characters))
		for i, c := range characters {
			charByName[c.Name] = c
			characterIDs[i] = c.ID
		}
		presetIDByChar, err = m.store.CharacterVoicesForModel(characterIDs, cloneModel)
		if err != nil {
			return nil, nil, err
		}
	}

	resolvedVoiceCache := map[string]narration.ResolvedVoice{} // character id -> resolved, avoids repeat lookups
	voiceByParagraph := make([]narration.ResolvedVoice, len(all))
	idsByVoice := map[string][]string{}
	for i, p := range all {
		v := bookVoice
		if book.MultiVoice() && p.Speaker != "" && p.Speaker != "Narrator" {
			if c, ok := charByName[p.Speaker]; ok {
				if cached, ok := resolvedVoiceCache[c.ID]; ok {
					v = cached
				} else if resolved, err := m.narration.ResolveCharacterVoice(book, bookVoice, c, cloneModel, presetIDByChar[c.ID]); err != nil {
					return nil, nil, err
				} else {
					resolvedVoiceCache[c.ID] = resolved
					v = resolved
				}
				v = narration.MemberVoice(v, c, p.SpeakerIndex)
			}
		}
		voiceByParagraph[i] = v
		vid := v.VoiceID()
		idsByVoice[vid] = append(idsByVoice[vid], p.ID)
	}

	audioStates := make(map[string]store.AudioState, len(all))
	for vid, ids := range idsByVoice {
		states, err := m.store.ParagraphAudioStatuses(ids, vid)
		if err != nil {
			return nil, nil, err
		}
		for id, st := range states {
			audioStates[id] = st
		}
	}

	out := make([]resolvedParagraph, 0, len(all))
	for i, p := range all {
		if audioStates[p.ID].Status == store.AudioReady {
			continue
		}
		out = append(out, resolvedParagraph{paragraph: p, voice: voiceByParagraph[i]})
	}
	return out, all, nil
}

// pushResolvedTask is every caller's own single entry point for dispatching
// one not-yet-ready paragraph (enqueueChapter's whole-chapter loop,
// enqueueLookahead's windowed loop, and EnqueueParagraphRegenerate's single
// explicit paragraph) - and so the one place a scare-quote merge group (see
// store.Paragraph.ScareQuote) can be transparently substituted for rp's own
// lone paragraph, without any of those three callers needing to know or
// care. This matters beyond enqueueChapter's own bulk pass: without it,
// EnqueueParagraphRegenerate's "the reader didn't like this line" button
// clicked on any single member of an already-merged group would silently
// re-render just that one paragraph in isolation, fragmenting the group's
// carefully-merged audio right back into the disjointed clips this feature
// exists to avoid.
//
// chapterParagraphs is chapterID's full paragraph list when the caller
// already loaded it (paragraphsNeedingGeneration returns it) - the merge
// check then reuses it instead of re-reading the whole chapter for every
// paragraph pushed, which with lookahead requests arriving every few
// seconds kept the store's single DB connection saturated. nil loads it.
func (m *Manager) pushResolvedTask(bookID, chapterID string, chapterIdx, tier int, rp resolvedParagraph, chapterParagraphs []store.Paragraph) {
	var group []store.Paragraph
	if chapterParagraphs != nil {
		group = scareQuoteRun(inlineParagraphSetIn(chapterParagraphs, rp.paragraph), rp.paragraph)
	} else {
		group = m.scareQuoteMergeGroup(chapterID, rp.paragraph)
	}
	if len(group) > 1 {
		m.pushMergedTask(bookID, chapterID, chapterIdx, tier, rp.voice, group)
		return
	}
	m.pushSingleTask(bookID, chapterID, chapterIdx, tier, rp)
}

func (m *Manager) pushSingleTask(bookID, chapterID string, chapterIdx, tier int, rp resolvedParagraph) {
	if err := audiopath.EnsureVoiceDir(m.dataDir, bookID, chapterID, rp.voice.VoiceID()); err != nil {
		log.Printf("jobs: create audio dir: %v", err)
		return
	}
	kind := KindVoiceClone
	if rp.voice.PresetID == "" {
		kind = KindVoiceDesign
	}
	m.pushTask(&task{
		kind:            kind,
		chapterIdx:      chapterIdx,
		tier:            tier,
		bookID:          bookID,
		chapterID:       chapterID,
		voiceID:         rp.voice.VoiceID(),
		paragraph:       rp.paragraph,
		instruct:        rp.voice.Instruct,
		presetID:        rp.voice.PresetID,
		language:        rp.voice.Language,
		refText:         rp.voice.RefText,
		speedMultiplier: rp.voice.SpeedMultiplier,
		seed:            rp.voice.Seed,
		cloneModel:      rp.voice.CloneModel,
		designModel:     rp.voice.DesignModel,
		cloneInstruct:   rp.voice.CloneInstruct,
	})
}

// pushMergedTask is pushSingleTask's own counterpart for a scare-quote
// merge group (see scareQuoteMergeGroup) - one task covering every member
// in members, keyed (via task.dedupKey, which reads t.paragraph) off
// members[0] specifically so a second call landing on any other member of
// the same group (the common case: enqueueChapter's loop reaches every
// not-yet-ready member in turn) correctly dedups against the first rather
// than queuing a second, competing task for the same group.
func (m *Manager) pushMergedTask(bookID, chapterID string, chapterIdx, tier int, voice narration.ResolvedVoice, members []store.Paragraph) {
	if err := audiopath.EnsureVoiceDir(m.dataDir, bookID, chapterID, voice.VoiceID()); err != nil {
		log.Printf("jobs: create audio dir: %v", err)
		return
	}
	kind := KindVoiceClone
	if voice.PresetID == "" {
		kind = KindVoiceDesign
	}
	m.pushTask(&task{
		kind:            kind,
		chapterIdx:      chapterIdx,
		tier:            tier,
		bookID:          bookID,
		chapterID:       chapterID,
		voiceID:         voice.VoiceID(),
		paragraph:       members[0],
		mergeParagraphs: members,
		instruct:        voice.Instruct,
		presetID:        voice.PresetID,
		language:        voice.Language,
		refText:         voice.RefText,
		speedMultiplier: voice.SpeedMultiplier,
		seed:            voice.Seed,
		cloneModel:      voice.CloneModel,
		designModel:     voice.DesignModel,
		cloneInstruct:   voice.CloneInstruct,
	})
}

// InlineParagraphSet returns the full contiguous run of chapterID's
// paragraphs (in Idx order) that paragraph belongs to: paragraph itself,
// plus every paragraph reachable from it by following Inline in either
// direction (the same run internal/epub.splitQuoteSegments originally
// split out of one source paragraph - see store.Paragraph.Inline's own
// doc comment). Unlike scareQuoteMergeGroup below (which narrows this
// down to only when the run is actually eligible for scare-quote
// merging), this applies no ScareQuote/Speaker filtering at all - it's
// httpapi's own tool for invalidating every paragraph whose merge
// eligibility could change together whenever any one member's ScareQuote
// flag changes, not just to decide whether to merge them for generation.
//
// Returns []store.Paragraph{paragraph} for a paragraph with no Inline
// neighbors on either side (the overwhelming common case), and on any
// lookup error (logged, not returned) or an unexpected gap in an
// otherwise-contiguous Idx run (see store.Store's own "Idx is dense per
// chapter" invariant - should never happen).
//
// Loads chapterID's whole paragraph list; inlineParagraphSetIn is the
// same over a list the caller already has.
func (m *Manager) InlineParagraphSet(chapterID string, paragraph store.Paragraph) []store.Paragraph {
	all, err := m.store.ListParagraphsRaw(chapterID)
	if err != nil {
		log.Printf("jobs: inline paragraph set lookup for chapter %s: %v", chapterID, err)
		return []store.Paragraph{paragraph}
	}
	return inlineParagraphSetIn(all, paragraph)
}

// inlineParagraphSetIn is InlineParagraphSet over all, the chapter's
// already-loaded paragraphs.
func inlineParagraphSetIn(all []store.Paragraph, paragraph store.Paragraph) []store.Paragraph {
	alone := []store.Paragraph{paragraph}
	byIdx := make(map[int]store.Paragraph, len(all))
	for _, p := range all {
		byIdx[p.Idx] = p
	}
	// paragraph itself may be a caller's own newer copy (just reset for
	// regenerate, or just flipped ScareQuote) than whatever
	// ListParagraphsRaw just read back - never let a stale re-read of
	// this one paragraph override the value the caller actually asked
	// to dispatch/invalidate.
	byIdx[paragraph.Idx] = paragraph

	start, end := paragraph.Idx, paragraph.Idx
	for {
		cur, ok := byIdx[start]
		if !ok || !cur.Inline {
			break
		}
		start--
	}
	for {
		next, ok := byIdx[end+1]
		if !ok || !next.Inline {
			break
		}
		end++
	}
	if start == end {
		return alone
	}

	group := make([]store.Paragraph, 0, end-start+1)
	for idx := start; idx <= end; idx++ {
		p, ok := byIdx[idx]
		if !ok {
			return alone
		}
		group = append(group, p)
	}
	return group
}

// sentenceTerminators are the trailing characters that end a segment's own
// sentence for scareQuoteMergeGroup's boundary rule below. Only .!?
// actually end a sentence: a colon/semicolon
// reads as part of the same continuous utterance ("He was blunt: 'Get
// out.'" merges into one clip), and an em dash/ellipsis usually signals an
// interrupted or trailing-off thought that keeps going rather than a hard
// stop - so ,;:—… all leave a boundary eligible to merge, only .!? close
// it off.
const sentenceTerminators = ".!?"

// segmentEndsSentence reports whether p's own text - the text just inside
// its closing quote mark, for a quote segment (store.Paragraph.IsQuote),
// or its own text directly otherwise - ends with one of
// sentenceTerminators - see sentenceTerminators' own doc comment.
func segmentEndsSentence(p store.Paragraph) bool {
	text := p.Text
	if p.IsQuote {
		runes := []rune(text)
		if len(runes) < 2 {
			return false
		}
		text = string(runes[:len(runes)-1])
	}
	trimmed := strings.TrimRightFunc(text, unicode.IsSpace)
	if trimmed == "" {
		return false
	}
	runes := []rune(trimmed)
	return strings.ContainsRune(sentenceTerminators, runes[len(runes)-1])
}

// scareQuoteMergeGroup narrows InlineParagraphSet down to just the
// contiguous run around paragraph that's actually eligible to merge into
// one narrator-voiced TTS call - not necessarily paragraph's *whole*
// inline chain (see InlineParagraphSet's own doc comment for what that
// covers). paragraph itself must be narration-voiced (narrationVoiced
// below); the run then grows outward from it, one boundary at a time,
// only as far as two conditions both hold at each step: the neighbor being
// pulled in is itself narration-voiced (never genuine, still-spoken
// dialogue - see narrationVoiced's own comment for why this checks
// IsQuote/ScareQuote directly, not just Speaker), and the boundary itself
// reads as the same sentence continuing rather than a fresh one starting -
// segmentEndsSentence on whichever segment sits on the earlier side of
// that boundary. A boundary merges segment i into segment i+1 exactly when
// segment i's own text doesn't end its sentence - symmetric and
// independent per boundary, so a scare quote naturally merges left only,
// right only, both, or neither depending purely on its own trailing
// punctuation and its immediate narration neighbors' own trailing
// punctuation, never on some other segment's ScareQuote status several
// paragraphs away in the same original source paragraph's inline chain.
//
// Deliberately narrower than an earlier, all-or-nothing version of this
// function (which returned paragraph's *entire* inline chain, or nothing
// at all, keyed on whether every member anywhere in that whole chain was
// narration-voiced): a single genuine line of dialogue or named-speaker
// paragraph anywhere in a long inline chain used to block merging for
// every scare quote in that chain, even ones nowhere near it. Now it's
// only ever a wall the walk below stops at, not a reason to refuse merging
// altogether - another scare quote in the same chain, on the far side of
// that wall from paragraph, still merges on its own once generation
// reaches it as its own anchor.
//
// Returns []store.Paragraph{paragraph} (no merge - callers treat len == 1
// as "generate paragraph alone, exactly as before this feature existed")
// whenever paragraph itself isn't narration-voiced, the resolved run has
// no scare-quoted paragraph in it, or every adjacent boundary reads as its
// own separate sentence.
func (m *Manager) scareQuoteMergeGroup(chapterID string, paragraph store.Paragraph) []store.Paragraph {
	return scareQuoteRun(m.InlineParagraphSet(chapterID, paragraph), paragraph)
}

// scareQuoteRun is scareQuoteMergeGroup given paragraph's inline set
// (InlineParagraphSet/inlineParagraphSetIn) directly.
func scareQuoteRun(group []store.Paragraph, paragraph store.Paragraph) []store.Paragraph {
	alone := []store.Paragraph{paragraph}
	if len(group) <= 1 {
		return alone
	}

	anchor := -1
	for i, p := range group {
		if p.ID == paragraph.ID {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return alone
	}

	// narrationVoiced reports whether p can share the narrator's own TTS
	// call at all - deliberately on IsQuote/ScareQuote directly, not
	// (only) on Speaker: a quote's Speaker only reliably reads a real
	// character name/"Unknown" *after* attribution has actually run over
	// it (disallowNarratorForDialogue enforces that a genuine quote is
	// never left "Narrator") - before that, an unattributed real quote
	// sits at Speaker == "" indistinguishably from plain narration, and
	// manually toggling a paragraph's own ScareQuote flag
	// (handleSetParagraphScareQuote) needs no attribution to have
	// happened first at all. Without this, a reader flagging one segment
	// of an unattributed inline run as a scare quote could pull a
	// genuine, not-yet-(or never-)attributed line of someone else's
	// actual dialogue into the same merged, narrator-voiced clip -
	// permanently baking a real character's line into the wrong voice,
	// since a merged clip is a single continuous recording with no seam
	// to later re-cut once attribution catches up. Once IsQuote/
	// ScareQuote rule that out, the remaining Speaker check still matters
	// on its own (a Speaker can diverge from IsQuote/ScareQuote in the
	// other direction too - a stray non-Narrator attribution surviving on
	// a paragraph that's since been re-tagged): every merged member
	// confirmed Narrator/"" is what lets pushMergedTask's caller skip
	// resolving each member's own voice separately - internal/narration.
	// Resolver only ever overrides a paragraph's voice away from the
	// book's own for a named-character Speaker, so once every member is
	// confirmed Narrator/"", rp's own already-resolved voice is
	// guaranteed correct for the whole group without a second lookup.
	narrationVoiced := func(p store.Paragraph) bool {
		if p.IsQuote && !p.ScareQuote {
			return false
		}
		if p.Speaker != "" && p.Speaker != "Narrator" {
			return false
		}
		return true
	}
	if !narrationVoiced(group[anchor]) {
		return alone
	}

	lo, hi := anchor, anchor
	for lo > 0 && narrationVoiced(group[lo-1]) && !segmentEndsSentence(group[lo-1]) {
		lo--
	}
	for hi < len(group)-1 && narrationVoiced(group[hi+1]) && !segmentEndsSentence(group[hi]) {
		hi++
	}
	if lo == hi {
		return alone
	}

	run := group[lo : hi+1]
	hasScareQuote := false
	for _, p := range run {
		if p.ScareQuote {
			hasScareQuote = true
			break
		}
	}
	if !hasScareQuote {
		return alone
	}
	return run
}

func (m *Manager) enqueueChapter(ctx context.Context, bookID, chapterID string, chapterIdx, wordLimit int) {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		log.Printf("jobs: book %s not found: %v", bookID, err)
		return
	}
	m.maybeRunChapterAmbience(bookID, chapterID, chapterIdx, book)
	resolved, all, err := m.paragraphsNeedingGeneration(ctx, book, chapterID)
	if err != nil {
		log.Printf("jobs: list paragraphs for chapter %s: %v", chapterID, err)
		return
	}
	if wordLimit > 0 {
		resolved = limitResolvedByWordCount(resolved, wordLimit)
	}
	for _, rp := range resolved {
		m.pushResolvedTask(bookID, chapterID, chapterIdx, TierBackground, rp, all)
	}
}

// maybeRunChapterAmbience fire-and-forget dispatches background-music
// scoring for chapterID (EnqueueAmbience, via m.runAmbience - a no-op
// if never configured, see AmbienceRunner's own doc comment) the moment its
// own narration audio generation is requested, rather than requiring a
// reader to separately visit the Speakers page and score it (or run a
// bulk "Score all music") before its music actually ever generates. A
// no-op for a book with MusicEnabled off (the overwhelming common case -
// same cheap-to-over-call reasoning MaybeAdvanceChapterMusic's own doc
// comment gives) or a chapter already done (ch.Passes.AmbienceDone()) -
// scoring only ever needs to run once per chapter; MaybeAdvanceChapterMusic
// (called from inside the real scorer once regions persist, and again as
// each narration paragraph this same enqueueChapter call is about to
// dispatch finishes) is what actually advances already-scored regions,
// completely independent of this. EnqueueAmbience's own dedup-by-
// chapter means calling this on every enqueueChapter (including "Generate
// remaining"'s whole-book walk, and a reader simply re-opening a chapter
// they're already generating) is harmless even for a chapter whose
// scoring is already queued or in flight.
func (m *Manager) maybeRunChapterAmbience(bookID, chapterID string, chapterIdx int, book *store.Book) {
	if !book.MusicEnabled || m.runAmbience == nil {
		return
	}
	ch, err := m.store.GetChapterByID(chapterID)
	if err != nil || ch == nil || ch.Passes.AmbienceDone() {
		return
	}
	chVal := *ch
	m.EnqueueAmbience(bookID, chapterID, chapterIdx, func(ctx context.Context) (int, func(), error) {
		return m.runAmbience(ctx, book, &chVal)
	})
}

// EnqueueParagraphRegenerate re-renders one already-generated paragraph
// from scratch, for an explicit "regenerate this paragraph" request (the
// reader didn't like how a specific line came out) rather than the normal
// pending -> generating -> ready flow. Runs at TierUrgent - a single
// explicit, deliberate request, not just "this is coming up soon" - so it
// always goes next regardless of where it sits relative to the reader's
// actual current position.
func (m *Manager) EnqueueParagraphRegenerate(bookID, chapterID string, chapterIdx int, paragraph store.Paragraph) {
	go m.enqueueParagraphRegenerate(bookID, chapterID, chapterIdx, paragraph)
}

func (m *Manager) enqueueParagraphRegenerate(bookID, chapterID string, chapterIdx int, paragraph store.Paragraph) {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		log.Printf("jobs: book %s not found: %v", bookID, err)
		return
	}
	if bookVoice, err := m.narration.BookVoice(book); err != nil {
		log.Printf("jobs: resolve book voice for %s: %v", bookID, err)
	} else {
		m.provisionMissingCharacterVoices(m.ctx, book, []string{paragraph.Speaker}, narration.EffectiveCloneModel(bookVoice))
	}
	v, err := m.narration.ForParagraph(book, paragraph.Speaker, paragraph.SpeakerIndex)
	if err != nil {
		log.Printf("jobs: resolve voice for paragraph %s: %v", paragraph.ID, err)
		return
	}
	voiceID := v.VoiceID()

	if err := m.store.ResetParagraphAudio(paragraph.ID, voiceID); err != nil {
		log.Printf("jobs: reset paragraph %s for regenerate: %v", paragraph.ID, err)
		return
	}
	m.pushResolvedTask(bookID, chapterID, chapterIdx, TierUrgent, resolvedParagraph{paragraph: paragraph, voice: v}, nil)
}

// EnqueueLookahead generates up to count upcoming paragraphs (count <= 0
// means LookaheadParagraphCount; anything above MaxLookaheadParagraphCount
// is clamped to it) starting at (fromChapterIdx, fromParagraphIdx), continuing
// into later chapters as needed instead of stopping at the current
// chapter's end - so there's always some runway ahead regardless of where
// a chapter boundary falls relative to the reader's position. Paragraphs
// before fromParagraphIdx in the starting chapter are left alone (no point
// generating audio for ones already played past).
//
// The first LookaheadUrgentParagraphCount paragraph positions from
// (fromChapterIdx, fromParagraphIdx) - counted by position, generated or
// not, and crossing chapter boundaries - are pushed at TierUrgent rather
// than TierLookahead if they need generation: this is also the request the
// reader/mobile client fires the moment they select a not-yet-generated
// paragraph (see httpapi.handleLookahead), so "jump to this paragraph"
// should generate it and the few right after it before any other lookahead
// work. Every other paragraph in the window stays at TierLookahead, ordered
// among themselves like any other same-tier poolGeneration work - by plain
// book/chapter/paragraph position (see lessByPosition), not by distance
// from wherever the reader happens to be right now.
func (m *Manager) EnqueueLookahead(bookID string, fromChapterIdx, fromParagraphIdx, count int) {
	if count <= 0 {
		count = LookaheadParagraphCount
	}
	count = min(count, MaxLookaheadParagraphCount)
	req := lookaheadRequest{fromChapterIdx, fromParagraphIdx, count}

	// Readers post a lookahead every few seconds and one run can take
	// longer than that (it reads and resolves whole chapters), so starting
	// a goroutine per request piled up concurrent runs all queued on the
	// store's single DB connection, stalling everything else. Only the
	// newest position matters, so a request arriving while this book's run
	// is busy just replaces the pending one.
	m.lookaheadMu.Lock()
	if run, ok := m.lookaheads[bookID]; ok {
		run.pending = &req
		m.lookaheadMu.Unlock()
		return
	}
	m.lookaheads[bookID] = &lookaheadRun{}
	m.lookaheadMu.Unlock()

	go func() {
		for {
			m.enqueueLookahead(bookID, req.fromChapterIdx, req.fromParagraphIdx, req.count)
			m.lookaheadMu.Lock()
			run := m.lookaheads[bookID]
			if run.pending == nil {
				delete(m.lookaheads, bookID)
				m.lookaheadMu.Unlock()
				return
			}
			req = *run.pending
			run.pending = nil
			m.lookaheadMu.Unlock()
		}
	}()
}

type lookaheadRequest struct {
	fromChapterIdx, fromParagraphIdx, count int
}

// lookaheadRun is one book's in-progress EnqueueLookahead; pending is the
// newest request that arrived while it ran, if any.
type lookaheadRun struct {
	pending *lookaheadRequest
}

func (m *Manager) enqueueLookahead(bookID string, fromChapterIdx, fromParagraphIdx, count int) {
	start := time.Now()
	defer func() {
		log.Printf("jobs: timing lookahead book=%s from=%d/%d count=%d took %s", bookID, fromChapterIdx, fromParagraphIdx, count, time.Since(start).Round(time.Millisecond))
	}()
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		log.Printf("jobs: book %s not found: %v", bookID, err)
		return
	}

	chapterIdx := fromChapterIdx
	remaining := count
	// Positions still inside the urgent window at the start of the
	// current chapter's unplayed paragraphs.
	urgentLeft := LookaheadUrgentParagraphCount
	for remaining > 0 {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		ch, err := m.store.GetChapterByIdx(bookID, chapterIdx)
		if err != nil {
			log.Printf("jobs: lookahead: get chapter %d of book %s: %v", chapterIdx, bookID, err)
			return
		}
		if ch == nil {
			return // ran off the end of the book
		}

		resolved, all, err := m.paragraphsNeedingGeneration(m.ctx, book, ch.ID)
		if err != nil {
			log.Printf("jobs: list paragraphs for chapter %s: %v", ch.ID, err)
			return
		}
		if chapterIdx == fromChapterIdx && fromParagraphIdx > 0 {
			resolved = skipResolvedBeforeIdx(resolved, fromParagraphIdx)
		}
		if len(resolved) > remaining {
			resolved = resolved[:remaining]
		}
		firstIdx := 0
		if chapterIdx == fromChapterIdx {
			firstIdx = fromParagraphIdx
		}
		for _, rp := range resolved {
			tier := TierLookahead
			if rp.paragraph.Idx-firstIdx < urgentLeft {
				tier = TierUrgent
			}
			m.pushResolvedTask(bookID, ch.ID, chapterIdx, tier, rp, all)
		}
		for _, p := range all {
			if p.Idx >= firstIdx {
				urgentLeft--
			}
		}
		if chapterIdx == fromChapterIdx {
			// Background music should be at least as responsive to reader
			// position as narration itself - a still-queued scoring/live
			// generation task for this chapter shouldn't be stuck waiting
			// out an unrelated TierBackground backlog (e.g. a whole-book
			// direction-tagging run) the way it otherwise would. Only the
			// reader's own current chapter, not the whole lookahead window
			// - see PromoteChapterMusicNear's own doc comment for why
			// that's enough.
			m.PromoteChapterMusicNear(bookID, ch.ID)
			// And start the music right where they are, without waiting
			// for the rest of the chapter to be voiced - see
			// KindMusicLiveGeneration. Live path only: the whole-chapter
			// batch stays TierBackground and is only ever kicked off by
			// MaybeAdvanceChapterMusic, never by reader position.
			m.advanceLiveChapterMusic(bookID, ch.ID, fromParagraphIdx)
		}
		remaining -= len(resolved)
		chapterIdx++
	}
}

// PromoteChapterMusicNear bumps to TierUrgent whichever still-queued
// background-music task (if any) is actually standing between the reader
// and having music in chapterID: a KindAmbience task for the chapter
// itself, if it hasn't been scored yet, and/or a KindMusicLiveGeneration
// task for the chapter - both keyed purely by chapterID. The whole-chapter
// KindMusicGeneration batch is deliberately left alone: reader position
// only drives the live path (the batch covers regions the reader may be
// nowhere near, and would otherwise hog poolSFX at urgent priority). A
// no-op, not an error, if neither exists (already dispatched/finished, not
// created yet, or the book doesn't have music on at all) - same shape as
// PromoteTier's own WithTask calls, just driven by reader position instead
// of an explicit dashboard action.
//
// Called from enqueueLookahead for the reader's own exact current
// position (not the whole lookahead window - a region generally spans far
// more than LookaheadParagraphCount paragraphs, and every position update
// re-triggers this anyway, so this chapter alone is enough to keep pace
// as they keep reading) - background music otherwise defaults to
// TierBackground end to end (EnqueueAmbience's own
// defaultAttributionTier, EnqueueMusicGeneration's own TierBackground -
// see MaybeAdvanceChapterMusic), since nothing about scoring/generating it
// is reader-blocking on its own the way an unready narration paragraph is.
func (m *Manager) PromoteChapterMusicNear(bookID, chapterID string) {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil || !book.MusicEnabled {
		return
	}
	promote := func(id string) {
		m.queue.WithTask(
			func(c taskqueue.Task) bool { return c.Key() == id },
			func(c taskqueue.Task) {
				if TierUrgent < c.Tier() {
					c.Promote(TierUrgent)
				}
			},
		)
	}
	promote("ambience:" + chapterID)
	m.queue.WithEachTask(
		func(c taskqueue.Task) bool { return isLiveMusicTask(c, chapterID) },
		func(c taskqueue.Task) {
			if TierUrgent < c.Tier() {
				c.Promote(TierUrgent)
			}
		},
	)
	// Unconditional, same "cheap to over-call" reasoning
	// MaybeAdvanceChapterMusic's own doc comment gives - simpler than
	// threading back whether either promote() call actually found and
	// promoted something, and the Jobs dashboard's own poll absorbs a
	// harmless extra notify fine.
	m.notifyChanged()
}

// EnqueueRemaining and EnqueueBookGenerate now live in pipeline.go, as
// cancelable bulk groups (EnqueueRemaining/EnqueueBookGenerate) - see their
// own doc comments there.

// pushTask registers a task as queued (skipping it if already
// queued/in-flight under the same dedupKey, to avoid dispatching the same
// paragraph - or the same chapter's attribution, or the same character's
// characterization - twice when overlapping calls both see it as pending)
// and adds it to the priority queue. A duplicate push still merges its
// llmWaiters (if any - only RunCharacterization populates them, see
// llmWaiters' own doc comment) onto whichever existing task owns key, in
// flight or still queued, so a second RunCharacterization call for an
// already-running character joins that run's result instead of being
// dropped with no way to ever complete, and bumps that existing task's own
// tier up in place if t's is more urgent - a whole chapter's worth of
// TierBackground work is a common case an explicit "play this paragraph
// now" click can land in the middle of, and without this, the dedup below
// would just silently drop that click's TierLookahead push, since the
// paragraph already has *a* queue entry, just the wrong-priority one. A
// duplicate EnqueueAttribution push for an already-queued/in-flight
// chapter has no waiter to merge and is already at defaultAttributionTier
// either way - it's just silently deduped, which is fine since nothing is
// blocked waiting on it anyway.
//
// For KindVoiceClone/KindVoiceDesign specifically (dedupKey's default
// case, keyed by paragraph ID alone - see its own doc comment), a merge
// also refreshes the existing queued task's voice-defining fields
// (voiceID/instruct/presetID/etc., even kind itself) from t's freshly
// re-resolved ones, via the narrower WithQueuedTask rather than WithTask.
// Without this, a paragraph queued once (e.g. by lookahead) and then
// re-pushed after something changes which voice it actually resolves to
// (speaker attribution reassigning it to a character, a character's own
// voice reassignment, book/MultiVoice changes) would still generate under
// whatever voice it first queued under: the merge branch used to touch
// only waiters/tier, silently keeping the stale voice fields, so the
// dispatched render came out under the wrong voice and paragraphsNeeding
// Generation - checking the *current* resolution - reported it not ready
// and queued yet another, correct render right behind it. From a reader's
// perspective that showed up as a paragraph that had just finished
// generating flipping back to pending and re-rendering. Deliberately
// restricted to a still-queued match (never in-flight): an in-flight
// task's own generate() goroutine reads these same fields with no lock of
// its own, on the assumption nothing mutates them after dispatch, so
// there's nothing to correct once a render is already underway - the
// eventual mismatch there still self-heals the same way it always did,
// via the next check finding it not-ready under the current voice.
func (m *Manager) pushTask(t *task) {
	t.pushedAt = time.Now()
	if !m.queue.Push(t) {
		// Via WithTask, not a plain Find + direct field mutation: this
		// task's own tier/chapterIdx/waiters are read concurrently by
		// taskqueue itself (Tier/Pool/Less, called from inside Pop/
		// Snapshot under its own lock) - mutating them after Find already
		// returned and released that lock would race against those reads.
		m.queue.WithTask(func(c taskqueue.Task) bool { return c.Key() == t.Key() }, func(c taskqueue.Task) {
			et := c.(*task)
			et.llmWaiters = append(et.llmWaiters, t.llmWaiters...)
			et.provisionWaiters = append(et.provisionWaiters, t.provisionWaiters...)
			et.previewWaiters = append(et.previewWaiters, t.previewWaiters...)
			et.previewTextWaiters = append(et.previewTextWaiters, t.previewTextWaiters...)
			if t.tier < et.tier {
				et.tier = t.tier
				et.chapterIdx = t.chapterIdx
			}
		})
		if t.kind == KindVoiceClone || t.kind == KindVoiceDesign {
			m.queue.WithQueuedTask(func(c taskqueue.Task) bool { return c.Key() == t.Key() }, func(c taskqueue.Task) {
				et := c.(*task)
				et.kind = t.kind
				et.voiceID = t.voiceID
				et.instruct = t.instruct
				et.presetID = t.presetID
				et.language = t.language
				et.refText = t.refText
				et.speedMultiplier = t.speedMultiplier
				et.seed = t.seed
				et.cloneModel = t.cloneModel
				et.designModel = t.designModel
				et.cloneInstruct = t.cloneInstruct
			})
		}
		m.notifyChanged()
		return
	}
	m.notePushed(t)
	select {
	case m.wake <- struct{}{}:
	default:
	}
	m.notifyChanged()
}

// notePushed increments m.chapterPending for a freshly (not a dedup
// merge), successfully queued t - shared by pushTask itself and this
// package's own dependency resolvers below, whose own lazily-created
// dependency tasks go straight through taskqueue.LockedQueue.Push (from
// inside a taskqueue.Resolver call) rather than pushTask, but still need
// the exact same bookkeeping so IsGenerating's own count includes a
// chapter's auto-created reference-clip/characterization/direction-
// tagging dependency the same as anything explicitly enqueued.
// finishTask decrements it uniformly for any task's completion regardless
// of which of these created it.
func (m *Manager) notePushed(t *task) {
	m.mu.Lock()
	m.chapterPending[t.chapterID]++
	m.mu.Unlock()
}

// resolveDependencies is this package's taskqueue.Resolver - see that
// type's own doc comment for the general contract, and Kind's own doc
// comment ("adding a new kind of job means extending those switches") for
// what this replaces: three near-identical, hand-copied functions
// (characterizationBlockerFor/speechDirectionBlockerFor/
// referenceClipBlockerFor, before this package existed), each separately
// implementing the same "look for a queued/in-flight match, else check
// whether the real, persisted state this dependency represents is already
// done, else lazily create one" shape - and each one a place that check
// could be (and, in this app's own history, was) forgotten, recreating a
// "finished" dependency's task forever. Now there's one entry point, and
// each dependency kind below is just the business-specific part of that
// shape (what counts as "done", what the fresh task looks like) with the
// generic queueing/dedup mechanics handled once, by taskqueue itself.
func (m *Manager) resolveDependencies(lq *taskqueue.LockedQueue, tk taskqueue.Task) []taskqueue.Task {
	t := tk.(*task)
	if t.skipDependencies {
		return nil
	}
	var deps []taskqueue.Task
	if d := m.characterizationDependency(lq, t); d != nil {
		deps = append(deps, d)
	}
	if t.kind == KindVoiceClone || t.kind == KindVoiceDesign {
		if d := m.speechDirectionDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
		if d := m.pronunciationDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	if t.kind == KindVoiceClone {
		if d := m.referenceClipDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
		if d := m.variantClipDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	if t.kind == KindSpeakerAttribution || t.kind == KindDescription {
		if d := m.scareQuoteDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	if t.kind == KindSpeakerAttribution || t.kind == KindAmbience {
		if d := m.summaryDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	if t.kind == KindAmbience || t.kind == KindSummary || t.kind == KindSpeakerCharacterization || t.kind == KindSpeakerAttribution {
		if d := m.bookSettingDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	if t.kind == KindMusicLiveGeneration {
		if d := m.liveMusicDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	if t.kind == KindSpeakerAttribution {
		if d := m.attributionOrderDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
		if d := m.globalAttributionSlotDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	return deps
}

// pushDependency pushes nt (a freshly-built, not-yet-queued dependency
// task) via lq, falling back to whatever's already there under the same
// key on the rare chance something else in this same resolveDependencies
// pass already created it first (see taskqueue.Resolver's own doc comment
// on why lq.Find is checked before this in every caller below - this is
// just the defensive "and if that raced anyway" fallback, not the
// primary dedup path) - shared by every dependency kind below instead of
// each hand-rolling the same fallback.
func (m *Manager) pushDependency(lq *taskqueue.LockedQueue, nt *task) taskqueue.Task {
	nt.pushedAt = time.Now()
	if lq.Push(nt) {
		m.notePushed(nt)
		return nt
	}
	if existing, ok := lq.Get(nt.Key()); ok {
		return existing
	}
	return nil
}

// skipResolvedBeforeIdx drops paragraphs before fromIdx. resolved is
// already ordered by Idx ascending (paragraphsNeedingGeneration), so this
// is just finding the first one at or past fromIdx.
func skipResolvedBeforeIdx(resolved []resolvedParagraph, fromIdx int) []resolvedParagraph {
	for i, rp := range resolved {
		if rp.paragraph.Idx >= fromIdx {
			return resolved[i:]
		}
	}
	return nil
}

// limitResolvedByWordCount returns a prefix of resolved covering at least
// wordLimit words, including whichever whole paragraph crosses that
// threshold (never cuts a paragraph short - generation is per-paragraph).
func limitResolvedByWordCount(resolved []resolvedParagraph, wordLimit int) []resolvedParagraph {
	total := 0
	for i, rp := range resolved {
		total += len(strings.Fields(rp.paragraph.Text))
		if total >= wordLimit {
			return resolved[:i+1]
		}
	}
	return resolved
}
