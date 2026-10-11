package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// jumpLLM answers AgeCastingBy: a teenager changes, anyone else doesn't.
type jumpLLM struct{}

func (jumpLLM) LLMGenerate(_ context.Context, _, user string, _ float32, _ int) (string, error) {
	if strings.Contains(user, "teenage") {
		return `{"was": 15, "age": 20, "changed": true, "instruct": "Speak as a young man with a settled, fuller voice and an easy pace."}`, nil
	}
	return `{"was": 45, "age": 50, "changed": false, "instruct": ""}`, nil
}

// A time jump ages the characters who speak on both sides of it and whose
// age group changes; removing it takes their stages with it.
func TestTimeJump(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, jumpLLM{})
	chapters := make([]store.ChapterInput, 2)
	for i := range chapters {
		chapters[i] = store.ChapterInput{Title: "Ch", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true},
			{Kind: store.BlockText, Text: "“Hello,”", IsQuote: true},
			{Kind: store.BlockText, Text: "“Well,”", IsQuote: true},
		}}
	}
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, chapters)
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	cast := map[string]string{
		"Will":  "Speak as a teenage boy with a bright, light voice and a quick pace.",
		"Loth":  "Speak as a middle-aged woman with a dry, low voice and a measured pace.",
		"Early": "Speak as a teenage girl with a bright, light voice and a quick pace.",
	}
	ids := map[string]string{}
	for name, casting := range cast {
		c, _, err := s.UpsertCharacter(scope, name, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetCharacterSummary(c.ID, casting, "A line."); err != nil {
			t.Fatal(err)
		}
		ids[name] = c.ID
	}
	// Early speaks only before the jump.
	for idx, speakers := range []map[int]string{{0: "Will", 1: "Loth", 2: "Early"}, {0: "Will", 1: "Loth"}} {
		ch, _ := s.GetChapterByIdx(bookID, idx)
		if err := s.SetParagraphSpeakers(ch.ID, speakers); err != nil {
			t.Fatal(err)
		}
	}
	jumpsURL := ts.URL + "/api/books/" + bookID + "/time-jumps"

	var queued queuedResponse
	resp := doJSON(t, http.MethodPost, jumpsURL, timeJumpRequest{ChapterIdx: 1, Years: 5}, &queued)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || queued.Queued != 2 {
		t.Fatalf("create: status %d, queued %d, want 202 and the 2 who speak on both sides", resp.StatusCode, queued.Queued)
	}
	resp = doJSON(t, http.MethodPost, jumpsURL, timeJumpRequest{ChapterIdx: 1, Years: 3}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("a second jump at the same chapter: status %d, want 409", resp.StatusCode)
	}

	var jumps []timeJumpDTO
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp = doJSON(t, http.MethodGet, jumpsURL, nil, &jumps)
		resp.Body.Close()
		if len(jumps) == 1 && jumps[0].Voices == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(jumps) != 1 || jumps[0].Years != 5 || jumps[0].ChapterIdx != 1 || jumps[0].Voices != 1 {
		t.Fatalf("jumps = %+v, want one of 5 years at chapter 1 with 1 voice aged", jumps)
	}
	stages, err := s.ListCharacterStages(scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || stages[0].CharacterID != ids["Will"] || stages[0].Age != 20 || stages[0].JumpID != jumps[0].ID || stages[0].ChapterIdx != 1 {
		t.Fatalf("stages = %+v, want only Will's, at 20, from chapter 1", stages)
	}

	resp = doJSON(t, http.MethodDelete, jumpsURL+"/"+jumps[0].ID, nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d", resp.StatusCode)
	}
	if stages, _ := s.ListCharacterStages(scope); len(stages) != 0 {
		t.Errorf("after removing the jump its stages remain: %+v", stages)
	}
}
