package store

import "testing"

func TestBookRecap(t *testing.T) {
	s := openTestStore(t)
	bookID, _, err := s.CreateBook("Book", "A", "en", "", "", 0, []ChapterInput{
		{Title: "One", Blocks: []BlockInput{{Kind: BlockText, Text: "Hello there."}, {Kind: BlockText, Text: "Again."}}},
		{Title: "Two"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := s.GetBookRecap(bookID); r != nil || err != nil {
		t.Fatalf("recap of a new book = %+v, %v", r, err)
	}
	in := BookRecap{BookID: bookID, Kind: RecapPosition, ChapterIdx: 1, ParagraphIdx: 4, ThroughIdx: 44, Title: "Book", Content: "So far.", ReadAt: 99}
	at, err := s.SetBookRecap(in)
	if err != nil {
		t.Fatal(err)
	}
	in.UpdatedAt, in.Segments = at, "[]"
	got, err := s.GetBookRecap(bookID)
	if err != nil || got == nil || *got != in {
		t.Fatalf("recap = %+v, %v; want %+v", got, err, in)
	}
	if !got.Covers(1, 4) || !got.Covers(1, 44) || got.Covers(1, 3) || got.Covers(1, 45) || got.Covers(0, 4) {
		t.Fatal("Covers is wrong about its own range")
	}
	// A clip is credited only to the recap it was spoken from.
	if ok, _ := s.SetBookRecapAudio(bookID, at-1, "v", 12, ""); ok {
		t.Fatal("audio recorded against a replaced recap")
	}
	if ok, err := s.SetBookRecapAudio(bookID, at, "v", 12, `[{"text":"x"}]`); !ok || err != nil {
		t.Fatalf("SetBookRecapAudio = %v, %v", ok, err)
	}
	if got, _ := s.GetBookRecap(bookID); got.AudioVoice != "v" || got.AudioSeconds != 12 || got.Segments != `[{"text":"x"}]` {
		t.Fatalf("audio = %q %v", got.AudioVoice, got.AudioSeconds)
	}

	extents, err := s.ChapterExtents(bookID)
	if err != nil || len(extents) != 2 {
		t.Fatalf("extents = %+v, %v", extents, err)
	}
	if e := extents[0]; e.Title != "One" || e.Paragraphs != 2 || e.Chars != len("Hello there.")+len("Again.") || e.Summarized {
		t.Fatalf("extent 0 = %+v", e)
	}
	if e := extents[1]; e.Paragraphs != 0 || e.Chars != 0 {
		t.Fatalf("extent 1 = %+v", e)
	}
	if err := s.SetChapterNotes(extents[0].ID, ChapterNotes{Summary: "x"}); err != nil {
		t.Fatal(err)
	}
	if extents, _ = s.ChapterExtents(bookID); !extents[0].Summarized || extents[1].Summarized {
		t.Fatalf("summarized = %v, %v", extents[0].Summarized, extents[1].Summarized)
	}

	if err := s.DeleteBook(bookID); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetBookRecap(bookID); r != nil {
		t.Fatal("recap outlived its book")
	}
}
