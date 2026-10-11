package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsworker/ttsworkertest"
)

// TestGenerateBookLeavesOutMatter: "generate the whole book" first has the
// book's matter pass run, then narrates only what it didn't mark - and the
// pass is run once, not again for the next request.
func TestGenerateBookLeavesOutMatter(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	mgr := NewManager(s, fake.Manager(), t.TempDir())

	one := func(text string) []store.BlockInput { return []store.BlockInput{{Kind: store.BlockText, Text: text}} }
	bookID, _, err := s.CreateBook("Book", "Author", "en", "", "", 0, []store.ChapterInput{
		{Title: "Title Page", Blocks: one("Book, by Author.")},
		{Title: "Chapter One", Blocks: one("It began.")},
		{Title: "About the Author", Blocks: one("She writes.")},
	})
	if err != nil {
		t.Fatal(err)
	}
	chapters, err := s.ListChapterSummaries(bookID, "", nil)
	if err != nil || len(chapters) != 3 {
		t.Fatalf("chapters: %v, %v", chapters, err)
	}
	runs := 0
	mgr.SetBookMatterFinder(func(_ context.Context, book *store.Book) error {
		runs++
		return s.SetBookMatter(book.ID, []string{chapters[0].ID, chapters[2].ID})
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)
	book, _ := s.GetBook(bookID)
	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatal(err)
	}
	voiceID := bookVoice.VoiceID()

	mgr.EnqueueBookGenerate(bookID)
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapters[1].ID, voiceID) })
	waitFor(t, 5*time.Second, func() bool {
		inFlight, queued := mgr.Snapshot()
		return len(inFlight) == 0 && len(queued) == 0
	})
	for _, i := range []int{0, 2} {
		if allParagraphsReady(t, s, chapters[i].ID, voiceID) {
			t.Errorf("%q was narrated", chapters[i].Title)
		}
	}
	mgr.EnsureBookMatter(ctx, bookID, TierBackground)
	if runs != 1 {
		t.Fatalf("the matter pass ran %d times", runs)
	}

	// A reader who opens the title page has it read.
	mgr.EnqueueLookahead(bookID, "", 0, 0, 5)
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapters[0].ID, voiceID) })
	if allParagraphsReady(t, s, chapters[2].ID, voiceID) {
		t.Error("the lookahead ran on into the back matter")
	}
}
