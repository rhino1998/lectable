package httpapi

import (
	"slices"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// A whole-library run keeps each series together and in order in one lane,
// since its books share a roster; a book in no series is a lane of its own.
func TestLibraryPreprocessOrder(t *testing.T) {
	got := libraryPreprocessOrder([]store.Book{
		{ID: "alone-new", AddedAt: 9},
		{ID: "b2", SeriesName: "B", SeriesIndex: 2, AddedAt: 1},
		{ID: "a1", SeriesName: "A", SeriesIndex: 1, AddedAt: 8},
		{ID: "alone-old", AddedAt: 2},
		{ID: "b1", SeriesName: "B", SeriesIndex: 1, AddedAt: 7},
	})
	want := [][]string{{"a1"}, {"b1", "b2"}, {"alone-old"}, {"alone-new"}}
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
