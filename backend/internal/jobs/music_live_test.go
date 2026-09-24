package jobs

import (
	"errors"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/taskqueue"
)

// fourRegions is a chapter of paragraphs 0-39 split into four ten-
// paragraph regions; every region after the first is a continuation.
func fourRegions(statuses ...string) []store.MusicRegion {
	regions := make([]store.MusicRegion, 4)
	for i := range regions {
		regions[i] = store.MusicRegion{
			ID:         string(rune('a' + i)),
			Idx:        i,
			StartIdx:   i * 10,
			EndIdx:     i*10 + 9,
			Transition: store.MusicTransitionContinuation,
			Status:     statuses[i],
		}
	}
	regions[0].Transition = store.MusicTransitionCut
	return regions
}

// voicedThrough reports a region as voiced (10s) only if every one of its
// paragraphs is at or before lastVoiced.
func voicedThrough(lastVoiced int) func(store.MusicRegion) (float64, bool) {
	return func(r store.MusicRegion) (float64, bool) {
		if r.EndIdx > lastVoiced {
			return 0, false
		}
		return 10, true
	}
}

func noneGenerating(string) bool { return false }

func batchIDs(batch []pendingMusicRegion) []string {
	var ids []string
	for _, p := range batch {
		ids = append(ids, p.region.ID)
	}
	return ids
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func noneQueued(string) bool { return false }

func TestSelectLiveMusicTakesReaderRegion(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(store.AudioReady, p, p, p)
	// Reader in region b (paragraph 14); b and c voiced, d not.
	next, prev, ok, _ := selectLiveMusic(regions, 14, voicedThrough(29), noneGenerating, noneQueued)
	if !ok || next.region.ID != "b" || prev != "a" {
		t.Fatalf("next = %q prev %q ok %v; want b seeded from ready a", next.region.ID, prev, ok)
	}
}

func TestSelectLiveMusicChainsPastCoveredRegion(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(store.AudioReady, p, p, p)
	// b already has a live task (queued or running) - chain on to c,
	// depending on b.
	queued := func(id string) bool { return id == "b" }
	next, prev, ok, _ := selectLiveMusic(regions, 14, voicedThrough(39), noneGenerating, queued)
	if !ok || next.region.ID != "c" || prev != "b" {
		t.Fatalf("next = %q prev %q ok %v; want c after b", next.region.ID, prev, ok)
	}
	// Same when b is claimed by the whole-chapter batch instead.
	generating := func(id string) bool { return id == "b" }
	next, prev, ok, _ = selectLiveMusic(regions, 14, voicedThrough(39), generating, noneQueued)
	if !ok || next.region.ID != "c" || prev != "b" {
		t.Fatalf("next = %q prev %q ok %v; want c after generating b", next.region.ID, prev, ok)
	}
}

func TestSelectLiveMusicWaitsForUnvoicedRegion(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(store.AudioReady, p, p, p)
	// Only part of b is voiced - nothing yet.
	if next, _, ok, _ := selectLiveMusic(regions, 14, voicedThrough(15), noneGenerating, noneQueued); ok {
		t.Fatalf("next = %q; want nothing until b is voiced", next.region.ID)
	}
	// b covered, c not voiced - the chain stops there.
	queued := func(id string) bool { return id == "b" }
	if next, _, ok, _ := selectLiveMusic(regions, 14, voicedThrough(25), noneGenerating, queued); ok {
		t.Fatalf("next = %q; want nothing until c is voiced", next.region.ID)
	}
}

func TestSelectLiveMusicJumpedIntoChapterGoesUnseeded(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(p, p, p, p)
	// Reader jumped straight to region c; a/b never generated.
	next, prev, ok, _ := selectLiveMusic(regions, 25, voicedThrough(39), noneGenerating, noneQueued)
	if !ok || next.region.ID != "c" || prev != "" {
		t.Fatalf("next = %q prev %q ok %v; want c unseeded", next.region.ID, prev, ok)
	}
}

func TestSelectLiveMusicExhaustedAtChapterEnd(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(store.AudioReady, p, p, p)
	queued := func(id string) bool { return id == "c" || id == "d" }
	generating := func(id string) bool { return id == "b" }
	if _, _, ok, exhausted := selectLiveMusic(regions, 14, voicedThrough(39), generating, queued); ok || !exhausted {
		t.Fatalf("ok=%v exhausted=%v; want exhausted once b-d are all covered", ok, exhausted)
	}
	// Blocked on an unvoiced region is not exhausted.
	if _, _, ok, exhausted := selectLiveMusic(regions, 14, voicedThrough(25), generating, noneQueued); ok || exhausted {
		t.Fatalf("ok=%v exhausted=%v; want blocked, not exhausted", ok, exhausted)
	}
}

func TestSelectLiveMusicSkipsReadyReaderRegion(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(store.AudioReady, store.AudioReady, p, p)
	next, prev, ok, _ := selectLiveMusic(regions, 14, voicedThrough(39), noneGenerating, noneQueued)
	if !ok || next.region.ID != "c" || prev != "b" {
		t.Fatalf("next = %q prev %q ok %v; want c seeded from b", next.region.ID, prev, ok)
	}
}

func TestSelectWholeChapterMusicStopsAtRegionBeingGenerated(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(store.AudioReady, p, p, p)
	generating := func(id string) bool { return id == "b" }
	if batch, _ := selectWholeChapterMusic(regions, voicedThrough(39), generating); len(batch) != 0 {
		t.Fatalf("batch = %v; want empty - everything after b needs b's clip", batchIDs(batch))
	}
	batch, seed := selectWholeChapterMusic(regions, voicedThrough(39), noneGenerating)
	if got := batchIDs(batch); !eq(got, []string{"b", "c", "d"}) || seed != "a" {
		t.Fatalf("batch = %v seed %q; want [b c d] seeded from a", got, seed)
	}
}

func TestSelectTreatsLegacyGeneratingStatusAsPending(t *testing.T) {
	regions := fourRegions(store.AudioReady, store.AudioGenerating, store.AudioPending, store.AudioPending)
	batch, _ := selectWholeChapterMusic(regions, voicedThrough(39), noneGenerating)
	if got := batchIDs(batch); !eq(got, []string{"b", "c", "d"}) {
		t.Fatalf("batch = %v; want a stored \"generating\" treated as pending", got)
	}
}

func TestClaimMusicRegion(t *testing.T) {
	m := newTestManager()
	if !m.claimMusicRegion("r1") {
		t.Fatal("first claim failed")
	}
	if m.claimMusicRegion("r1") {
		t.Fatal("second claim succeeded while r1 is still claimed")
	}
	if !m.MusicRegionGenerating("r1") {
		t.Fatal("r1 not reported generating while claimed")
	}
	m.releaseMusicRegion("r1")
	if m.MusicRegionGenerating("r1") || !m.claimMusicRegion("r1") {
		t.Fatal("r1 not claimable again after release")
	}
}

func TestGenerateChapterMusicExplicit(t *testing.T) {
	s := openTestStore(t)
	// Music left off: the explicit button runs regardless.
	book, chapterID := createBookAndChapter(t, s, "", 0, "First paragraph.", "Second paragraph.")
	mgr := newTestManagerWithStore(s)
	mgr.narration = narration.NewResolver(s)

	if _, err := mgr.GenerateChapterMusic(book.ID, chapterID); !errors.Is(err, ErrChapterNotScored) {
		t.Fatalf("unscored: err = %v; want ErrChapterNotScored", err)
	}
	regions, err := s.AppendMusicRegions(chapterID, []store.MusicRegionInput{{StartIdx: 0, Mood: "calm", Prompt: "calm music"}}, 1)
	if err != nil {
		t.Fatalf("AppendMusicRegions: %v", err)
	}
	if _, err := mgr.GenerateChapterMusic(book.ID, chapterID); !errors.Is(err, ErrChapterNotVoiced) {
		t.Fatalf("unvoiced: err = %v; want ErrChapterNotVoiced", err)
	}

	voice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	paragraphs, _ := s.ListParagraphsRaw(chapterID)
	for _, p := range paragraphs {
		if err := s.SetParagraphReady(p.ID, voice.VoiceID(), 2); err != nil {
			t.Fatalf("SetParagraphReady: %v", err)
		}
	}
	// A previously failed region is retried by the explicit run.
	if err := s.SetMusicRegionError(regions[0].ID, "boom"); err != nil {
		t.Fatalf("SetMusicRegionError: %v", err)
	}
	queued, err := mgr.GenerateChapterMusic(book.ID, chapterID)
	if err != nil || queued != 1 {
		t.Fatalf("GenerateChapterMusic = %d, %v; want 1 queued", queued, err)
	}
	task, ok := mgr.queue.Find(func(c taskqueue.Task) bool { return c.Key() == "music_gen:"+chapterID })
	if !ok || task.Tier() != TierNormal {
		t.Fatalf("music_gen task found=%v; want queued at TierNormal", ok)
	}
	if r, _ := s.GetMusicRegion(regions[0].ID); r == nil || r.Status != store.AudioPending {
		t.Fatalf("errored region not reset to pending: %+v", r)
	}
}

// TestLookaheadOnlyDrivesLiveMusic: reader-position lookahead on a fully
// voiced chapter must neither queue the whole-chapter music batch nor
// promote an already-queued one out of TierBackground - it only drives
// the live path.
func TestLookaheadOnlyDrivesLiveMusic(t *testing.T) {
	s := openTestStore(t)
	book, chapterID := createBookAndChapter(t, s, "", 0, "First paragraph.", "Second paragraph.")
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, book.CharacterVoiceMode, false, true); err != nil {
		t.Fatalf("UpdateVoice(MusicEnabled=true): %v", err)
	}
	book, _ = s.GetBook(book.ID)
	if _, err := s.AppendMusicRegions(chapterID, []store.MusicRegionInput{{StartIdx: 0, Mood: "calm", Prompt: "calm music"}}, 0); err != nil {
		t.Fatalf("AppendMusicRegions: %v", err)
	}

	mgr := newTestManagerWithStore(s)
	mgr.narration = narration.NewResolver(s)
	mgr.ctx = t.Context() // enqueueLookahead checks it between chapters
	voice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	paragraphs, _ := s.ListParagraphsRaw(chapterID)
	for _, p := range paragraphs {
		if err := s.SetParagraphReady(p.ID, voice.VoiceID(), 2); err != nil {
			t.Fatalf("SetParagraphReady: %v", err)
		}
	}
	findGen := func() (taskqueue.Task, bool) {
		return mgr.queue.Find(func(c taskqueue.Task) bool { return c.Key() == "music_gen:"+chapterID })
	}

	mgr.enqueueLookahead(book.ID, 0, 0, LookaheadParagraphCount)
	if _, ok := findGen(); ok {
		t.Fatalf("lookahead queued the whole-chapter music batch")
	}

	mgr.MaybeAdvanceChapterMusic(book.ID, chapterID)
	if task, ok := findGen(); !ok || task.Tier() != TierBackground {
		t.Fatalf("music_gen found=%v; want queued at TierBackground", ok)
	}
	mgr.enqueueLookahead(book.ID, 0, 0, LookaheadParagraphCount)
	if task, ok := findGen(); !ok || task.Tier() != TierBackground {
		t.Fatalf("lookahead promoted the whole-chapter music batch (found=%v)", ok)
	}
}

