package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestResetAttributionClearsSpeakersAndStaleAudio checks POST
// .../reset/attribution end to end: a character's line loses its speaker
// and its generated audio (row and file), while a narration line's audio -
// never voiced by a character - survives.
func TestResetAttributionClearsSpeakersAndStaleAudio(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, `"Hi," she said.`, "Narration.")
	ch, err := s.GetChapterByIdx(bookID, 0)
	if err != nil || ch == nil {
		t.Fatalf("GetChapterByIdx: %v", err)
	}
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "Alice"}); err != nil {
		t.Fatalf("SetParagraphSpeakers: %v", err)
	}
	if err := s.SetChapterAttributed(ch.ID); err != nil {
		t.Fatalf("SetChapterAttributed: %v", err)
	}
	paragraphs, err := s.ListParagraphsRaw(ch.ID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	files := make([]string, len(paragraphs))
	for i, p := range paragraphs {
		if err := s.SetParagraphReady(p.ID, "voice-x", 1); err != nil {
			t.Fatalf("SetParagraphReady: %v", err)
		}
		files[i] = srv.paragraphAudioPath(bookID, ch.ID, "voice-x", p.Idx)
		if err := os.MkdirAll(filepath.Dir(files[i]), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(files[i], []byte("wav"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := http.Post(ts.URL+"/api/books/"+bookID+"/reset/attribution", "", nil)
	if err != nil {
		t.Fatalf("POST reset: %v", err)
	}
	defer resp.Body.Close()
	var body struct{ Invalidated int }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusOK || body.Invalidated != 1 {
		t.Fatalf("status %d invalidated %d, want 200 and 1", resp.StatusCode, body.Invalidated)
	}

	p0, err := s.GetParagraph(paragraphs[0].ID)
	if err != nil {
		t.Fatalf("GetParagraph: %v", err)
	}
	if p0.Speaker != "" {
		t.Fatalf("speaker %q after reset, want \"\"", p0.Speaker)
	}
	if _, err := os.Stat(files[0]); !os.IsNotExist(err) {
		t.Fatalf("expected Alice's audio file removed, stat err %v", err)
	}
	if _, err := os.Stat(files[1]); err != nil {
		t.Fatalf("expected narration audio file kept: %v", err)
	}
	ch, err = s.GetChapterByID(ch.ID)
	if err != nil {
		t.Fatalf("GetChapterByID: %v", err)
	}
	if ch.Passes.Attribution {
		t.Fatalf("expected passes.attribution cleared")
	}

	resp2, err := http.Post(ts.URL+"/api/books/"+bookID+"/reset/bogus", "", nil)
	if err != nil {
		t.Fatalf("POST reset bogus: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown pass status %d, want 404", resp2.StatusCode)
	}
}
