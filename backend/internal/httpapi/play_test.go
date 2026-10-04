package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// A play uploads with each speech already its speaker's, the label kept
// as unspoken text, and its speakers on the roster - and an attribution
// reset puts a changed line back on the speaker the book names.
func TestUploadPlay(t *testing.T) {
	_, s, _, ts := newTestServer(t)

	body := `<h1>Act I</h1><p>A drawing room.</p>` + strings.Repeat(
		`<p>JACK. [Gravely.] I have lost both my parents.</p><p>LADY BRACKNELL. That looks like carelessness. [She rises.]</p>`, 25)
	resp := postEpub(t, ts.URL+"/api/books", testEpub(t, body))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	var book struct{ ID string }
	if err := json.NewDecoder(resp.Body).Decode(&book); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	ch, _ := s.GetChapterByIdx(book.ID, 0)
	paragraphs, err := s.ListParagraphsRaw(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	type line struct {
		text, speaker, label string
		quote, inline        bool
	}
	want := []line{
		{"Act I", "", "", false, false},
		{"A drawing room.", "", "", false, false},
		{"I have lost both my parents.", "Jack", "JACK. [Gravely.]", true, false},
		{"That looks like carelessness.", "Lady Bracknell", "LADY BRACKNELL.", true, false},
		{"[She rises.]", "", "", false, true},
	}
	for i, w := range want {
		p := paragraphs[i]
		if got := (line{p.Text, p.Speaker, p.Label, p.IsQuote, p.Inline}); got != w {
			t.Errorf("paragraph %d: got %+v, want %+v", i, got, w)
		}
		if p.SourceSpeaker != w.speaker {
			t.Errorf("paragraph %d: source speaker %q, want %q", i, p.SourceSpeaker, w.speaker)
		}
	}

	b, _ := s.GetBook(book.ID)
	roster, err := s.ListCharacters(store.SeriesScope(b))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range roster {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "Jack,Lady Bracknell" && got != "Lady Bracknell,Jack" {
		t.Errorf("roster: %s", got)
	}

	// The chapter API carries the label for clients to show.
	var chapter struct {
		Paragraphs []struct{ Label, Speaker string }
	}
	getJSONInto(t, ts.URL+"/api/books/"+book.ID+"/chapters/0", &chapter)
	if p := chapter.Paragraphs[2]; p.Label != "JACK. [Gravely.]" || p.Speaker != "Jack" {
		t.Errorf("chapter API paragraph 2: %+v", p)
	}

	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{2: "Algernon"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetBookPass(book.ID, "attribution", 0); err != nil {
		t.Fatal(err)
	}
	paragraphs, _ = s.ListParagraphsRaw(ch.ID)
	if paragraphs[2].Speaker != "Jack" || paragraphs[3].Speaker != "Lady Bracknell" {
		t.Errorf("after an attribution reset: %q, %q", paragraphs[2].Speaker, paragraphs[3].Speaker)
	}

	// ?play=off imports the same file as prose.
	resp = postEpub(t, ts.URL+"/api/books?play=off", testEpub(t, strings.Replace(body, "Act I", "Act One", 1)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload as prose: %d", resp.StatusCode)
	}
	var prose struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&prose)
	resp.Body.Close()
	pch, _ := s.GetChapterByIdx(prose.ID, 0)
	pp, _ := s.ListParagraphsRaw(pch.ID)
	if pp[2].IsQuote || pp[2].Speaker != "" || !strings.HasPrefix(pp[2].Text, "JACK.") {
		t.Errorf("play=off still read a play: %+v", pp[2])
	}
}

func getJSONInto(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}
