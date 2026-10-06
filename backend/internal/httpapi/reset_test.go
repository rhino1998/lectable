package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/store"
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
		key := fmt.Sprintf("%032x", i+1)
		if err := s.SetParagraphReadyClip(p.ID, "voice-x", key, 1, "", nil); err != nil {
			t.Fatalf("SetParagraphReadyClip: %v", err)
		}
		files[i] = audiopath.SharedClipFile(srv.DataDir, key)
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
	states, err := s.ParagraphAudioStatuses([]string{paragraphs[0].ID, paragraphs[1].ID}, "voice-x")
	if err != nil {
		t.Fatalf("ParagraphAudioStatuses: %v", err)
	}
	if st, ok := states[paragraphs[0].ID]; ok {
		t.Fatalf("Alice's line still has audio (%s) after the reset", st.Status)
	}
	if states[paragraphs[1].ID].Status != store.AudioReady {
		t.Fatalf("the narration's audio is %q, want it kept", states[paragraphs[1].ID].Status)
	}
	// Neither clip is deleted with its row: an unused one is kept a while
	// in case the same reading is wanted again, then swept.
	for i, file := range files {
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("clip %d: %v", i, err)
		}
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
