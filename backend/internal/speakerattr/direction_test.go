package speakerattr

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// directionReplier answers each line's call from replies, keyed by a
// word of the line, in order per line.
type directionReplier struct {
	mu      sync.Mutex
	replies map[string][]string
	users   []string
}

func (r *directionReplier) LLMGenerate(_ context.Context, system, user string, _ float32, _ int) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users = append(r.users, user)
	for key, list := range r.replies {
		if strings.Contains(user, ">> "+key) && len(list) > 0 {
			r.replies[key] = list[1:]
			return list[0], nil
		}
	}
	return "", nil
}

func TestDirectLines(t *testing.T) {
	llm := &directionReplier{replies: map[string][]string{
		"“Fine":  {"```\n[weary] Fine. Go, then.\n```", "Line: [bone-tired, past arguing] Fine. [sighs] Go, then."},
		"“Never": {"[icy] Never again, do you hear me?", "Never again, do you hear me?", "Never again. Do you hear me now?"},
		"“Hello": {"Hello there, I said."},
	}}
	lines := []DirectionLine{
		{Idx: 1, Text: "“Fine. Go, then.”", Speaker: "Erin", Profile: "a young innkeeper", Label: "weary", Before: []string{"She sighed."}},
		{Idx: 4, Text: "“Never again, do you hear me?”", Speaker: "Klbkch"},
		{Idx: 7, Text: "“Hello there.”"},
	}
	out, err := NewClient(Config{}, llm).DirectLines(context.Background(), DirectionContext{BookTitle: "B", ChapterTitle: "C"}, lines)
	if err != nil {
		t.Fatal(err)
	}
	// A one-word style is sent back once for a phrase.
	if d := out[1]; d.Style != "bone-tired, past arguing" || len(d.Marks) != 1 || d.Marks[0].Tag != "sighs" {
		t.Errorf("line 1 = %+v", d)
	}
	// Past its retries, a one-word style is taken; here the second reply,
	// with no direction, came first.
	if d, ok := out[4]; !ok || !d.Empty() {
		t.Errorf("line 4 = %+v, %v", d, ok)
	}
	// A reply that changes the words leaves the line undirected.
	if d, ok := out[7]; !ok || !d.Empty() {
		t.Errorf("line 7 = %+v, %v", d, ok)
	}
	for _, u := range llm.users {
		if strings.Contains(u, "Erin") && !strings.Contains(u, "may be wrong") {
			t.Errorf("the label hint should ask for a phrase: %s", u)
		}
	}
	if !strings.Contains(directionSystemPrompt, "[clears throat]") || !strings.Contains(directionSystemPrompt, "<whisper>") {
		t.Error("the prompt should list direction's tags")
	}
}

func TestDeliveryCue(t *testing.T) {
	for text, want := range map[string]bool{
		"she laughed, wiping her eyes":        true,
		"he said, his voice breaking":         true,
		"Relc muttered under his breath":      true,
		"He walked to the door and opened it": false,
	} {
		if DeliveryCue(text) != want {
			t.Errorf("DeliveryCue(%q) = %v", text, !want)
		}
	}
}
