package jobs

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"sync"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/musicgen"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/taskqueue"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// EnqueueMusicGeneration queues fn as a KindMusicGeneration task - render
// and persist every one of a chapter's currently-eligible background-music
// regions in one task (see generateChapterMusicBatch). Its only caller is
// advanceWholeChapterMusic, once the whole chapter is voiced and it's found
// at least one pending region that (for a "continuation" transition)
// whose immediately preceding region has already finished or will finish
// earlier in this same batch - see MusicRegion's own doc comment for the
// full readiness contract. TierBackground, not TierUrgent the way
// EnqueueSFXGeneration's own explicit reader click is - see
// KindMusicGeneration's own doc comment for why. chapterID is this task's
// own dedup key suffix (see task.dedupKey) - a chapter can only usefully
// have one music-generation batch in flight at a time, the same "one at a
// time per chapter" shape KindAmbience already has.
func (m *Manager) EnqueueMusicGeneration(bookID, chapterID string, chapterIdx int, fn func(ctx context.Context, attempt int) error) {
	m.enqueueMusicGeneration(bookID, chapterID, chapterIdx, TierBackground, fn)
}

func (m *Manager) enqueueMusicGeneration(bookID, chapterID string, chapterIdx, tier int, fn func(ctx context.Context, attempt int) error) {
	m.pushTask(&task{
		kind:        KindMusicGeneration,
		tier:        tier,
		bookID:      bookID,
		chapterID:   chapterID,
		chapterIdx:  chapterIdx,
		llmKey:      chapterID,
		runMusicGen: fn,
	})
}

// pendingMusicRegion is one region MaybeAdvanceChapterMusic found eligible
// to generate right now, plus the target duration it computed for it (the
// sum of that region's own paragraphs' real narration durations) - carried
// from the scan into generateChapterMusicBatch so the batch task itself
// never needs to re-derive it.
type pendingMusicRegion struct {
	region         store.MusicRegion
	targetDuration float64
}

// MaybeAdvanceChapterMusic checks chapterID's own music regions (if
// bookID's own store.Book.MusicEnabled - background music is a book-wide
// toggle, not a per-chapter one, unlike scoring itself, which stays
// chapter-scoped) and dispatches whatever is eligible to generate, along
// two independent paths:
//
//   - Whole chapter (advanceWholeChapterMusic, KindMusicGeneration,
//     TierBackground): once every paragraph in the chapter has ready
//     narration, every eligible region goes out as one batch.
//   - Live (advanceLiveMusic, KindMusicLiveGeneration, TierUrgent): when
//     the reader's position is in this chapter, regions go out one task
//     each, chained forward from the one they're in, as soon as each
//     region's own paragraphs are voiced, so music starts while the rest
//     of the chapter is still being narrated.
//
// Both honor store.MusicRegion's own continuation contract: a
// MusicTransitionContinuation region's immediately preceding region must
// already have settled (AudioReady or AudioError) or be earlier in the
// same batch, so its clip exists to seed this one's first chunk from
// (advanceLiveMusic's own doc comment has its one exception). Both can
// reach the same region, so generateChapterMusicBatch claims each region
// in memory before generating it (claimMusicRegion) - a region is only ever
// generated once, and a batch that finds its next region claimed by the
// other path stops there and is picked up again when that task finishes.
//
// Unlike the LLM kinds (attribution/direction/music-scoring), the actual
// generation call needs nothing httpapi doesn't already give this package
// directly - m.tts satisfies musicgen.Backend, and m.store/m.dataDir are
// this package's own fields - so this builds and dispatches the whole
// closure itself with no caller-supplied function required.
//
// Called from: httpapi.ambienceChapter, after each batch of freshly-
// scored regions persists (a chapter already fully narrated before music
// was turned on has no future paragraph-completion event to kick it off);
// saveParagraphAudio/handleMergedResult, once a paragraph finishes
// narrating, in case it was the last one some region needed; handleResult,
// once either music kind's batch finishes, to advance past it; and
// enqueueLookahead (via advanceLiveChapterMusic, live path only, with the
// reader's exact position). Cheap to over-call: a book with MusicEnabled false (the
// overwhelming common case) costs one row lookup and returns immediately,
// and both kinds' own per-chapter dedup keys mean a chapter is never
// queued twice for the same path.
func (m *Manager) MaybeAdvanceChapterMusic(bookID, chapterID string) {
	st, err := m.loadChapterMusic(bookID, chapterID)
	if err != nil || st == nil || !st.book.MusicEnabled || len(st.regions) == 0 {
		return
	}
	// Live path follows the book's saved position, if it's in this chapter.
	if st.book.PosChapterIdx == st.ch.Idx {
		m.advanceLiveMusic(bookID, chapterID, st.ch.Idx, st.regions, st.book.PosParagraphIdx, st.regionDuration, liveChainCarry{})
	}
	if st.allReady {
		m.advanceWholeChapterMusic(bookID, chapterID, st.ch.Idx, st.regions, st.regionDuration, TierBackground)
	}
}

