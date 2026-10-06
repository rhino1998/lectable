package jobs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiomaint"
	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsworker/ttsworkertest"
)

// repeatedChapter is a chapter saying "Fritz said." at paragraphs 0, 7 and
// 8: 7 is too far from 0 to be part of its run (clipkey.RunWindow), 8 is
// right behind 7.
var repeatedChapter = []string{
	"Fritz said.", "One.", "Two.", "Three.", "Four.", "Five.", "Six.", "Fritz said.", "Fritz said.",
}

// waitIdle waits for mgr to have nothing queued or running. "Generate this
// chapter" is a task of its own that outlives the chapter's last clip by a
// moment, and asking for the same chapter again before it is done only
// joins it.
func waitIdle(t *testing.T, mgr *Manager) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		inFlight, queued := mgr.Snapshot()
		return len(inFlight)+len(queued) == 0
	})
}

// clipKeys returns the shared clip each of chapterID's paragraphs is
// ready on under voiceID, by Idx ("" for none).
func clipKeys(t *testing.T, s *store.SQLiteStore, chapterID, voiceID string) ([]store.Paragraph, []store.AudioState) {
	t.Helper()
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	ids := make([]string, len(paragraphs))
	for i, p := range paragraphs {
		ids[i] = p.ID
	}
	states, err := s.ParagraphAudioStatuses(ids, voiceID)
	if err != nil {
		t.Fatalf("ParagraphAudioStatuses: %v", err)
	}
	out := make([]store.AudioState, len(paragraphs))
	for i, id := range ids {
		out[i] = states[id]
	}
	return paragraphs, out
}

func TestRepeatedLinesShareAClipOutsideARun(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)
	book, chapterID := createBookAndChapter(t, s, "", 0, repeatedChapter...)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)
	mgr.EnqueueChapter(book.ID, chapterID, 0)

	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })

	paragraphs, states := clipKeys(t, s, chapterID, voiceID)
	if states[0].ClipKey == "" || states[0].ClipKey != states[7].ClipKey {
		t.Errorf("paragraphs 0 and 7 say the same line more than a run apart but are on clips %q and %q", states[0].ClipKey, states[7].ClipKey)
	}
	if states[7].ClipKey == states[8].ClipKey {
		t.Error("paragraphs 7 and 8 repeat a line back to back but share one clip")
	}
	if got, want := len(fake.GenerateCalls()), len(repeatedChapter)-1; got != want {
		t.Errorf("got %d Generate calls, want %d (paragraph 7 reuses paragraph 0's clip)", got, want)
	}
	if states[0].DurationSeconds == 0 || states[0].DurationSeconds != states[7].DurationSeconds {
		t.Errorf("durations %v and %v, want the shared clip's for both", states[0].DurationSeconds, states[7].DurationSeconds)
	}
	shared := audiopath.SharedClipFile(dataDir, states[0].ClipKey)
	before, err := os.Stat(shared)
	if err != nil {
		t.Fatalf("shared clip: %v", err)
	}
	if _, err := os.Stat(audiopath.ParagraphFile(dataDir, book.ID, chapterID, voiceID, 0)); !os.IsNotExist(err) {
		t.Errorf("a paragraph on a shared clip still has a file at its own path: %v", err)
	}

	// Regenerating one of the two renders the clip anew for both.
	calls := len(fake.GenerateCalls())
	mgr.EnqueueParagraphRegenerate(book.ID, chapterID, 0, paragraphs[7])
	waitFor(t, 5*time.Second, func() bool { return len(fake.GenerateCalls()) > calls })
	waitFor(t, 5*time.Second, func() bool {
		after, err := os.Stat(shared)
		return err == nil && !os.SameFile(before, after) && allParagraphsReady(t, s, chapterID, voiceID)
	})
	_, states = clipKeys(t, s, chapterID, voiceID)
	if states[0].ClipKey != states[7].ClipKey || states[0].Status != store.AudioReady {
		t.Errorf("after regenerating paragraph 7, paragraph 0 is %s on %q and 7 on %q, want both ready on one clip",
			states[0].Status, states[0].ClipKey, states[7].ClipKey)
	}
}

