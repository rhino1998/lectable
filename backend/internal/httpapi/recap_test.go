package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

func TestSpokenTitle(t *testing.T) {
	for in, want := range map[string]string{
		"City of Fallen Souls: A Dark Fantasy LitRPG Adventure (UnderVerse Book 3)": "City of Fallen Souls",
		"Arise: Reclaimer: A LitRPG Progression Fantasy.":                           "Arise: Reclaimer",
		"The Wandering Inn: Volume 10":                                              "The Wandering Inn: Volume 10",
		"Spire's Spite":                                                             "Spire's Spite",
	} {
		if got := spokenTitle(in); got != want {
			t.Errorf("spokenTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if got := spokenRecap("Arise: Reclaimer", "Steve woke."); got != "Previously, in Arise: Reclaimer. Steve woke." {
		t.Errorf("spokenRecap = %q", got)
	}
}

func TestRecapChunks(t *testing.T) {
	sentence := "Mocar sailed on through the storm with what was left of the crew. "
	long := strings.TrimSpace(strings.Repeat(sentence, 12)) // ~790 chars
	chunks := recapChunks("Short one.\n\n" + long)
	if len(chunks) != 3 || chunks[0] != "Short one." {
		t.Fatalf("chunks = %q", chunks)
	}
	for _, c := range chunks {
		if len(c) > recapChunkChars || !strings.HasSuffix(c, ".") {
			t.Errorf("chunk of %d chars, ending %q", len(c), c[len(c)-10:])
		}
	}
	if strings.Join(chunks[1:], " ") != long {
		t.Error("splitting a paragraph changed its text")
	}
}

// recapTestLLM answers the summary pass with the chapter's title and the
// recap passes with a line naming which one it was, recording the recap
// prompts.
type recapTestLLM struct {
	mu      sync.Mutex
	prompts []string
}

func (l *recapTestLLM) LLMGenerate(_ context.Context, sys, user string, _ float32, _ int) (string, error) {
	switch {
	case strings.Contains(sys, "notes on one chapter"):
		title := ""
		for _, line := range strings.Split(user, "\n") {
			if rest, ok := strings.CutPrefix(line, "Chapter: "); ok {
				title = rest
			}
		}
		return fmt.Sprintf(`{"summary": "What happened in %s.", "scenes": [], "cast": []}`, title), nil
	case strings.Contains(sys, "previously on"), strings.Contains(sys, "the book before it"):
		l.mu.Lock()
		l.prompts = append(l.prompts, user)
		l.mu.Unlock()
		kind := "The story so far"
		if strings.Contains(sys, "the book before it") {
			kind = "The earlier book"
		}
		return kind + strings.Repeat(" went on and on", 10) + ".", nil
	}
	return "", nil
}

func recapTestChapters(n int) []store.ChapterInput {
	var out []store.ChapterInput
	for i := range n {
		ch := store.ChapterInput{Title: fmt.Sprintf("Chapter %d", i+1)}
		for j := range 10 {
			ch.Blocks = append(ch.Blocks, store.BlockInput{Kind: store.BlockText, Text: fmt.Sprintf("Paragraph %d of chapter %d. %s", j, i+1, strings.Repeat("The road went on. ", 20))})
		}
		out = append(out, ch)
	}
	return out
}

// waitRecap polls GET .../recap until done(reply) or fails the test.
func waitRecap(t *testing.T, base, bookID string, done func(bookRecapResponse) bool) bookRecapResponse {
	t.Helper()
	var out bookRecapResponse
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		resp, err := http.Get(base + "/api/books/" + bookID + "/recap")
		if err != nil {
			t.Fatal(err)
		}
		out = bookRecapResponse{}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if done(out) {
			return out
		}
	}
	t.Fatalf("recap never got there: %+v (recap %+v)", out, out.Recap)
	return out
}

func postRecap(t *testing.T, base, bookID, query string) int {
	t.Helper()
	resp, err := http.Post(base+"/api/books/"+bookID+"/recap"+query, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST recap: status %d", resp.StatusCode)
	}
	var q queuedResponse
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		t.Fatal(err)
	}
	return q.Queued
}

