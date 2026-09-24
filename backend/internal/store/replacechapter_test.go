package store

import "testing"

// TestReplaceChapterContent covers a single-chapter re-import: new
// paragraphs replace the old ones under the same chapter row (same
// (chapter_id, idx) keys deleted and re-inserted in one transaction),
// attribution and pass flags are gone, and a bookmark follows its idx.
func TestReplaceChapterContent(t *testing.T) {
	s := openTestStore(t)
	bookID, chapterID := oneChapterBook(t, s, "", 0, "one", "two", "three", "four")

	if err := s.SetParagraphSpeakers(chapterID, map[int]string{1: "Alice"}); err != nil {
		t.Fatalf("SetParagraphSpeakers: %v", err)
	}
	if err := s.SetChapterAttributed(chapterID); err != nil {
		t.Fatalf("SetChapterAttributed: %v", err)
	}
	old, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	if _, err := s.UpsertBookmark(old[3].ID, "keep me"); err != nil {
		t.Fatalf("UpsertBookmark: %v", err)
	}

	blocks := []BlockInput{
		{Kind: BlockText, Text: "“Hi,”", IsQuote: true},
		{Kind: BlockText, Text: "she said.", Inline: true},
		{Kind: BlockBreak},
		{Kind: BlockText, Text: "The end."},
	}
	oldImages, newImages, err := s.ReplaceChapterContent(chapterID, "Chapter One (fixed)", blocks)
	if err != nil {
		t.Fatalf("ReplaceChapterContent: %v", err)
	}
	if len(oldImages) != 0 || len(newImages) != 0 {
		t.Fatalf("unexpected images: old=%v new=%v", oldImages, newImages)
	}

	got, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 paragraphs, got %d", len(got))
	}
	if got[0].Text != "“Hi,”" || !got[0].IsQuote || !got[1].Inline || got[2].Text != "The end." {
		t.Fatalf("unexpected paragraphs: %+v", got)
	}
	for _, p := range got {
		if p.Speaker != "" {
			t.Errorf("paragraph %d kept speaker %q", p.Idx, p.Speaker)
		}
	}

	ch, err := s.GetChapterByID(chapterID)
	if err != nil || ch == nil {
		t.Fatalf("GetChapterByID: %v", err)
	}
	if ch.Title != "Chapter One (fixed)" {
		t.Errorf("title = %q", ch.Title)
	}
	if ch.Passes != (Passes{}) {
		t.Errorf("passes not reset: %+v", ch.Passes)
	}

	bms, err := s.ListBookmarks(bookID)
	if err != nil {
		t.Fatalf("ListBookmarks: %v", err)
	}
	if len(bms) != 1 || bms[0].ParagraphIdx != 2 || bms[0].Note != "keep me" {
		t.Fatalf("bookmark not re-pointed to the clamped idx: %+v", bms)
	}
}
