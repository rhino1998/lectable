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

// TestLibraryLaneFeedTakesInNewBooks: once every lane has been handed out,
// the feed reads the library again and hands out the books added since -
// but never a lane of a series while another lane of it is running.
func TestLibraryLaneFeedTakesInNewBooks(t *testing.T) {
	library := []store.Book{
		{ID: "a1", SeriesName: "A", SeriesIndex: 1},
		{ID: "x", AddedAt: 1},
	}
	list := func() ([]store.Book, error) { return library, nil }
	feed := newLibraryLaneFeed(library)

	ids, series, ok := feed.take(list)
	if !ok || !slices.Equal(ids, []string{"a1"}) || series != "A" {
		t.Fatalf("first take = %v %q %v", ids, series, ok)
	}
	if ids, _, ok := feed.take(list); !ok || !slices.Equal(ids, []string{"x"}) {
		t.Fatalf("second take = %v %v", ids, ok)
	}

	// Books uploaded while the run is going: another of series A, whose
	// lane is still running, and a new series.
	library = append(library,
		store.Book{ID: "a2", SeriesName: "A", SeriesIndex: 2},
		store.Book{ID: "b1", SeriesName: "B", SeriesIndex: 1},
		store.Book{ID: "b2", SeriesName: "B", SeriesIndex: 2},
	)
	if ids, series, ok := feed.take(list); !ok || !slices.Equal(ids, []string{"b1", "b2"}) || series != "B" {
		t.Fatalf("take after upload = %v %q %v, want series B's new books", ids, series, ok)
	}
	if ids, _, ok := feed.take(list); ok {
		t.Fatalf("took %v while series A's lane is still running", ids)
	}
	feed.done("A")
	if ids, _, ok := feed.take(list); !ok || !slices.Equal(ids, []string{"a2"}) {
		t.Fatalf("take after series A's lane finished = %v %v", ids, ok)
	}
	feed.done("B")
	if ids, _, ok := feed.take(list); ok {
		t.Fatalf("took %v with every book handed out", ids)
	}
}