// TestLiveMusicChainsRegionByRegion: a live task queues the next region's
// task as soon as it dispatches, and that successor can't dispatch until
// its predecessor (its seed) finishes. Only one live task per chapter is
// ever queued ahead at a time.
func TestLiveMusicChainsRegionByRegion(t *testing.T) {
	s := openTestStore(t)
	book, chapterID := createBookAndChapter(t, s, "", 0, "One.", "Two.", "Three.")
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, book.CharacterVoiceMode, false, true); err != nil {
		t.Fatalf("UpdateVoice(MusicEnabled=true): %v", err)
	}
	book, _ = s.GetBook(book.ID)
	regions, err := s.AppendMusicRegions(chapterID, []store.MusicRegionInput{
		{StartIdx: 0, Prompt: "a", Transition: store.MusicTransitionCut},
		{StartIdx: 1, Prompt: "b", Transition: store.MusicTransitionContinuation},
		{StartIdx: 2, Prompt: "c", Transition: store.MusicTransitionContinuation},
	}, 2)
	if err != nil {
		t.Fatalf("AppendMusicRegions: %v", err)
	}

	mgr := newTestManagerWithStore(s)
	mgr.narration = narration.NewResolver(s)
	mgr.queue.RegisterPool(poolSFX, taskqueue.PoolConfig{Capacity: 4})
	voice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	paragraphs, _ := s.ListParagraphsRaw(chapterID)
	for _, p := range paragraphs {
		if err := s.SetParagraphReady(p.ID, voice.VoiceID(), 2); err != nil {
			t.Fatalf("SetParagraphReady: %v", err)
		}
	}
	popLive := func() *task {
		tk, ok := mgr.queue.Pop()
		if !ok {
			return nil
		}
		return tk.(*task)
	}

	mgr.advanceLiveChapterMusic(book.ID, chapterID, 0)
	mgr.advanceLiveChapterMusic(book.ID, chapterID, 0) // no second queued task
	a := popLive()
	if a == nil || a.llmKey != regions[0].ID {
		t.Fatalf("first live task = %+v; want region a", a)
	}
	if popLive() != nil {
		t.Fatalf("more than one live task was queued ahead")
	}

	// a dispatches: b is queued right away, but blocked on a.
	mgr.continueLiveMusic(a)
	if !mgr.liveMusicTaskExists(regions[1].ID) {
		t.Fatalf("b not queued when a dispatched")
	}
	if tk := popLive(); tk != nil {
		t.Fatalf("dispatched %s while a (its seed) is still in flight", tk.Key())
	}

	mgr.queue.Finish(a.Key())
	b := popLive()
	if b == nil || b.llmKey != regions[1].ID || b.musicPrevRegionID != regions[0].ID {
		t.Fatalf("after a finished, got %+v; want region b depending on a", b)
	}
	mgr.continueLiveMusic(b)
	if !mgr.liveMusicTaskExists(regions[2].ID) {
		t.Fatalf("c not queued when b dispatched")
	}
}

