package httpapi

import (
	"net/http"
	"slices"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// TestGlobalFacts checks /api/facts end to end, and that casting is shown
// the facts a character's material brings up, a series' own fact in place
// of the global one for the same key.
func TestGlobalFacts(t *testing.T) {
	srv, s, _, ts := newTestServer(t)

	// A new library starts with the baseline facts (migration 00026).
	var listed []worldFactDTO
	doJSON(t, http.MethodGet, ts.URL+"/api/facts", nil, &listed)
	if !slices.ContainsFunc(listed, func(f worldFactDTO) bool { return f.Key == "dwarf" && slices.Contains(f.Aliases, "dwarven") }) {
		t.Fatalf("baseline facts = %+v, want a dwarf fact", listed)
	}
	for _, f := range listed {
		doJSON(t, http.MethodDelete, ts.URL+"/api/facts/"+f.ID, nil, nil)
	}

	var created idResponse
	resp := doJSON(t, http.MethodPut, ts.URL+"/api/facts", upsertWorldFactRequest{Key: " Dwarf ", Aliases: []string{"dwarven", "dwarf", " "}, Fact: "Scottish accent."}, &created)
	if resp.StatusCode != http.StatusOK || created.ID == "" {
		t.Fatalf("PUT status %d id %q", resp.StatusCode, created.ID)
	}
	doJSON(t, http.MethodPut, ts.URL+"/api/facts", upsertWorldFactRequest{Key: "elf", Fact: "Refined RP."}, nil)
	if resp := doJSON(t, http.MethodPut, ts.URL+"/api/facts", upsertWorldFactRequest{Key: "elf"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT without a fact: status %d", resp.StatusCode)
	}
	if resp := doJSON(t, http.MethodPut, ts.URL+"/api/facts", upsertWorldFactRequest{ID: created.ID, Key: "Elf", Fact: "x"}, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("PUT onto a taken key: status %d", resp.StatusCode)
	}

	doJSON(t, http.MethodGet, ts.URL+"/api/facts", nil, &listed)
	if len(listed) != 2 || listed[0].Key != "Dwarf" || !slices.Equal(listed[0].Aliases, []string{"dwarven"}) || listed[1].Aliases == nil {
		t.Fatalf("GET = %+v", listed)
	}

	bookID := createTestBook(t, s, "Tower", 1, "Borin, a dwarven smith, looked up.")
	book, err := s.GetBook(bookID)
	if err != nil || book == nil {
		t.Fatalf("GetBook: %v", err)
	}
	char := store.Character{Name: "Borin"}
	evidence := speakerattr.CharacterEvidence{Introductions: []string{"Borin, a dwarven smith, looked up."}}
	notes, err := srv.worldNotes(book, char, evidence)
	if err != nil || !slices.Equal(notes, []string{"Dwarf: Scottish accent."}) {
		t.Fatalf("worldNotes = %q, %v", notes, err)
	}
	var own idResponse
	if resp := doJSON(t, http.MethodPut, ts.URL+"/api/books/"+bookID+"/facts", upsertWorldFactRequest{Key: "dwarf", Fact: "Welsh accent."}, &own); resp.StatusCode != http.StatusOK || own.ID == "" {
		t.Fatalf("PUT book fact: status %d id %q", resp.StatusCode, own.ID)
	}
	// The book lists its series' own fact, then the global ones, the
	// dwarf one superseded.
	var bookFacts []worldFactDTO
	doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/facts", nil, &bookFacts)
	if len(bookFacts) != 3 || bookFacts[0].Global || bookFacts[0].Fact != "Welsh accent." ||
		!bookFacts[1].Global || !bookFacts[1].Superseded || !bookFacts[2].Global || bookFacts[2].Superseded {
		t.Fatalf("book facts = %+v", bookFacts)
	}
	// A global fact can't be removed through a book.
	doJSON(t, http.MethodDelete, ts.URL+"/api/books/"+bookID+"/facts/"+created.ID, nil, nil)
	if doJSON(t, http.MethodGet, ts.URL+"/api/facts", nil, &listed); len(listed) != 2 {
		t.Fatalf("global facts after a book delete = %+v", listed)
	}
	notes, err = srv.worldNotes(book, char, evidence)
	if err != nil || !slices.Equal(notes, []string{"dwarf: Welsh accent."}) {
		t.Fatalf("worldNotes with a series fact = %q, %v", notes, err)
	}

	if resp := doJSON(t, http.MethodDelete, ts.URL+"/api/books/"+bookID+"/facts/"+own.ID, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE book fact: status %d", resp.StatusCode)
	}
	if notes, err = srv.worldNotes(book, char, evidence); err != nil || !slices.Equal(notes, []string{"Dwarf: Scottish accent."}) {
		t.Fatalf("worldNotes after removing the series fact = %q, %v", notes, err)
	}
	if resp := doJSON(t, http.MethodDelete, ts.URL+"/api/facts/"+created.ID, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status %d", resp.StatusCode)
	}
	doJSON(t, http.MethodGet, ts.URL+"/api/facts", nil, &listed)
	if len(listed) != 1 || listed[0].Key != "elf" {
		t.Fatalf("GET after DELETE = %+v", listed)
	}
}

// TestGlobalLexicon checks /api/lexicon: a rule saved there applies to
// every book, resets the audio it matches, and a series' own rule can't be
// removed through it.
func TestGlobalLexicon(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "Nyx drew her blade.")

	var created idResponse
	if resp := doJSON(t, http.MethodPut, ts.URL+"/api/lexicon", upsertLexiconRequest{Term: "Nyx", Replacement: "Nicks"}, &created); resp.StatusCode != http.StatusOK || created.ID == "" {
		t.Fatalf("PUT status %d id %q", resp.StatusCode, created.ID)
	}
	var listed []globalLexiconEntryDTO
	doJSON(t, http.MethodGet, ts.URL+"/api/lexicon", nil, &listed)
	if len(listed) != 1 || listed[0].Term != "Nyx" {
		t.Fatalf("GET = %+v", listed)
	}
	lex, err := srv.buildLexicon(bookID)
	if err != nil || len(lex) != 1 || !lex[0].Global || lex[0].Matches != 1 {
		t.Fatalf("book lexicon = %+v, %v", lex, err)
	}

	book, _ := s.GetBook(bookID)
	own, _, err := s.UpsertLexiconEntry(store.SeriesScope(book), pronounce.LexiconEntry{Term: "blade", Replacement: "blayd"})
	if err != nil {
		t.Fatal(err)
	}
	doJSON(t, http.MethodDelete, ts.URL+"/api/lexicon/"+own.ID, nil, nil)
	doJSON(t, http.MethodDelete, ts.URL+"/api/lexicon/"+created.ID, nil, nil)
	lex, err = srv.buildLexicon(bookID)
	if err != nil || len(lex) != 1 || lex[0].Term != "blade" {
		t.Fatalf("book lexicon after deletes = %+v, %v", lex, err)
	}
}