// advanceLiveChapterMusic advances only the live music path from livePos,
// the paragraph idx the reader is at right now in chapterID
// (enqueueLookahead, which knows it before the book's saved position
// catches up). Never touches the whole-chapter batch - see
// enqueueLookahead.
func (m *Manager) advanceLiveChapterMusic(bookID, chapterID string, livePos int) {
	st, err := m.loadChapterMusic(bookID, chapterID)
	if err != nil || st == nil || !st.book.MusicEnabled || len(st.regions) == 0 {
		return
	}
	m.advanceLiveMusic(bookID, chapterID, st.ch.Idx, st.regions, livePos, st.regionDuration, liveChainCarry{})
}

// chapterMusicState is everything both music paths need to decide what's
// eligible in one chapter - see loadChapterMusic.
type chapterMusicState struct {
	book    *store.Book
	ch      *store.Chapter
	regions []store.MusicRegion
	// allReady reports whether every paragraph in the chapter has ready
	// narration - the whole-chapter path's own gate.
	allReady bool
	// regionDuration is a region's narration length, or ok=false if any of
	// its paragraphs isn't voiced yet (or it points past the chapter's own
	// paragraphs - stale scoring).
	regionDuration func(store.MusicRegion) (float64, bool)
}

// loadChapterMusic gathers chapterMusicState for chapterID, or nil if the
// book/chapter doesn't exist. Doesn't check store.Book.MusicEnabled -
// the automatic path does, the explicit GenerateChapterMusic doesn't.
func (m *Manager) loadChapterMusic(bookID, chapterID string) (*chapterMusicState, error) {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		return nil, err
	}
	ch, err := m.store.GetChapterByID(chapterID)
	if err != nil || ch == nil {
		return nil, err
	}
	regions, err := m.store.ListMusicRegions(chapterID)
	if err != nil {
		return nil, err
	}
	if len(regions) == 0 {
		// Not scored - nothing either path could do, so skip the
		// narration-status queries below entirely.
		return &chapterMusicState{book: book, ch: ch}, nil
	}
	paragraphs, err := m.store.ListParagraphsRaw(chapterID)
	if err != nil {
		return nil, err
	}
	// Ready narration durations, keyed by paragraph id - under the voice
	// each paragraph currently resolves to; one status query per distinct
	// voice (usually one or a handful per chapter). They size each region
	// below. allReady gates the whole-chapter batch: that one only starts
	// once the *whole* chapter is voiced, so it never competes with the
	// chapter's own narration for the GPU (poolSFX and poolGeneration are
	// mutually exclusive TTS-family pools). The live batch only needs its
	// own regions' paragraphs.
	idsByVoice := map[string][]string{}
	for _, p := range paragraphs {
		voice, verr := m.narration.ForParagraph(book, p.Speaker)
		if verr != nil {
			return nil, verr
		}
		idsByVoice[voice.VoiceID()] = append(idsByVoice[voice.VoiceID()], p.ID)
	}
	durationByID := make(map[string]float64, len(paragraphs))
	allReady := true
	for voiceID, ids := range idsByVoice {
		states, serr := m.store.ParagraphAudioStatuses(ids, voiceID)
		if serr != nil {
			return nil, serr
		}
		for _, id := range ids {
			if st, ok := states[id]; ok && st.Status == store.AudioReady {
				durationByID[id] = st.DurationSeconds
			} else {
				allReady = false
			}
		}
	}
	byIdx := make(map[int]store.Paragraph, len(paragraphs))
	for _, p := range paragraphs {
		byIdx[p.Idx] = p
	}
	regionDuration := func(region store.MusicRegion) (float64, bool) {
		total := 0.0
		for idx := region.StartIdx; idx <= region.EndIdx; idx++ {
			p, ok := byIdx[idx]
			if !ok {
				return 0, false
			}
			d, ok := durationByID[p.ID]
			if !ok {
				return 0, false
			}
			total += d
		}
		return total, true
	}
	return &chapterMusicState{book: book, ch: ch, regions: regions, allReady: allReady, regionDuration: regionDuration}, nil
}

// ErrChapterNotVoiced/ErrChapterNotScored are GenerateChapterMusic's own
// "can't yet" answers: a region's clip is sized to its own narration, so
// music can't generate before the chapter's narration exists, and there
// are no regions at all before scoring.
var (
	ErrChapterNotVoiced = errors.New("chapter narration isn't fully generated yet")
	ErrChapterNotScored = errors.New("chapter background music hasn't been scored yet")
)

