package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

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
	voice, err := srv.Narration.ForParagraph(book, "")
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
