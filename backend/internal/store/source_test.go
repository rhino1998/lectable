package store

import (
	"path/filepath"
	"testing"
)

func TestBookSourceAndAppendChapters(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "library.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	text := func(lines ...string) []BlockInput {
		var out []BlockInput
		for _, l := range lines {
			out = append(out, BlockInput{Kind: BlockText, Text: l})
		}
		return out
	}
	bookID, _, err := s.CreateBook("Serial", "A", "en", "", "", 0, []ChapterInput{{Title: "One", Blocks: text("One", "a"), SourceID: "101"}})
	if err != nil {
		t.Fatal(err)
	}
	if srcs, err := s.BookSources(bookID); err != nil || len(srcs) != 0 {
		t.Fatalf("new book's sources = %+v, %v; want none", srcs, err)
	}
	want := BookSource{BookID: bookID, Kind: "royalroad", SourceID: "7", URL: "https://example.test/fiction/7", CheckedAt: 123, RemoteChapters: 3, Error: "boom", Blocked: "Four"}
	if err := s.SaveBookSource(&want); err != nil || want.ID == "" {
		t.Fatalf("SaveBookSource: id %q, %v", want.ID, err)
	}
	second := BookSource{BookID: bookID, Kind: "patreon", SourceID: "55", Prefix: "patreon:"}
	if err := s.SaveBookSource(&second); err != nil {
		t.Fatal(err)
	}
	want.Blocked, want.CheckedAt = "", 456
	if err := s.SaveBookSource(&want); err != nil {
		t.Fatal(err)
	}
	if got, err := s.BookSources(bookID); err != nil || len(got) != 2 || got[0] != want || got[1] != second {
		t.Fatalf("BookSources = %+v, %v", got, err)
	}
	if got, err := s.GetBookSource(second.ID); err != nil || got == nil || *got != second {
		t.Fatalf("GetBookSource = %+v, %v", got, err)
	}
	if err := s.SetBookSourcePlan(second.ID, SourcePlan{Matched: []string{"patreon:1"}}); err != nil {
		t.Fatal(err)
	}
	if plan, err := s.GetBookSourcePlan(second.ID); err != nil || len(plan.Matched) != 1 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if plan, _ := s.GetBookSourcePlan(want.ID); len(plan.Matched) != 0 {
		t.Fatalf("the other source's plan = %+v", plan)
	}
	if books, err := s.SourcedBooks(); err != nil || len(books) != 1 || books[0].CheckedAt != 0 {
		t.Fatalf("SourcedBooks = %+v, %v", books, err)
	}
	if err := s.DeleteBookSource(second.ID); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.FindBookBySource("patreon", "55"); id != "" {
		t.Fatal("a removed source still finds its book")
	}
	if id, err := s.FindBookBySource("royalroad", "7"); err != nil || id != bookID {
		t.Fatalf("FindBookBySource = %q, %v", id, err)
	}
	if id, err := s.FindBookBySource("royalroad", "8"); err != nil || id != "" {
		t.Fatalf("FindBookBySource(other) = %q, %v", id, err)
	}

	images, err := s.AppendChapters(bookID, []ChapterInput{
		{Title: "Two", Blocks: append(text("Two", "b"), BlockInput{Kind: BlockImage, Ext: ".png"}), SourceID: "102"},
		{Title: "Three", Blocks: text("Three"), SourceID: "103"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].Ext != ".png" {
		t.Fatalf("images = %+v", images)
	}
	ids, err := s.ChapterSourceIDs(bookID)
	if err != nil || len(ids) != 3 || ids[0] != "101" || ids[1] != "102" || ids[2] != "103" {
		t.Fatalf("ChapterSourceIDs = %v, %v", ids, err)
	}
	for idx, title := range []string{"One", "Two", "Three"} {
		ch, err := s.GetChapterByIdx(bookID, idx)
		if err != nil || ch == nil || ch.Title != title {
			t.Fatalf("chapter %d = %+v, %v; want %q", idx, ch, err, title)
		}
	}
	two, _ := s.GetChapterByIdx(bookID, 1)
	if images[0].ChapterID != two.ID {
		t.Errorf("image belongs to %s, want chapter Two (%s)", images[0].ChapterID, two.ID)
	}
	if _, err := s.AppendChapters("nope", []ChapterInput{{Title: "X"}}); err == nil {
		t.Error("appending to a missing book succeeded")
	}

	if err := s.DeleteBook(bookID); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.FindBookBySource("royalroad", "7"); id != "" {
		t.Errorf("deleted book still found by its source")
	}
}