// A clip outlives its last paragraph: the same reading wanted again is
// found, not rendered - unless the audio was cleared on purpose
// (DiscardOrphanedClips), or the clip has sat unused past its retention.
func TestOrphanedSharedClipIsReusedUntilDiscarded(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)
	book, chapterID := createBookAndChapter(t, s, "", 0, "The only paragraph.")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)
	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()
	narrate := func() store.AudioState {
		t.Helper()
		waitIdle(t, mgr)
		mgr.EnqueueChapter(book.ID, chapterID, 0)
		waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })
		_, states := clipKeys(t, s, chapterID, voiceID)
		return states[0]
	}
	sweep := func(cutoff time.Time) int {
		inUse, err := s.ClipKeys()
		if err != nil {
			t.Fatalf("ClipKeys: %v", err)
		}
		return audiomaint.SweepSharedClips(ctx, dataDir, func(key string) bool { return !inUse[key] }, cutoff).Removed
	}

	first := narrate()
	if first.ClipKey == "" {
		t.Fatal("the paragraph isn't on a shared clip")
	}
	if n := sweep(time.Now().Add(time.Minute)); n != 0 {
		t.Errorf("sweep removed %d clips still in use", n)
	}

	// The rows go, as on a voice change or a reimport: the clip stays.
	if err := s.DeleteChapterAudio(chapterID); err != nil {
		t.Fatalf("DeleteChapterAudio: %v", err)
	}
	calls := len(fake.GenerateCalls())
	again := narrate()
	if got := len(fake.GenerateCalls()); got != calls {
		t.Errorf("got %d Generate calls, want %d: the orphaned clip should have been found", got, calls)
	}
	if again.ClipKey != first.ClipKey || again.DurationSeconds != first.DurationSeconds || again.WordTimings != first.WordTimings {
		t.Errorf("reused clip %q (%vs), want %q (%vs) with its word timings", again.ClipKey, again.DurationSeconds, first.ClipKey, first.DurationSeconds)
	}

	// An explicit clear discards what it orphaned, so it renders anew.
	cleared := time.Now().Add(-time.Second)
	if err := s.DeleteChapterAudio(chapterID); err != nil {
		t.Fatalf("DeleteChapterAudio: %v", err)
	}
	if n := mgr.DiscardOrphanedClips(cleared); n != 1 {
		t.Errorf("discarded %d clips, want 1", n)
	}
	if _, err := os.Stat(audiopath.SharedClipFile(dataDir, first.ClipKey)); !os.IsNotExist(err) {
		t.Errorf("discarded clip still on disk: %v", err)
	}
	narrate()
	if got := len(fake.GenerateCalls()); got != calls+1 {
		t.Errorf("got %d Generate calls after an explicit clear, want %d", got, calls+1)
	}

	// Left unused, the clip is swept once it is old enough.
	if err := s.DeleteChapterAudio(chapterID); err != nil {
		t.Fatalf("DeleteChapterAudio: %v", err)
	}
	if n := sweep(time.Now().Add(-time.Hour)); n != 0 {
		t.Errorf("sweep removed %d clips newer than its cutoff", n)
	}
	if n := sweep(time.Now().Add(time.Minute)); n == 0 {
		t.Error("sweep left the unused clip")
	}
}

// Clips written before the shared store are adopted into it, and repeats
// collapse onto the first one adopted.
func TestAdoptSharedClips(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)
	book, chapterID := createBookAndChapter(t, s, "", 0, repeatedChapter...)
	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	clip := func(idx int) string { return audiopath.ParagraphFile(dataDir, book.ID, chapterID, voiceID, idx) }
	for _, p := range paragraphs {
		if err := audiopath.EnsureDir(clip(p.Idx)); err != nil {
			t.Fatal(err)
		}
		// Each its own file, as before the store.
		if err := os.WriteFile(clip(p.Idx), []byte{byte(p.Idx)}, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.SetParagraphReady(p.ID, voiceID, float64(p.Idx+1)); err != nil {
			t.Fatal(err)
		}
	}

	stats := mgr.AdoptSharedClips(t.Context())
	if stats.Adopted != len(paragraphs)-1 || stats.Deduplicated != 1 {
		t.Errorf("adopted %d and deduplicated %d, want %d and 1", stats.Adopted, stats.Deduplicated, len(paragraphs)-1)
	}
	_, states := clipKeys(t, s, chapterID, voiceID)
	for i, st := range states {
		if st.Status != store.AudioReady || st.ClipKey == "" {
			t.Fatalf("paragraph %d is %s on clip %q, want ready on a shared clip", i, st.Status, st.ClipKey)
		}
		if _, err := os.Stat(audiopath.SharedClipFile(dataDir, st.ClipKey)); err != nil {
			t.Errorf("paragraph %d's shared clip: %v", i, err)
		}
		if _, err := os.Stat(clip(i)); !os.IsNotExist(err) {
			t.Errorf("paragraph %d still has a file at its own path: %v", i, err)
		}
	}
	if states[0].ClipKey != states[7].ClipKey {
		t.Error("paragraph 7 wasn't put on paragraph 0's clip")
	}
	if states[7].ClipKey == states[8].ClipKey {
		t.Error("paragraphs 7 and 8, back to back, were collapsed onto one clip")
	}
	if states[7].DurationSeconds != 1 {
		t.Errorf("paragraph 7 has duration %v, want paragraph 0's 1s", states[7].DurationSeconds)
	}
	if again := mgr.AdoptSharedClips(t.Context()); again != (AdoptStats{}) {
		t.Errorf("a second run did %+v, want nothing", again)
	}
}

