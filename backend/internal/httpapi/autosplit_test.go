package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// autoSplitLLM answers every "(who?)" line from answers (idx -> speaker),
// recording which lines each call asked about.
type autoSplitLLM struct {
	mu      sync.Mutex
	answers map[int]string
	asked   [][]string
}

var whoLine = regexp.MustCompile(`(?m)^(\d+) \(who\?\): `)

func (f *autoSplitLLM) LLMGenerate(_ context.Context, _, user string, _ float32, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var asked []string
	var out strings.Builder
	for _, m := range whoLine.FindAllStringSubmatch(user, -1) {
		asked = append(asked, m[1])
		idx, err := strconv.Atoi(m[1])
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "%d: %s\n", idx, f.answers[idx])
	}
	f.asked = append(f.asked, asked)
	return out.String(), nil
}

// TestAutoSplitAsksOnlyUntaggedTargets covers Auto Split's narrowed flow
// (reattributeChapterSpeaker): a target with an explicit tag is settled
// without the model, a narration target becomes Narrator, only the
// remaining target is asked about, and an answer still naming the split
// speaker falls back to Unknown.
func TestAutoSplitAsksOnlyUntaggedTargets(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	llm := &autoSplitLLM{answers: map[int]string{4: "Alice", 6: "Bogus"}}
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, llm)

	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{
		Title: "One",
		Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Morning,”", IsQuote: true},      // 0: target, tagged Carol
			{Kind: store.BlockText, Text: "Carol said.", Inline: true},      // 1
			{Kind: store.BlockText, Text: "The sun rose over the harbour."}, // 2: target, narration
			{Kind: store.BlockText, Text: "“Nice day.”", IsQuote: true},     // 3: not a target
			{Kind: store.BlockText, Text: "“Is it?”", IsQuote: true},        // 4: target, untagged
			{Kind: store.BlockText, Text: "They walked on in silence."},     // 5
			{Kind: store.BlockText, Text: "“Hmm.”", IsQuote: true},          // 6: target, model says Bogus again
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	for _, n := range []string{"Alice", "Carol", "Bogus"} {
		if _, _, err := s.UpsertCharacter(scope, n, false); err != nil {
			t.Fatal(err)
		}
	}
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "Bogus", 2: "Bogus", 3: "Alice", 4: "Bogus", 6: "Bogus"}); err != nil {
		t.Fatal(err)
	}

	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/speakers/reattribute", map[string]any{"name": "Bogus"}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("reattribute: %d", resp.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		inFlight, queued := srv.Jobs.Snapshot()
		if len(inFlight) == 0 && len(queued) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reattribution didn't drain")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ps, _ := s.ListParagraphsRaw(ch.ID)
	got := map[int]string{}
	for _, p := range ps {
		got[p.Idx] = p.Speaker
	}
	want := map[int]string{0: "Carol", 2: "Narrator", 3: "Alice", 4: "Alice", 6: "Unknown"}
	for idx, w := range want {
		if got[idx] != w {
			t.Errorf("paragraph %d: %q, want %q", idx, got[idx], w)
		}
	}
	for _, asked := range llm.asked {
		for _, idx := range asked {
			if idx != "4" && idx != "6" {
				t.Errorf("model was asked about line %s; only untagged dialogue targets (4, 6) should be", idx)
			}
		}
	}
	if len(llm.asked) == 0 {
		t.Fatal("model was never asked")
	}
}
