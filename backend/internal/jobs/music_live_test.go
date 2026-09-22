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

func TestSelectLiveMusicTakesReaderRegionAndNext(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(store.AudioReady, p, p, p)
	// Reader in region b (paragraph 14); b and c voiced, d not.
	batch, seed := selectLiveMusic(regions, 14, voicedThrough(29), noneGenerating)
	if got := batchIDs(batch); !eq(got, []string{"b", "c"}) {
		t.Fatalf("batch = %v; want [b c]", got)
	}
	if seed != "a" {
		t.Fatalf("seed = %q; want a (b continues from ready a)", seed)
	}
}

func TestSelectLiveMusicWaitsForUnvoicedRegion(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(store.AudioReady, p, p, p)
	// Only part of b is voiced - nothing yet.
	if batch, _ := selectLiveMusic(regions, 14, voicedThrough(15), noneGenerating); len(batch) != 0 {
		t.Fatalf("batch = %v; want empty until b is voiced", batchIDs(batch))
	}
}

func TestSelectLiveMusicJumpedIntoChapterGoesUnseeded(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(p, p, p, p)
	// Reader jumped straight to region c; a/b never generated.
	batch, seed := selectLiveMusic(regions, 25, voicedThrough(39), noneGenerating)
	if got := batchIDs(batch); !eq(got, []string{"c", "d"}) {
		t.Fatalf("batch = %v; want [c d]", got)
	}
	if seed != "" {
		t.Fatalf("seed = %q; want unseeded", seed)
	}
}

func TestSelectLiveMusicWaitsOnPredecessorBeingGenerated(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(p, p, p, p)
	generating := func(id string) bool { return id == "b" }
	if batch, _ := selectLiveMusic(regions, 25, voicedThrough(39), generating); len(batch) != 0 {
		t.Fatalf("batch = %v; want empty while b (c's seed) is generating", batchIDs(batch))
	}
}

func TestSelectLiveMusicSkipsReadyReaderRegion(t *testing.T) {
	p := store.AudioPending
	regions := fourRegions(store.AudioReady, store.AudioReady, p, p)
	batch, seed := selectLiveMusic(regions, 14, voicedThrough(39), noneGenerating)
	if got := batchIDs(batch); !eq(got, []string{"c"}) {
		t.Fatalf("batch = %v; want [c]", got)
	}
	if seed != "b" {
		t.Fatalf("seed = %q; want b", seed)
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
