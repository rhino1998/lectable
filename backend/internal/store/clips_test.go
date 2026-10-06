package store

import (
	"path/filepath"
	"testing"
)

// A merge group anchor on a shared clip gets its members' parts from their
// pointer rows, once.
func TestBackfillClipParts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "library.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	blocks := make([]BlockInput, 4)
	for i := range blocks {
		blocks[i] = BlockInput{Kind: BlockText, Text: "Paragraph."}
	}
	bookID, _, err := s.CreateBook("Book", "Author", "en", "", "", 0, []ChapterInput{{Title: "One", Blocks: blocks}})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	ch, err := s.GetChapterByIdx(bookID, 0)
	if err != nil || ch == nil {
		t.Fatalf("GetChapterByIdx: %v", err)
	}
	ps, err := s.ListParagraphsRaw(ch.ID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	const voice = "v"
	// Paragraphs 0-2 are one group on a shared clip; 3 is on its own.
	if err := s.SetParagraphReadyClip(ps[0].ID, voice, "k-group", 1, "[]", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphReadyPointer(ps[1].ID, voice, 1, 1.5, 0.5); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphReadyPointer(ps[2].ID, voice, 2, 2, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphReadyClip(ps[3].ID, voice, "k-single", 1, "[]", nil); err != nil {
		t.Fatal(err)
	}

	if n, err := s.BackfillClipParts(); err != nil || n != 1 {
		t.Fatalf("BackfillClipParts = %d, %v; want 1", n, err)
	}
	// The parts ride along when the group's rows go.
	if err := s.DeleteChapterAudio(ch.ID); err != nil {
		t.Fatal(err)
	}
	orphan, err := s.ClipOrphan("k-group")
	if err != nil || orphan == nil {
		t.Fatalf("ClipOrphan: %+v, %v", orphan, err)
	}
	want := []ClipPart{{PointerSeconds: 1.5, DurationSeconds: 0.5, WordTimings: "[]"}, {PointerSeconds: 2, DurationSeconds: 3, WordTimings: "[]"}}
	if len(orphan.Parts) != 2 || orphan.Parts[0] != want[0] || orphan.Parts[1] != want[1] {
		t.Errorf("orphaned group's parts = %+v, want %+v", orphan.Parts, want)
	}
	if single, err := s.ClipOrphan("k-single"); err != nil || single == nil || len(single.Parts) != 0 {
		t.Errorf("the single clip's orphan = %+v, %v; want one with no parts", single, err)
	}
	if n, err := s.BackfillClipParts(); err != nil || n != 0 {
		t.Errorf("a second BackfillClipParts = %d, %v; want 0", n, err)
	}
}
