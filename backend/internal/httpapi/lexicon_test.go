package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// TestLexiconUpsertInvalidatesMatchesAndMarksThem checks PUT/DELETE
// .../lexicon end to end: saving a rule resets audio only for paragraphs it
// matches, shows as a pronunciation mark on them, and deleting it resets
// them again.
func TestLexiconUpsertInvalidatesMatchesAndMarksThem(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "Nyx drew her blade.", "The onyx gleamed.")
	book, err := s.GetBook(bookID)
	if err != nil || book == nil {
		t.Fatalf("GetBook: %v", err)
	}
	ch, err := s.GetChapterByIdx(bookID, 0)
	if err != nil || ch == nil {
		t.Fatalf("GetChapterByIdx: %v", err)
	}
	voice, err := srv.Narration.ForParagraph(book, "", 0, narration.NoChapter)
	if err != nil {
		t.Fatalf("ForParagraph: %v", err)
	}
	markReady := func() []store.Paragraph {
		t.Helper()
		ps, err := s.ListParagraphsRaw(ch.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if err := s.SetParagraphReady(p.ID, voice.VoiceID(), 1); err != nil {
				t.Fatal(err)
			}
		}
		return ps
	}
	statuses := func(ps []store.Paragraph) []string {
		t.Helper()
		out := make([]string, len(ps))
		for i, p := range ps {
			st, err := s.GetParagraphAudioStatus(p.ID, voice.VoiceID())
			if err != nil {
				t.Fatal(err)
			}
			out[i] = st
		}
		return out
	}
	ps := markReady()

	body, _ := json.Marshal(upsertLexiconRequest{Term: "nyx", Replacement: "Nicks"})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/books/"+bookID+"/lexicon", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created idResponse
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || created.ID == "" {
		t.Fatalf("PUT status %d id %q", resp.StatusCode, created.ID)
	}
	if got := statuses(ps); got[0] == store.AudioReady || got[1] != store.AudioReady {
		t.Fatalf("after PUT statuses = %v, want only the matching paragraph reset (\"onyx\" isn't a whole-word match)", got)
	}

	detail, err := srv.buildChapter(bookID, 0)
	if err != nil {
		t.Fatal(err)
	}
	marks := detail.Paragraphs[0].PronunciationMarks
	if len(marks) != 1 || marks[0].Original != "Nyx" || marks[0].Replacement != "Nicks" {
		t.Fatalf("marks = %+v", marks)
	}
	lex, err := srv.buildLexicon(bookID)
	if err != nil || len(lex) != 1 || lex[0].Matches != 1 {
		t.Fatalf("lexicon = %+v, %v", lex, err)
	}

	markReady()
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/api/books/"+bookID+"/lexicon/"+created.ID, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status %d", resp.StatusCode)
	}
	if got := statuses(ps); got[0] == store.AudioReady || got[1] != store.AudioReady {
		t.Fatalf("after DELETE statuses = %v", got)
	}
}

func TestLexiconPreviewText(t *testing.T) {
	ps := []store.Paragraph{
		{Text: "Nothing here."},
		{Text: "The gate opened. Nyx stepped through, blade drawn. Then silence."},
	}
	rule := pronounce.LexiconEntry{Term: "nyx", Replacement: "Nicks"}
	if got, want := lexiconPreviewText(ps, rule), "Nicks stepped through, blade drawn."; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
	if got := lexiconPreviewText(ps[:1], rule); got != "Nicks" {
		t.Errorf("no match: = %q, want the replacement alone", got)
	}
	long := strings.Repeat("word ", 100) + "Nyx " + strings.Repeat("word ", 100) + "end."
	if got := lexiconPreviewText([]store.Paragraph{{Text: long}}, rule); len(got) > lexiconPreviewMaxChars+10 || !strings.Contains(got, "Nicks") {
		t.Errorf("long sentence not windowed around the match: %d chars", len(got))
	}
}

