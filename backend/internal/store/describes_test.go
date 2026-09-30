package store

import (
	"reflect"
	"testing"
)

func describedNames(t *testing.T, s *DuckStore, chapterID string) [][]string {
	t.Helper()
	paras, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]string, len(paras))
	for i, p := range paras {
		out[i] = append([]string{}, p.DescribesCharacters...)
	}
	return out
}

func TestReassignCharacterDescriptions(t *testing.T) {
	s := openTestStore(t)
	bookID, chapterID := oneChapterBook(t, s, "", 0,
		"Lars was tall.", "Lars and Mia were tired.", "Mia and Lars Machmüller argued.", "Nothing here.")
	if err := s.SetParagraphDescriptions(chapterID, map[int][]string{
		0: {"Lars"}, 1: {"Lars", "Mia"}, 2: {"Mia", "Lars Machmüller", "Lars"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.ReassignCharacterDescriptions(bookID, "Lars", "Lars Machmüller"); err != nil {
		t.Fatalf("ReassignCharacterDescriptions: %v", err)
	}
	want := [][]string{{"Lars Machmüller"}, {"Lars Machmüller", "Mia"}, {"Mia", "Lars Machmüller"}, {}}
	if got := describedNames(t, s, chapterID); !reflect.DeepEqual(got, want) {
		t.Fatalf("after merge = %q, want %q", got, want)
	}

	// "" drops the name.
	if err := s.ReassignCharacterDescriptions(bookID, "Mia", ""); err != nil {
		t.Fatal(err)
	}
	want = [][]string{{"Lars Machmüller"}, {"Lars Machmüller"}, {"Lars Machmüller"}, {}}
	if got := describedNames(t, s, chapterID); !reflect.DeepEqual(got, want) {
		t.Fatalf("after drop = %q, want %q", got, want)
	}
}

func TestRepairDescribedNames(t *testing.T) {
	s := openTestStore(t)
	bookID, chapterID := oneChapterBook(t, s, "", 0, "Lars was tall.", "Al and Stranger met.", "Bo sat.")
	scope := "book:" + bookID
	lars, _, err := s.UpsertCharacter(scope, "Lars Machmüller", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCharacterAliases(lars.ID, []string{"Lars", "Al"}); err != nil {
		t.Fatal(err)
	}
	// "Al" is also Albert's alias - ambiguous, so it's left alone. "Bo" is
	// a live character's name as well as an alias - also left alone.
	albert, _, err := s.UpsertCharacter(scope, "Albert", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCharacterAliases(albert.ID, []string{"Al", "Bo"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpsertCharacter(scope, "Bo", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphDescriptions(chapterID, map[int][]string{
		0: {"Lars"}, 1: {"Al", "Stranger"}, 2: {"Bo"},
	}); err != nil {
		t.Fatal(err)
	}

	n, err := s.RepairDescribedNames()
	if err != nil || n != 1 {
		t.Fatalf("RepairDescribedNames = %d, %v; want 1", n, err)
	}
	want := [][]string{{"Lars Machmüller"}, {"Al", "Stranger"}, {"Bo"}}
	if got := describedNames(t, s, chapterID); !reflect.DeepEqual(got, want) {
		t.Fatalf("after repair = %q, want %q", got, want)
	}
	if n, err := s.RepairDescribedNames(); err != nil || n != 0 {
		t.Fatalf("second RepairDescribedNames = %d, %v; want 0", n, err)
	}
}