// GenerateChapterMusic is the explicit, reader-clicked whole-chapter
// music run (the Speakers page's per-chapter button): unlike the automatic
// path (MaybeAdvanceChapterMusic), it runs regardless of the book-wide
// MusicEnabled toggle - the same "always allowed" treatment scoring
// already gets - and first resets every AudioError region back to pending
// so a retry actually retries them; the automatic path treats an errored
// region as settled and never goes back to it. TierNormal, above the
// automatic path's TierBackground. Returns how many regions it queued (0
// if every region already has music).
func (m *Manager) GenerateChapterMusic(bookID, chapterID string) (int, error) {
	st, err := m.loadChapterMusic(bookID, chapterID)
	if err != nil {
		return 0, err
	}
	if st == nil {
		return 0, ErrTaskNotFound
	}
	if len(st.regions) == 0 {
		return 0, ErrChapterNotScored
	}
	if !st.allReady {
		return 0, ErrChapterNotVoiced
	}
	for i, r := range st.regions {
		if r.Status == store.AudioError && !m.musicRegionGenerating(r.ID) {
			if err := m.store.ResetMusicRegionAudio(r.ID); err != nil {
				return 0, err
			}
			st.regions[i].Status = store.AudioPending
		}
	}
	return m.advanceWholeChapterMusic(bookID, chapterID, st.ch.Idx, st.regions, st.regionDuration, TierNormal), nil
}

// musicRegionPending reports whether a region still needs generating.
// "generating" is never written any more (see Manager.musicGenerating) but
// may still be on a row left over from before that, and means the same
// thing now: nothing is actually generating it.
func musicRegionPending(status string) bool {
	return status == store.AudioPending || status == store.AudioGenerating
}

// advanceWholeChapterMusic dispatches every currently-eligible region of a
// fully-voiced chapter as one KindMusicGeneration batch. The scan walks
// regions in order and stops the moment it hits one it can't safely batch
// because the region before it neither has settled nor is itself part of
// this batch - regions further along the chapter can never be eligible
// before that point either. Everything gathered before that point (zero,
// one, or several regions) becomes one batch.
func (m *Manager) advanceWholeChapterMusic(bookID, chapterID string, chapterIdx int, regions []store.MusicRegion, regionDuration func(store.MusicRegion) (float64, bool), tier int) int {
	batch, initialSeedRegionID := selectWholeChapterMusic(regions, regionDuration, m.musicRegionGenerating)
	if len(batch) == 0 {
		return 0
	}
	m.enqueueMusicGeneration(bookID, chapterID, chapterIdx, tier, func(ctx context.Context, attempt int) error {
		return m.generateChapterMusicBatch(ctx, bookID, chapterID, batch, initialSeedRegionID)
	})
	return len(batch)
}

// selectWholeChapterMusic is advanceWholeChapterMusic's own scan, pulled
// out as a pure function so it's testable without a real Store - generating
// reports whether a region is claimed right now (Manager.musicRegionGenerating).
func selectWholeChapterMusic(regions []store.MusicRegion, regionDuration func(store.MusicRegion) (float64, bool), generating func(string) bool) (batch []pendingMusicRegion, initialSeedRegionID string) {
	// initialSeedRegionID is the batch's own first item's seed, if any -
	// read from whatever real, already-settled region (from an earlier
	// batch, or the chapter's own start) immediately precedes it, since
	// generateChapterMusicBatch's own loop only ever tracks a seed forward
	// from one batch item to the next and has no earlier history of its
	// own to fall back on for the very first one.
	lastBatchedIdx := -1
	for i, region := range regions {
		// A region the live batch is generating right now counts as
		// unsettled - the scan continues past it only to break on the
		// region after it, so nothing gets batched behind it until its
		// own task finishes and re-runs this.
		if !musicRegionPending(region.Status) || generating(region.ID) {
			continue
		}
		if i > 0 {
			prev := regions[i-1]
			settled := !musicRegionPending(prev.Status)
			inThisBatch := lastBatchedIdx == i-1
			if !settled && !inThisBatch {
				break
			}
		}
		total, ok := regionDuration(region)
		if !ok {
			// Stale scoring (a region pointing past the chapter's own
			// paragraphs) - nothing further along can be trusted either.
			break
		}
		if len(batch) == 0 && i > 0 && regions[i-1].Status == store.AudioReady {
			initialSeedRegionID = regions[i-1].ID
		}
		batch = append(batch, pendingMusicRegion{region: region, targetDuration: total})
		lastBatchedIdx = i
	}
	return batch, initialSeedRegionID
}