// A recap is written from the summaries of the chapters behind the reader
// (summarizing them first) and the part of the current chapter they have
// heard, then spoken; asking again at the same place queues nothing; and
// the next book in the series opens with a recap of this one.
func TestBookRecap(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	llm := &recapTestLLM{}
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, llm)
	one, _, err := s.CreateBook("Book One: A Fantasy Adventure", "A", "en", "", "Saga", 1, recapTestChapters(4))
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := s.CreateBook("Book Two", "A", "en", "", "Saga", 2, recapTestChapters(2))
	if err != nil {
		t.Fatal(err)
	}

	// Nothing read: nothing to recap, and no earlier book.
	if n := postRecap(t, ts.URL, one, ""); n != 0 {
		t.Fatalf("queued %d for an unread first book", n)
	}

	if err := s.UpdatePosition(one, 2, 3, 0); err != nil {
		t.Fatal(err)
	}
	if n := postRecap(t, ts.URL, one, ""); n != 1 {
		t.Fatalf("queued = %d, want 1", n)
	}
	got := waitRecap(t, ts.URL, one, func(r bookRecapResponse) bool { return r.Recap != nil && r.Recap.AudioURL != "" && !r.Preparing })
	r := got.Recap
	if r.Kind != store.RecapPosition || r.ChapterIdx != 2 || r.ParagraphIdx != 3 || r.ThroughIdx != 9 || r.Title != "Book One" {
		t.Fatalf("recap = %+v", r)
	}
	if !strings.HasPrefix(r.Text, "The story so far") || r.AudioSeconds <= 0 || r.ReadAt == 0 {
		t.Fatalf("recap = %+v", r)
	}
	// The clip's layout: the opener leads the first stretch, and each
	// stretch's words sit inside it.
	if len(r.Segments) == 0 || !strings.HasPrefix(r.Segments[0].Text, "Previously, in Book One. The story so far") {
		t.Fatalf("segments = %+v", r.Segments)
	}
	for i, seg := range r.Segments {
		if seg.End <= seg.Start || i > 0 && seg.Start <= r.Segments[i-1].End {
			t.Fatalf("segment %d spans %v-%v after %+v", i, seg.Start, seg.End, r.Segments[:i])
		}
		for _, w := range seg.Words {
			if w.Start < seg.Start || w.Start > seg.End {
				t.Fatalf("segment %d (%v-%v) has a word at %v", i, seg.Start, seg.End, w.Start)
			}
		}
	}
	if last := r.Segments[len(r.Segments)-1]; last.End > r.AudioSeconds+0.01 {
		t.Fatalf("segments end at %v, clip at %v", last.End, r.AudioSeconds)
	}
	prompt := llm.prompts[0]
	for _, want := range []string{"- Chapter 1: What happened in Chapter 1.", "- Chapter 2: What happened in Chapter 2.", "in the middle of (Chapter 3)", "Paragraph 2 of chapter 3."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	for _, spoiler := range []string{"Paragraph 3 of chapter 3.", "Chapter 4"} {
		if strings.Contains(prompt, spoiler) {
			t.Errorf("prompt tells what the reader hasn't heard (%q)", spoiler)
		}
	}
	resp, err := http.Get(ts.URL + r.AudioURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "audio/ogg") {
		t.Fatalf("recap audio: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// Same place, or a little further on: the one it has will do.
	if err := s.UpdatePosition(one, 2, 5, 0); err != nil {
		t.Fatal(err)
	}
	if n := postRecap(t, ts.URL, one, ""); n != 0 {
		t.Fatalf("queued %d for a position its recap covers", n)
	}
	if n := postRecap(t, ts.URL, one, "?force=true"); n != 1 {
		t.Fatalf("force queued %d", n)
	}
	waitRecap(t, ts.URL, one, func(r bookRecapResponse) bool {
		return r.Recap != nil && r.Recap.ParagraphIdx == 5 && r.Recap.AudioURL != "" && !r.Preparing
	})

	// The next book, unread, opens with a recap of this one.
	if n := postRecap(t, ts.URL, two, ""); n != 1 {
		t.Fatalf("queued = %d for the second book", n)
	}
	got = waitRecap(t, ts.URL, two, func(r bookRecapResponse) bool { return r.Recap != nil && r.Recap.AudioURL != "" && !r.Preparing })
	if r := got.Recap; r.Kind != store.RecapSeries || r.Title != "Book One" || !strings.HasPrefix(r.Text, "The earlier book") || r.ChapterIdx != 0 || r.ParagraphIdx != 0 {
		t.Fatalf("series recap = %+v", r)
	}
	last := llm.prompts[len(llm.prompts)-1]
	if !strings.Contains(last, "- Chapter 4: What happened in Chapter 4.") || !strings.Contains(last, "about to start: Book Two") {
		t.Errorf("series prompt:\n%s", last)
	}
}

// The offer threshold is one library-wide setting.
func TestRecapSettings(t *testing.T) {
	srv, _, _, ts := newTestServer(t)
	if v, err := srv.buildRecapSettings(); err != nil || v.AfterMinutes != 24*60 {
		t.Fatalf("default = %+v, %v; want a day", v, err)
	}
	put := func(body string) int {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings/recap", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := put(`{"afterMinutes": 0}`); code != http.StatusNoContent {
		t.Fatalf("PUT immediately: %d", code)
	}
	if v, _ := srv.buildRecapSettings(); v.AfterMinutes != 0 {
		t.Fatalf("after PUT = %+v", v)
	}
	if code := put(`{"afterMinutes": -5}`); code != http.StatusBadRequest {
		t.Fatalf("PUT -5: %d", code)
	}
}