// TestLiveMusicChainCrossesChapterBoundary: once chapter 1's last region is
// in flight, dispatching it queues chapter 2's first region, which waits on
// it (chain order) without seeding from it.
func TestLiveMusicChainCrossesChapterBoundary(t *testing.T) {
	s := openTestStore(t)
	bookID, _, err := s.CreateBook("Book", "Author", "en", "", "", 0, []store.ChapterInput{
		{Title: "Ch1", Blocks: []store.BlockInput{{Kind: store.BlockText, Text: "One."}}},
		{Title: "Ch2", Blocks: []store.BlockInput{{Kind: store.BlockText, Text: "Two."}}},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	book, _ := s.GetBook(bookID)
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, book.CharacterVoiceMode, false, true); err != nil {
		t.Fatalf("UpdateVoice(MusicEnabled=true): %v", err)
	}
	book, _ = s.GetBook(bookID)

	mgr := newTestManagerWithStore(s)
	mgr.narration = narration.NewResolver(s)
	mgr.queue.RegisterPool(poolSFX, taskqueue.PoolConfig{Capacity: 4})
	voice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	var chapterIDs, regionIDs []string
	for idx := 0; idx < 2; idx++ {
		ch, err := s.GetChapterByIdx(bookID, idx)
		if err != nil || ch == nil {
			t.Fatalf("GetChapterByIdx(%d): %v", idx, err)
		}
		chapterIDs = append(chapterIDs, ch.ID)
		regions, err := s.AppendMusicRegions(ch.ID, []store.MusicRegionInput{{StartIdx: 0, Prompt: "m", Transition: store.MusicTransitionContinuation}}, 0)
		if err != nil {
			t.Fatalf("AppendMusicRegions: %v", err)
		}
		regionIDs = append(regionIDs, regions[0].ID)
		paragraphs, _ := s.ListParagraphsRaw(ch.ID)
		for _, p := range paragraphs {
			if err := s.SetParagraphReady(p.ID, voice.VoiceID(), 2); err != nil {
				t.Fatalf("SetParagraphReady: %v", err)
			}
		}
	}

	mgr.advanceLiveChapterMusic(bookID, chapterIDs[0], 0)
	tk, ok := mgr.queue.Pop()
	if !ok || tk.(*task).llmKey != regionIDs[0] {
		t.Fatalf("first live task = %v; want chapter 1's region", tk)
	}
	last := tk.(*task)

	mgr.continueLiveMusic(last)
	found, ok := mgr.queue.Find(func(c taskqueue.Task) bool { return c.Key() == "music_live:"+regionIDs[1] })
	if !ok {
		t.Fatalf("chapter 2's first region not queued when chapter 1's last dispatched")
	}
	next := found.(*task)
	if next.chapterID != chapterIDs[1] || next.musicPrevRegionID != regionIDs[0] || next.musicPrevChapterID != chapterIDs[0] {
		t.Fatalf("next = chapter %s prev %s/%s; want chapter 2 waiting on chapter 1's region", next.chapterID, next.musicPrevChapterID, next.musicPrevRegionID)
	}
	if tk, ok := mgr.queue.Pop(); ok {
		t.Fatalf("dispatched %s while chapter 1's region is still in flight", tk.Key())
	}
	mgr.queue.Finish(last.Key())
	if tk, ok := mgr.queue.Pop(); !ok || tk.Key() != next.Key() {
		t.Fatalf("after chapter 1 finished, got %v; want chapter 2's region", tk)
	}
}