// advanceLiveMusic queues the next region of the live music chain as its
// own KindMusicLiveGeneration task: the first region at or after the one
// the reader is in (the one whose [StartIdx, EndIdx] contains pos) that
// still needs music and isn't already being generated or queued - see
// selectLiveMusic. At most one live task per chapter is ever queued (not
// yet dispatched) at a time; the chain advances as each one dispatches
// (continueLiveMusic queues its successor, which waits on it via
// liveMusicDependency) and finishes (handleResult's MaybeAdvanceChapterMusic),
// and as more paragraphs get voiced. The chain is naturally bounded by
// narration: a region only goes out once its own paragraphs are voiced.
//
// A still-queued live task for a region the reader has already moved past
// is canceled first, so a jump forward never waits behind stale music.
//
// Crosses chapter boundaries: once every region from pos to the end of
// this chapter is settled or covered, the chain continues into the next
// chapter's first region (see liveChainCarry), skipping over at most
// maxLiveMusicChapterHops chapters in one call.
func (m *Manager) advanceLiveMusic(bookID, chapterID string, chapterIdx int, regions []store.MusicRegion, pos int, regionDuration func(store.MusicRegion) (float64, bool), carry liveChainCarry) {
	behind := map[string]bool{}
	for _, r := range regions {
		if r.EndIdx < pos {
			behind[r.ID] = true
		}
	}
	for {
		tk, ok := m.queue.Cancel(func(c taskqueue.Task) bool {
			return isLiveMusicTask(c, chapterID) && behind[c.(*task).llmKey]
		})
		if !ok {
			break
		}
		m.dropCanceledQueued(tk.(*task))
	}
	if m.queue.AnyQueued(func(c taskqueue.Task) bool { return isLiveMusicTask(c, chapterID) }) {
		return
	}

	next, prevID, ok, exhausted := selectLiveMusic(regions, pos, regionDuration, m.musicRegionGenerating, m.liveMusicTaskExists)
	if !ok {
		if exhausted {
			m.advanceLiveMusicIntoNextChapter(bookID, chapterID, chapterIdx, regions[len(regions)-1].ID, carry.hops)
		}
		return
	}
	// Seeded from prevID's stems, layer by layer (musicRegionSeeds decides
	// which, from the transition and the two ambience prompts) - read at
	// run time, by which point liveMusicDependency has made sure prevID's
	// own generation (if any was pending) is done.
	seedID := prevID
	prevChapterID := chapterID
	if prevID == "" && carry.prevRegionID != "" && next.region.ID == regions[0].ID {
		// First region of a chapter the chain crossed into: wait on the
		// previous chapter's last region, but don't seed from it.
		prevID, prevChapterID = carry.prevRegionID, carry.prevChapterID
	}
	region := next.region
	m.pushTask(&task{
		kind:               KindMusicLiveGeneration,
		tier:               TierUrgent,
		bookID:             bookID,
		chapterID:          chapterID,
		chapterIdx:         chapterIdx,
		llmKey:             region.ID,
		musicRegionStart:   region.StartIdx,
		musicPrevRegionID:  prevID,
		musicPrevChapterID: prevChapterID,
		runMusicGen: func(ctx context.Context, attempt int) error {
			if m.readerPassedRegion(bookID, chapterIdx, region) {
				return nil
			}
			return m.generateChapterMusicBatch(ctx, bookID, chapterID, []pendingMusicRegion{next}, seedID)
		},
	})
}

// maxLiveMusicChapterHops bounds how many chapters one advanceLiveMusic
// call walks past the one it started in, looking for the chain's next
// region - only chapters whose music is already all settled/covered are
// ever walked past, so this just caps store reads when several upcoming
// chapters already have music.
const maxLiveMusicChapterHops = 2

// liveChainCarry is what advanceLiveMusic carries from one chapter into the
// next when the chain crosses a chapter boundary.
type liveChainCarry struct {
	prevChapterID string
	prevRegionID  string
	hops          int
}

// advanceLiveMusicIntoNextChapter continues the live chain from the end of
// chapter chapterIdx (whose last region is lastRegionID) into the next
// chapter, from its first paragraph. A next chapter that isn't scored yet
// stops the chain, but gets its scoring promoted so the chain can cross
// into it once scoring lands (httpapi.ambienceChapter's own
// MaybeAdvanceChapterMusic call picks it back up then).
func (m *Manager) advanceLiveMusicIntoNextChapter(bookID, chapterID string, chapterIdx int, lastRegionID string, hops int) {
	if hops >= maxLiveMusicChapterHops {
		return
	}
	next, err := m.store.GetChapterByIdx(bookID, chapterIdx+1)
	if err != nil || next == nil {
		return
	}
	st, err := m.loadChapterMusic(bookID, next.ID)
	if err != nil || st == nil || !st.book.MusicEnabled {
		return
	}
	if len(st.regions) == 0 {
		m.queue.WithTask(
			func(c taskqueue.Task) bool { return c.Key() == "ambience:"+next.ID },
			func(c taskqueue.Task) {
				if TierLookahead < c.Tier() {
					c.Promote(TierLookahead)
				}
			},
		)
		return
	}
	m.advanceLiveMusic(bookID, next.ID, next.Idx, st.regions, 0, st.regionDuration, liveChainCarry{
		prevChapterID: chapterID,
		prevRegionID:  lastRegionID,
		hops:          hops + 1,
	})
}

// continueLiveMusic is a live task's dispatch-time hook: queue the chain's
// next region now, rather than only once t finishes, so it's already
// waiting (on t, via liveMusicDependency) the moment t's slot frees up.
// Continues from t's own region, or from the reader's saved position if
// that's further along in this chapter.
func (m *Manager) continueLiveMusic(t *task) {
	pos := t.musicRegionStart
	if book, err := m.store.GetBook(t.bookID); err == nil && book != nil && book.PosChapterIdx == t.chapterIdx && book.PosParagraphIdx > pos {
		pos = book.PosParagraphIdx
	}
	m.advanceLiveChapterMusic(t.bookID, t.chapterID, pos)
}

