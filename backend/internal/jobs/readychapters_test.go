package jobs

import (
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
)

func TestReadyChaptersForgetOnlyWhatAWriteCouldChange(t *testing.T) {
	var r readyChapters
	known := func(chapterID string) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, ok := r.chapters[chapterID]
		return ok
	}
	fill := func() {
		r.put(r.gen, "bookA", "a1", 10)
		r.put(r.gen, "bookB", "b1", 20)
	}

	fill()
	r.changed([]string{store.PositionTable})
	r.changed([]string{"jobs_log"})
	if !known("a1") || !known("b1") {
		t.Fatal("a saved position or an unrelated table forgot a chapter")
	}

	r.changed([]string{"paragraph_audio", store.BookScope("bookA")})
	if known("a1") || !known("b1") {
		t.Fatalf("a clip in bookA: a1 known=%v, b1 known=%v; want false, true", known("a1"), known("b1"))
	}

	fill()
	r.changed([]string{"character_voices"})
	if known("a1") || known("b1") {
		t.Fatal("a voice reassignment left chapters remembered")
	}

	// An answer worked out before a write isn't one to keep.
	stale := r.gen
	r.changed([]string{store.AllTables})
	r.put(stale, "bookA", "a1", 10)
	if known("a1") {
		t.Fatal("kept an answer from before a write")
	}
}