// The same scare-quote merge group twice in a chapter is rendered once:
// the second group's members point into the first's clip the same way.
func TestRepeatedMergeGroupsShareAClip(t *testing.T) {
	fake := ttsworkertest.New(t)
	wordConsistentTTS(t, fake)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)

	blocks := append(scareQuoteTestBlocks(), scareQuoteTestBlocks()...)
	book, chapterID := createBookWithBlocks(t, s, blocks...)
	half := len(blocks) / 2
	if err := s.SetParagraphScareQuotes(chapterID, map[int]bool{1: true, half + 1: true}); err != nil {
		t.Fatalf("SetParagraphScareQuotes: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)
	mgr.EnqueueChapter(book.ID, chapterID, 0)
	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })

	_, states := clipKeys(t, s, chapterID, voiceID)
	if states[0].ClipKey == "" || states[0].ClipKey != states[half].ClipKey {
		t.Fatalf("the two groups' anchors are on clips %q and %q, want one shared clip", states[0].ClipKey, states[half].ClipKey)
	}
	for i := 1; i <= 2; i++ {
		first, second := states[i], states[half+i]
		if second.PointerOffset != i || second.PointerSeconds != first.PointerSeconds || second.DurationSeconds != first.DurationSeconds {
			t.Errorf("member %d of the second group points %d back at %vs for %vs, want %d back at %vs for %vs",
				i, second.PointerOffset, second.PointerSeconds, second.DurationSeconds, i, first.PointerSeconds, first.DurationSeconds)
		}
	}
	merged := 0
	for _, c := range fake.GenerateCalls() {
		if c.Text == "Their so-called “expert,” had no idea what he was doing." {
			merged++
		}
	}
	if merged != 1 {
		t.Errorf("the merged text was generated %d times, want once", merged)
	}
}

// A clip kept at its own path under a voice its paragraph no longer uses
// is dropped; one under the current voice that the first adoption missed
// gets its content key.
func TestAdoptStaleClips(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)
	book, chapterID := createBookAndChapter(t, s, "", 0, "A line.")
	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	p := paragraphs[0]
	const oldVoice = "0000000000000000"
	for _, voiceID := range []string{oldVoice, bookVoice.VoiceID()} {
		own := audiopath.ParagraphFile(dataDir, book.ID, chapterID, voiceID, p.Idx)
		if err := audiopath.EnsureDir(own); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(own, []byte(voiceID), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.SetParagraphReady(p.ID, voiceID, 2); err != nil {
			t.Fatal(err)
		}
	}

	if adopted, dropped := mgr.AdoptStaleClips(t.Context()); adopted != 1 || dropped != 1 {
		t.Fatalf("adopted %d and dropped %d, want 1 and 1", adopted, dropped)
	}
	states, err := s.ParagraphAudioStatuses([]string{p.ID}, oldVoice)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := states[p.ID]; ok {
		t.Errorf("the old voice's row is still there (%s)", st.Status)
	}
	if _, err := os.Stat(audiopath.ParagraphFile(dataDir, book.ID, chapterID, oldVoice, p.Idx)); !os.IsNotExist(err) {
		t.Errorf("the old voice's clip is still on disk: %v", err)
	}
	_, current := clipKeys(t, s, chapterID, bookVoice.VoiceID())
	if current[0].Status != store.AudioReady || current[0].ClipKey == "" {
		t.Errorf("the current voice's clip is %s on %q, want ready on a content key", current[0].Status, current[0].ClipKey)
	}
	if adopted, dropped := mgr.AdoptStaleClips(t.Context()); adopted+dropped != 0 {
		t.Errorf("a second run adopted %d and dropped %d", adopted, dropped)
	}
}