// readerPassedRegion reports whether the reader's saved position is already
// past region (a later chapter, or a later paragraph in this one) - a live
// task that finally dispatches for music nobody will hear skips it; the
// whole-chapter batch still covers it later.
func (m *Manager) readerPassedRegion(bookID string, chapterIdx int, region store.MusicRegion) bool {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		return false
	}
	return book.PosChapterIdx > chapterIdx || (book.PosChapterIdx == chapterIdx && book.PosParagraphIdx > region.EndIdx)
}

// isLiveMusicTask reports whether c is a KindMusicLiveGeneration task for
// chapterID.
func isLiveMusicTask(c taskqueue.Task, chapterID string) bool {
	t := c.(*task)
	return t.kind == KindMusicLiveGeneration && t.chapterID == chapterID
}

// liveMusicTaskExists reports whether a live task for regionID is queued or
// in flight.
func (m *Manager) liveMusicTaskExists(regionID string) bool {
	_, ok := m.queue.Find(func(c taskqueue.Task) bool { return c.Key() == "music_live:"+regionID })
	return ok
}

// liveMusicDependency makes a live task wait for whatever is producing its
// predecessor region's clip (its seed): the predecessor's own live task,
// queued or in flight, or - if the whole-chapter batch has claimed the
// predecessor - that batch.
func (m *Manager) liveMusicDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	if t.musicPrevRegionID == "" {
		return nil
	}
	if d, ok := lq.Get("music_live:" + t.musicPrevRegionID); ok {
		return d
	}
	if m.musicRegionGenerating(t.musicPrevRegionID) {
		if d, ok := lq.Get("music_gen:" + t.musicPrevChapterID); ok {
			return d
		}
	}
	return nil
}

// selectLiveMusic is advanceLiveMusic's own scan, pulled out as a pure
// function for the same reason as selectWholeChapterMusic. Walks forward
// from the reader's region past everything already settled or covered
// (generating, or with a live task queued/in flight - queued) and returns
// the first region that still needs one, plus the region right before it
// (the seed/dependency - see liveMusicDependency), or ok=false if that
// region isn't voiced yet. exhausted reports the chain ran off the end of
// the chapter (everything from the reader's region on is settled or
// covered) - the caller's cue to continue into the next chapter. The
// reader's own region is the one exception
// to "predecessor must be settled or covered": if the region before it
// was never generated at all (the reader jumped straight into the middle
// of the chapter), it generates unseeded rather than making the reader
// wait on music for a stretch they've skipped.
func selectLiveMusic(regions []store.MusicRegion, pos int, regionDuration func(store.MusicRegion) (float64, bool), generating, queued func(string) bool) (next pendingMusicRegion, prevRegionID string, ok, exhausted bool) {
	cur := -1
	for i, r := range regions {
		if r.StartIdx <= pos && pos <= r.EndIdx {
			cur = i
			break
		}
	}
	if cur < 0 {
		return pendingMusicRegion{}, "", false, false
	}
	covered := func(id string) bool { return generating(id) || queued(id) }
	for i := cur; i < len(regions); i++ {
		region := regions[i]
		if !musicRegionPending(region.Status) || covered(region.ID) {
			continue
		}
		total, voiced := regionDuration(region)
		if !voiced {
			return pendingMusicRegion{}, "", false, false
		}
		if i > 0 {
			prev := regions[i-1]
			if !musicRegionPending(prev.Status) || covered(prev.ID) {
				prevRegionID = prev.ID
			}
			// Otherwise i == cur (anything later has a settled/covered
			// predecessor, or the loop would have stopped there) - go
			// unseeded, see this function's own doc comment.
		}
		return pendingMusicRegion{region: region, targetDuration: total}, prevRegionID, true, false
	}
	return pendingMusicRegion{}, "", false, true
}

// claimMusicRegion marks id as being generated, reporting false if some
// other task already has it - the whole-chapter and live batches can both
// reach the same region, and poolSFX runs several tasks at once.
func (m *Manager) claimMusicRegion(id string) bool {
	m.musicGenMu.Lock()
	if m.musicGenerating[id] {
		m.musicGenMu.Unlock()
		return false
	}
	if m.musicGenerating == nil {
		m.musicGenerating = map[string]bool{}
	}
	m.musicGenerating[id] = true
	m.musicGenMu.Unlock()
	m.notifyChanged()
	return true
}

func (m *Manager) releaseMusicRegion(id string) {
	m.musicGenMu.Lock()
	delete(m.musicGenerating, id)
	m.musicGenMu.Unlock()
	m.notifyChanged()
}

func (m *Manager) musicRegionGenerating(id string) bool {
	m.musicGenMu.Lock()
	defer m.musicGenMu.Unlock()
	return m.musicGenerating[id]
}

// MusicRegionGenerating reports whether region id is being generated right
// now - the in-memory state httpapi overlays onto a region's stored status
// (which never holds "generating" itself).
func (m *Manager) MusicRegionGenerating(id string) bool {
	return m.musicRegionGenerating(id)
}

