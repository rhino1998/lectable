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

// A shared clip is only reused while a paragraph still uses it: clearing
// the audio renders it again, and the sweep then drops the unused clip.
func TestClearedSharedClipIsRenderedAgainAndSwept(t *testing.T) {
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

	_, states := clipKeys(t, s, chapterID, voiceID)
	key := states[0].ClipKey
	if key == "" {
		t.Fatal("the paragraph isn't on a shared clip")
	}
	sweep := func(cutoff time.Time) int {
		inUse, err := s.ClipKeys()
		if err != nil {
			t.Fatalf("ClipKeys: %v", err)
		}
		return audiomaint.SweepSharedClips(ctx, dataDir, func(key string) bool { return !inUse[key] }, cutoff).Removed
	}
	if n := sweep(time.Now().Add(time.Minute)); n != 0 {
		t.Errorf("sweep removed %d clips still in use", n)
	}

	// What "clear generation" does to the rows.
	if err := s.DeleteChapterAudio(chapterID); err != nil {
		t.Fatalf("DeleteChapterAudio: %v", err)
	}
	if n := sweep(time.Now().Add(-time.Hour)); n != 0 {
		t.Errorf("sweep removed %d clips newer than its cutoff", n)
	}

	calls := len(fake.GenerateCalls())
	mgr.EnqueueChapter(book.ID, chapterID, 0)
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })
	if got := len(fake.GenerateCalls()); got != calls+1 {
		t.Errorf("got %d Generate calls after clearing, want %d: a clip nothing uses must not be reused", got, calls+1)
	}

	if err := s.DeleteChapterAudio(chapterID); err != nil {
		t.Fatalf("DeleteChapterAudio: %v", err)
	}
	if n := sweep(time.Now().Add(time.Minute)); n == 0 {
		t.Fatal("sweep left the unused clip")
	}
	if _, err := os.Stat(audiopath.SharedClipFile(dataDir, key)); !os.IsNotExist(err) {
		t.Errorf("swept clip still on disk: %v", err)
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