func TestNameWordsAndUnusualSpelling(t *testing.T) {
	if got := nameWords("Lady Aeris Vane-Holt, of the Vale"); !reflect.DeepEqual(got, []string{"Lady", "Aeris", "Vane-Holt", "Vale"}) {
		t.Errorf("nameWords = %q", got)
	}
	for _, w := range []string{"Aeris", "Nyx", "Qarth", "Zhao", "Éowyn", "T'Challa"} {
		if !unusualSpelling(w) {
			t.Errorf("%q should look unusual", w)
		}
	}
	for _, w := range []string{"Davos", "Knight", "Robert", "Stark", "Alice"} {
		if unusualSpelling(w) {
			t.Errorf("%q should look ordinary", w)
		}
	}
}

func TestLexiconSuggestionsSkipCoveredAndCommonWords(t *testing.T) {
	srv, s, _, _ := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "Hello.")
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	for _, name := range []string{"Lady Aeris", "Davos", "Nyx"} {
		if _, _, err := s.UpsertCharacter(scope, name, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.UpsertLexiconEntry(scope, pronounce.LexiconEntry{Term: "nyx", Replacement: "Nicks"}); err != nil {
		t.Fatal(err)
	}
	got, err := srv.buildLexiconSuggestions(bookID)
	if err != nil {
		t.Fatal(err)
	}
	var terms []string
	for _, g := range got {
		terms = append(terms, g.Term)
	}
	if !reflect.DeepEqual(terms, []string{"Aeris", "Davos"}) {
		t.Errorf("suggestions = %q, want unusual Aeris first, no Lady (title) or Nyx (has a rule)", terms)
	}
}

func TestCharacterNamesAlphabetical(t *testing.T) {
	srv, s, _, _ := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "Hello.")
	book, _ := s.GetBook(bookID)
	for _, name := range []string{"zed", "Amy", "bob"} {
		if _, _, err := s.UpsertCharacter(store.SeriesScope(book), name, false); err != nil {
			t.Fatal(err)
		}
	}
	got, err := srv.buildCharacterNames(bookID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, []string{"Amy", "bob", "zed"}) {
		t.Fatalf("names = %q", names)
	}
}

// TestGlobalPrefixLexiconRule checks a global rule with Prefix set: it
// applies to every book, covers derived words, invalidates their audio in
// each book, and lists as global.
func TestGlobalPrefixLexiconRule(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	bookA := createTestBook(t, s, "Series A", 1, "The gaoler slept.", "Nothing here.")
	bookB := createTestBook(t, s, "", 0, "Back to gaol.")
	body, _ := json.Marshal(upsertLexiconRequest{Term: "gaol", Replacement: "jail", Prefix: true, Global: true})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/books/"+bookA+"/lexicon", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status %d", resp.StatusCode)
	}
	for _, c := range []struct{ book, want string }{{bookA, "jailer"}, {bookB, "jail"}} {
		detail, err := srv.buildChapter(c.book, 0)
		if err != nil {
			t.Fatal(err)
		}
		marks := detail.Paragraphs[0].PronunciationMarks
		if len(marks) != 1 || marks[0].Replacement != "jail" {
			t.Fatalf("book %s marks = %+v", c.book, marks)
		}
		lex, err := srv.buildLexicon(c.book)
		if err != nil || len(lex) != 1 || !lex[0].Global || !lex[0].Prefix || lex[0].Matches != 1 {
			t.Fatalf("book %s lexicon = %+v, %v", c.book, lex, err)
		}
	}
	book, _ := s.GetBook(bookA)
	lexicon, err := store.BookLexicon(s, book)
	if err != nil {
		t.Fatal(err)
	}
	p := store.Paragraph{Text: "The gaoler slept."}
	if got := p.ResolveGenerationText("", store.PronounceLexicon(lexicon)); got != "The jailer slept." {
		t.Fatalf("generation text = %q", got)
	}
}

func TestLexiconMatchesReportsDistinctWords(t *testing.T) {
	ps := []store.Paragraph{{Text: "The gaoler and the gaolers."}, {Text: "Gaoler again."}, {Text: "Nothing."}}
	m := lexiconMatches(ps, pronounce.LexiconEntry{Term: "gaol", Prefix: true})
	if m.Paragraphs != 2 || !reflect.DeepEqual(m.Words, []string{"gaoler", "gaolers"}) || m.MoreWords != 0 {
		t.Fatalf("= %+v", m)
	}
}