// generateChapterMusicBatch is MaybeAdvanceChapterMusic's own dispatched
// work: generate and persist every region in batch, strictly in order,
// within one task. A "continuation" region's seed is read from whichever
// region immediately precedes it on disk (audiopath.MusicRegionSeedFile) right
// before generating it, not precomputed up front - by the time this loop
// reaches that region, the previous one has either already settled before
// this batch was even built (initialSeedRegionID, only ever consulted for
// batch's own first item - see MaybeAdvanceChapterMusic), or was itself
// just generated (and its file just written) earlier in this very loop, so
// reading fresh each time covers both cases with the same code path.
//
// Stops early (leaving whatever's left in batch untouched, still
// AudioPending) if ctx is canceled between regions - a preempted/paused
// batch simply gets picked up again by a later MaybeAdvanceChapterMusic
// call, the same "cooperative pause, not a failure" shape every other
// batched pass in this package follows.
//
// A single region's own generation failure no longer aborts the rest of
// the batch the way an earlier version of this function did (return err
// immediately, relying on handleResult's own retryEligible/requeueCopy to
// retry the *whole* batch from its own first item). That shape was fine
// for a small, per-region task, but got statistically much worse once
// music generation moved to one task per chapter covering every one of
// its currently-eligible regions at once (splitLongRegions can put 15-20+
// regions in a single long chapter's own batch): one region hitting a
// purely transient failure (e.g. ttsworker mid-restart) meant every region
// after it in dispatch order got zero attempts that round, not just the
// one that actually failed, and a persistent failure on an early region
// could burn through the whole task's own maxTaskAttempts budget - three
// full-batch retries, each one needlessly re-generating every already-
// Ready region before ever reaching the one still broken - while every
// later region sat untouched the entire time. generateMusicRegionWithRetry
// below now retries an individual region's own generation in place (its
// own bounded maxTaskAttempts budget, independent of every other region's
// and of this task's own outer retry), logs whatever it ultimately can't
// recover from, and this loop then moves on to the next region regardless
// - so one region's bad luck can no longer starve its siblings. Every
// region's own Status (Ready/Error) is set by generateMusicRegion itself
// on every attempt, so the DB always ends up correct regardless of how
// this loop as a whole concludes.
func (m *Manager) generateChapterMusicBatch(ctx context.Context, bookID, chapterID string, batch []pendingMusicRegion, initialSeedRegionID string) error {
	prevRegionID := initialSeedRegionID
	for _, item := range batch {
		if ctx.Err() != nil {
			return nil
		}
		// The other music kind may have this region right now - stop
		// here rather than generate what comes after it without its clip
		// to seed from; that task's own completion re-runs
		// MaybeAdvanceChapterMusic, which picks up where this left off.
		if !m.claimMusicRegion(item.region.ID) {
			return nil
		}
		// Re-read now that it's claimed: it may have been generated since
		// this batch was built.
		current, gerr := m.store.GetMusicRegion(item.region.ID)
		if gerr != nil || current == nil || !musicRegionPending(current.Status) {
			m.releaseMusicRegion(item.region.ID)
			if current != nil && current.Status == store.AudioReady {
				prevRegionID = current.ID
			}
			continue
		}
		seeds := m.musicRegionSeeds(bookID, chapterID, item.region, prevRegionID)
		err := m.generateMusicRegionWithRetry(ctx, bookID, chapterID, item.region, item.targetDuration, seeds)
		m.releaseMusicRegion(item.region.ID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			// Already logged (and persisted as AudioError) inside
			// generateMusicRegionWithRetry - move on rather than let this
			// one region's exhausted retries starve every region after it.
			continue
		}
		prevRegionID = item.region.ID
	}
	return nil
}

// musicRegionRetryDelay is the pause generateMusicRegionWithRetry takes
// between one region's own failed attempt and its next - long enough to
// give a crashed/restarting ttsworker (see internal/ttsworker's own
// watchdog) a real chance to be back up by the next attempt, short enough
// not to noticeably slow a batch down when the failure is something else
// entirely (a bad prompt, a genuinely broken request).
const musicRegionRetryDelay = 2 * time.Second

// generateMusicRegionWithRetry retries generateMusicRegion up to
// maxTaskAttempts times for region alone - see generateChapterMusicBatch's
// own doc comment for why this now lives at the individual-region level
// instead of the whole chapter-batch task retrying from its own first
// item. A cancellation is never retried (mirrors retryEligible's own
// exclusion) and returns immediately so the caller's own ctx.Err() check
// treats this the same cooperative-pause way as everywhere else. Every
// attempt after the first, and the final permanent failure if every
// attempt is exhausted, gets its own explicit log line - previously only
// the outer whole-task failure was ever logged, which (now that a batch
// can cover 15-20+ regions) made an individual region's own permanent
// failure easy to lose track of.
func (m *Manager) generateMusicRegionWithRetry(ctx context.Context, bookID, chapterID string, region store.MusicRegion, targetDuration float64, seeds musicSeeds) error {
	var err error
	for attempt := 0; attempt < maxTaskAttempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		err = m.generateMusicRegion(ctx, bookID, chapterID, region, targetDuration, seeds)
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		if attempt+1 >= maxTaskAttempts {
			break
		}
		log.Printf("jobs: music region %s (chapter %s) generation failed (attempt %d/%d), retrying: %v", region.ID, chapterID, attempt+1, maxTaskAttempts, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(musicRegionRetryDelay):
		}
	}
	log.Printf("jobs: music region %s (chapter %s) generation permanently failed after %d attempts: %v", region.ID, chapterID, maxTaskAttempts, err)
	return err
}