// Clips an earlier version keyed privately are dropped, rows and files;
// content-keyed ones are untouched.
func TestDropUnsharedClips(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)
	_, chapterID := createBookAndChapter(t, s, "", 0, "A line.", "Another line.")
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	const voiceID = "0000000000000000"
	keys := []string{unsharedKey(paragraphs[0].ID, voiceID), "0123456789abcdef0123456789abcdef"}
	for i, p := range paragraphs {
		file := audiopath.SharedClipFile(dataDir, keys[i])
		if err := audiopath.EnsureDir(file); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.SetParagraphReadyClip(p.ID, voiceID, keys[i], 3, "", nil); err != nil {
			t.Fatal(err)
		}
	}

	if n := mgr.DropUnsharedClips(t.Context()); n != 1 {
		t.Fatalf("dropped %d clips, want the 1 keyed privately", n)
	}
	_, states := clipKeys(t, s, chapterID, voiceID)
	if states[0].Status != "" {
		t.Errorf("the privately keyed row is still there (%s)", states[0].Status)
	}
	if _, err := os.Stat(audiopath.SharedClipFile(dataDir, keys[0])); !os.IsNotExist(err) {
		t.Errorf("the privately keyed clip is still on disk: %v", err)
	}
	if orphan, err := s.ClipOrphan(keys[0]); err != nil || orphan != nil {
		t.Errorf("the dropped clip left an orphan record: %+v, %v", orphan, err)
	}
	if states[1].Status != store.AudioReady || states[1].ClipKey != keys[1] {
		t.Errorf("the content-keyed row is %s on %q, want it untouched", states[1].Status, states[1].ClipKey)
	}
	if _, err := os.Stat(audiopath.SharedClipFile(dataDir, keys[1])); err != nil {
		t.Errorf("the content-keyed clip: %v", err)
	}
	if n := mgr.DropUnsharedClips(t.Context()); n != 0 {
		t.Errorf("a second run dropped %d", n)
	}
}

// A paragraph whose clip already exists, and that waits on nothing, is
// put on it as it is enqueued: no task is queued for it at all.
func TestExistingClipResolvesAtEnqueue(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)
	book, chapterID := createBookAndChapter(t, s, "", 0, "The only paragraph.")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)
	mgr.EnqueueChapter(book.ID, chapterID, 0)
	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })
	waitIdle(t, mgr)
	_, before := clipKeys(t, s, chapterID, voiceID)

	if err := s.DeleteChapterAudio(chapterID); err != nil {
		t.Fatalf("DeleteChapterAudio: %v", err)
	}
	pending, all, err := mgr.paragraphsNeedingGeneration(ctx, book, chapterID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("paragraphsNeedingGeneration: %d pending, %v", len(pending), err)
	}
	calls := len(fake.GenerateCalls())
	if key := mgr.pushResolvedTask(book.ID, chapterID, 0, TierBackground, pending[0], all, ""); key != "" {
		t.Errorf("a task (%s) was queued for a paragraph whose clip exists", key)
	}
	_, after := clipKeys(t, s, chapterID, voiceID)
	if after[0].Status != store.AudioReady || after[0].ClipKey != before[0].ClipKey {
		t.Errorf("after enqueue the paragraph is %s on %q, want ready on %q", after[0].Status, after[0].ClipKey, before[0].ClipKey)
	}
	if got := len(fake.GenerateCalls()); got != calls {
		t.Errorf("got %d Generate calls, want %d", got, calls)
	}
}

// A merge group's clip is found again after its rows are gone, members'
// pointers and all.
func TestOrphanedMergeGroupClipIsReused(t *testing.T) {
	fake := ttsworkertest.New(t)
	wordConsistentTTS(t, fake)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)
	book, chapterID := createBookWithBlocks(t, s, scareQuoteTestBlocks()...)
	if err := s.SetParagraphScareQuotes(chapterID, map[int]bool{1: true}); err != nil {
		t.Fatalf("SetParagraphScareQuotes: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)
	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()
	narrate := func() []store.AudioState {
		t.Helper()
		waitIdle(t, mgr)
		mgr.EnqueueChapter(book.ID, chapterID, 0)
		waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })
		_, states := clipKeys(t, s, chapterID, voiceID)
		return states
	}
	first := narrate()
	waitIdle(t, mgr)
	calls := len(fake.GenerateCalls())
	if err := s.DeleteChapterAudio(chapterID); err != nil {
		t.Fatalf("DeleteChapterAudio: %v", err)
	}
	again := narrate()
	waitIdle(t, mgr)
	if got := len(fake.GenerateCalls()); got != calls {
		t.Errorf("got %d Generate calls, want %d: the group's clip should have been found", got, calls)
	}
	for i := 0; i <= 2; i++ {
		a, b := first[i], again[i]
		if b.ClipKey != a.ClipKey || b.PointerOffset != a.PointerOffset || b.PointerSeconds != a.PointerSeconds ||
			b.DurationSeconds != a.DurationSeconds || b.WordTimings != a.WordTimings {
			t.Errorf("member %d came back as %+v, want %+v", i, b, a)
		}
	}
}
