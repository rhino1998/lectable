package store

import (
	"slices"
	"testing"
)

func TestChapterMatter(t *testing.T) {
	s := openTestStore(t)
	text := func(lines ...string) []BlockInput {
		var out []BlockInput
		for _, l := range lines {
			out = append(out, BlockInput{Kind: BlockText, Text: l})
		}
		return out
	}
	bookID, _, err := s.CreateBook("Book", "A", "en", "", "", 0, []ChapterInput{
		{Title: "Title Page", Blocks: text("Title Page", "Book")},
		{Title: "Chapter One", Blocks: text("Chapter One", "It began.", "It went on.")},
		{Title: "Post-Credits Scene", Blocks: text("One year later.")},
		{Title: "About the Author", Blocks: text("She writes.")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if need, err := s.BookNeedsMatterPass(bookID); !need || err != nil {
		t.Fatalf("a new book needs its pass: %v, %v", need, err)
	}
	if ids, err := s.BooksNeedingMatterPass(); err != nil || !slices.Equal(ids, []string{bookID}) {
		t.Fatalf("BooksNeedingMatterPass = %v, %v", ids, err)
	}
	sections, err := s.MatterSections(bookID)
	if err != nil || len(sections) != 4 {
		t.Fatalf("MatterSections = %+v, %v", sections, err)
	}
	if one := sections[1]; one.Title != "Chapter One" || one.Chars != len("Chapter One"+"It began."+"It went on.") || !slices.Equal(one.Opening, []string{"Chapter One", "It began.", "It went on."}) {
		t.Fatalf("section 1 = %+v", one)
	}

	// The reader says the bonus scene is story before the pass has run;
	// the pass, calling it matter, doesn't replace that.
	if err := s.SetChapterMatter(sections[2].ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBookMatter(bookID, []string{sections[0].ID, sections[2].ID, sections[3].ID}); err != nil {
		t.Fatal(err)
	}
	matter := func() []bool {
		chapters, err := s.ListChapterSummaries(bookID, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []bool
		for _, c := range chapters {
			out = append(out, c.Matter)
		}
		return out
	}
	if got := matter(); !slices.Equal(got, []bool{true, false, false, true}) {
		t.Fatalf("matter after the pass = %v", got)
	}
	if need, _ := s.BookNeedsMatterPass(bookID); need {
		t.Fatal("a judged book doesn't need its pass")
	}
	if ch, _ := s.GetChapterByIdx(bookID, 2); ch.Matter || ch.MatterBy != MatterByReader {
		t.Fatalf("chapter 2 = %+v", ch)
	}
	if err := s.SetChapterMatter(sections[0].ID, false); err != nil {
		t.Fatal(err)
	}
	// A pass finding nothing clears what an earlier one set, not the reader's.
	if err := s.SetBookMatter(bookID, nil); err != nil {
		t.Fatal(err)
	}
	if got := matter(); slices.Contains(got, true) {
		t.Fatalf("matter after an empty pass = %v", got)
	}
	extents, err := s.ChapterExtents(bookID)
	if err != nil || len(extents) != 4 || extents[3].Matter {
		t.Fatalf("extents = %+v, %v", extents, err)
	}
}