// musicSeeds are one region's continuation seeds - nil when the music
// starts fresh. See musicRegionSeeds.
type musicSeeds struct {
	music []byte
	// latents is the seed as Stable Audio latents (a .seed.lat), preferred
	// over music - see musicgen.Region.SeedLatents.
	latents *ttsproto.Latents
}

// musicRegionSeeds reads region's continuation seed from the region
// immediately before it (prevRegionID, "" for none): the music layer
// continues only across a "continuation" transition, seeded from the
// previous region's music-only seed tail (audiopath.MusicRegionSeedFile),
// never the mix, so ambience never bleeds into the continuation. A region
// the background conversion (internal/audiomaint) hasn't reached yet
// still has its full legacy music stem, or - generated before stems
// existed - only a WAV mix. Ambience needs no seed - a setting's ambience
// is one shared loop (see generateMusicRegion).
func (m *Manager) musicRegionSeeds(bookID, chapterID string, region store.MusicRegion, prevRegionID string) musicSeeds {
	var seeds musicSeeds
	if prevRegionID == "" || region.Transition != store.MusicTransitionContinuation {
		return seeds
	}
	if l, err := latents.ReadFile(audiopath.MusicRegionSeedLatentsFile(m.dataDir, bookID, chapterID, prevRegionID)); err == nil {
		seeds.latents = l
	}
	for _, path := range []string{
		audiopath.MusicRegionSeedFile(m.dataDir, bookID, chapterID, prevRegionID),
		audiopath.LegacyMusicRegionStemFile(m.dataDir, bookID, chapterID, prevRegionID, "music"),
		audiopath.LegacyWAV(audiopath.MusicRegionFile(m.dataDir, bookID, chapterID, prevRegionID)),
	} {
		if data, err := os.ReadFile(path); err == nil {
			seeds.music = data
			break
		}
	}
	return seeds
}

// ambienceLoop reads a setting's loop WAV, rebuilding it first from its
// latents sidecar when only that exists (a book imported from a
// latent-only export): one codec decode plus the loop recipe
// (musicgen.LoopFromDecoded), cached as the WAV. An error means neither is
// usable - the caller renders the loop afresh.
func (m *Manager) ambienceLoop(ctx context.Context, loopPath string) ([]byte, error) {
	data, err := os.ReadFile(loopPath)
	if err == nil {
		return data, nil
	}
	lat, lerr := latents.ReadFile(latents.Sidecar(loopPath))
	if lerr != nil {
		return nil, err
	}
	decoded, err := m.tts.CodecDecode(ctx, "stable_audio_medium", lat)
	if err != nil {
		log.Printf("jobs: rebuild ambience loop %s from latents: %v", loopPath, err)
		return nil, err
	}
	loop, err := musicgen.LoopFromDecoded(decoded)
	if err != nil {
		return nil, err
	}
	if err := audiopath.WriteFileAtomic(loopPath, loop); err != nil {
		log.Printf("jobs: cache rebuilt ambience loop %s: %v", loopPath, err)
	}
	return loop, nil
}

// saveMusicSeed keeps what a following continuation region seeds from:
// the tail as latents (f16 - conditioning only, ~2e-4 off, far under the
// ~17% a WAV re-encode loses) when musicgen produced them, else as WAV.
// Either way it removes the other form, so a region never has both.
func (m *Manager) saveMusicSeed(bookID, chapterID, regionID string, music []byte, seedLatents *ttsproto.Latents) error {
	wavPath := audiopath.MusicRegionSeedFile(m.dataDir, bookID, chapterID, regionID)
	latPath := audiopath.MusicRegionSeedLatentsFile(m.dataDir, bookID, chapterID, regionID)
	if seedLatents != nil {
		if err := latents.WriteFile(latPath, seedLatents, "f16"); err != nil {
			return err
		}
		_ = os.Remove(wavPath)
		return nil
	}
	seed, err := musicgen.SeedTail(music)
	if err != nil {
		return fmt.Errorf("cut music seed: %w", err)
	}
	if err := audiopath.WriteFileAtomic(wavPath, seed); err != nil {
		return err
	}
	_ = os.Remove(latPath)
	return nil
}

// lockAmbience serializes work on one book's ambience loop for prompt, so
// two regions set in the same place never both render it.
func (m *Manager) lockAmbience(bookID, prompt string) func() {
	v, _ := m.ambienceLocks.LoadOrStore(audiopath.AmbienceLoopFile("", bookID, prompt), &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// mixAmbienceLoop tiles loop to music's length and mixes it under music.
// The loop's start offset is derived from regionID, so neighbouring regions
// in one place don't all open on the same sound.
func mixAmbienceLoop(music, loop []byte, regionID string) ([]byte, error) {
	dur, err := wav.Duration(music)
	if err != nil {
		return nil, err
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(regionID))
	offset := float64(h.Sum32()%1000) / 1000 * musicgen.AmbienceLoopSeconds
	ambience, err := musicgen.TileLoop(loop, dur.Seconds(), offset)
	if err != nil {
		return nil, err
	}
	return musicgen.MixAmbience(music, ambience)
}

// generateMusicRegion is generateChapterMusicBatch's own per-region work:
// render region's full-duration clip via internal/musicgen (chaining as
// many Stable Audio Medium calls as targetDuration needs - see that
// package's own doc comment) and persist it exactly like an ordinary
// paragraph's own audio (saveParagraphAudio's shape, just against
// music_regions/audiopath.MusicRegionFile instead of paragraph_audio/
// audiopath.ParagraphFile). A region with an ambience prompt gets that
// setting's ambience loop (audiopath.AmbienceLoopFile - rendered alongside
// this region's music the first time the setting appears, reused after)
// tiled under it and served mixed (musicgen.MixAmbience), as Ogg Opus; the
// music layer's tail is kept on disk to seed the next region
// (musicRegionSeeds).
func (m *Manager) generateMusicRegion(ctx context.Context, bookID, chapterID string, region store.MusicRegion, targetDuration float64, seeds musicSeeds) error {
	in := musicgen.Region{
		Prompt:                region.Prompt,
		TargetDurationSeconds: targetDuration,
		Seed:                  seeds.music,
		SeedLatents:           seeds.latents,
	}
	var loop []byte
	loopPath := audiopath.AmbienceLoopFile(m.dataDir, bookID, region.Ambience)
	if region.Ambience != "" {
		defer m.lockAmbience(bookID, region.Ambience)()
		if data, err := m.ambienceLoop(ctx, loopPath); err == nil {
			loop = data
		} else {
			in.AmbiencePrompt = region.Ambience
		}
	}
	res, err := musicgen.GenerateRegion(ctx, m.tts, in)
	if err != nil {
		_ = m.store.SetMusicRegionError(region.ID, err.Error())
		return err
	}
	music := res.Music
	audio := music
	if res.AmbienceLoop != nil {
		loop = res.AmbienceLoop
		if err := os.MkdirAll(audiopath.AmbienceDir(m.dataDir, bookID), 0o755); err == nil {
			if err := os.WriteFile(loopPath, loop, 0o644); err != nil {
				log.Printf("jobs: save ambience loop for region %s: %v", region.ID, err)
			}
			// The loop's latents, for latent-only exports (f16: a transfer
			// decodes on other hardware anyway, so exactness buys nothing).
			if res.AmbienceLoopLatents != nil {
				if err := latents.WriteFile(latents.Sidecar(loopPath), res.AmbienceLoopLatents, "f16"); err != nil {
					log.Printf("jobs: save ambience loop latents for region %s: %v", region.ID, err)
				}
			}
		}
	}
	if loop != nil {
		audio, err = mixAmbienceLoop(music, loop, region.ID)
		if err != nil {
			_ = m.store.SetMusicRegionError(region.ID, "ambience: "+err.Error())
			return fmt.Errorf("ambience: %w", err)
		}
	}
	if err := audiopath.EnsureMusicDir(m.dataDir, bookID, chapterID); err != nil {
		_ = m.store.SetMusicRegionError(region.ID, "failed to create music dir: "+err.Error())
		return err
	}
	// A regenerated region may still have full-length stems from before
	// seeds were cut to their tail.
	for _, stem := range []string{"music", "ambience"} {
		_ = os.Remove(audiopath.LegacyMusicRegionStemFile(m.dataDir, bookID, chapterID, region.ID, stem))
	}
	if err := m.saveMusicSeed(bookID, chapterID, region.ID, music, res.SeedLatents); err != nil {
		_ = m.store.SetMusicRegionError(region.ID, "failed to save music seed: "+err.Error())
		return err
	}
	outPath := audiopath.MusicRegionFile(m.dataDir, bookID, chapterID, region.ID)
	if err := audiopath.WriteClip(outPath, audio); err != nil {
		_ = m.store.SetMusicRegionError(region.ID, "failed to save audio: "+err.Error())
		return err
	}
	dur, err := wav.Duration(audio)
	if err != nil {
		log.Printf("jobs: parse wav duration for music region %s: %v", region.ID, err)
	}
	if err := m.store.SetMusicRegionReady(region.ID, dur.Seconds()); err != nil {
		log.Printf("jobs: mark music region %s ready: %v", region.ID, err)
	}
	return nil
}
