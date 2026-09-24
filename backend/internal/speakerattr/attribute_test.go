package speakerattr

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseAttributionLines(t *testing.T) {
	answer := map[int]bool{12: true, 15: true, 16: true}
	got, err := parseAttributionLines("Here you go:\n12: Gandalf\n15 (who?): Guard [role]\n16 - \"Unknown\".\n99: Frodo\n", answer)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]Attributed{12: {Speaker: "Gandalf"}, 15: {Speaker: "Guard [role]", Role: true}, 16: {Speaker: "Unknown"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// The old JSON shape still parses, filtered to what was asked.
	got, err = parseAttributionLines(`[{"idx": 12, "speaker": "Gandalf"}, {"idx": 13, "speaker": "Narrator"}]`, answer)
	if err != nil || len(got) != 1 || got[12].Speaker != "Gandalf" {
		t.Fatalf("JSON fallback: %+v, %v", got, err)
	}

	if _, err := parseAttributionLines("I can't tell.", answer); err == nil {
		t.Fatal("a reply with no usable line should be an error, so it's retried")
	}
}

func TestRelevantNames(t *testing.T) {
	roster := Roster{
		Names:     []string{"Fritz", "Bert", "Jagged Nic", "Lady Hightide", "Guard", "Veronica"},
		Aliases:   map[string][]string{"Veronica": {"Vee"}},
		Prominent: []string{"Fritz"},
	}
	lines := []ParagraphInput{{Text: "“Morning,” Nic said to the guard."}, {Text: "Lady Smith waved. “Vee!”"}}
	got := relevantNames(roster, lines)
	// Fritz: prominent. Jagged Nic: "Nic". Guard: "guard". Veronica: her
	// alias. Not Lady Hightide - "Lady" is a title word, not her name.
	if want := []string{"Fritz", "Jagged Nic", "Guard", "Veronica"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("relevantNames = %v, want %v", got, want)
	}
}

// recordingLLM answers every "(who?)" line with a fixed speaker after a
// short delay, recording each prompt and the peak number of calls in
// flight at once.
type recordingLLM struct {
	mu       sync.Mutex
	prompts  []string
	inFlight int
	peak     int
}

func (r *recordingLLM) LLMGenerate(_ context.Context, _, user string, _ float32, _ int) (string, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, user)
	r.inFlight++
	r.peak = max(r.peak, r.inFlight)
	r.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()

	var out strings.Builder
	for _, m := range promptLine.FindAllStringSubmatch(user, -1) {
		if m[2] != "" {
			fmt.Fprintf(&out, "%s: Bert\n", m[1])
		}
	}
	return out.String(), nil
}

func TestAttributeChapterAsksOnlyDialogueInParallel(t *testing.T) {
	// 120 paragraphs = 3 batches of 40. Batch 2 (40-79) is all narration.
	var paras []ParagraphInput
	for i := 0; i < 120; i++ {
		isQuote := i%2 == 0 && (i < 40 || i >= 80)
		text := fmt.Sprintf("narration %d", i)
		if isQuote {
			text = fmt.Sprintf("“line %d”", i)
		}
		paras = append(paras, ParagraphInput{Idx: i, Text: text, IsQuote: isQuote})
	}
	llm := &recordingLLM{}
	c := NewClient(Config{}, llm)
	out, _, remaining, err := c.AttributeChapter(context.Background(), "Book", "Ch", Roster{Names: []string{"Bert"}}, paras, nil)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("err=%v remaining=%d", err, len(remaining))
	}
	if len(llm.prompts) != 2 {
		t.Fatalf("want 2 model calls (the narration-only batch needs none), got %d", len(llm.prompts))
	}
	if llm.peak != 2 {
		t.Errorf("want 2 batches in flight at once, peak was %d", llm.peak)
	}
	for _, p := range paras {
		if got, want := out[p.Idx], map[bool]string{true: "Bert", false: ""}[p.IsQuote]; got != want {
			t.Fatalf("paragraph %d: speaker %q, want %q", p.Idx, got, want)
		}
	}
	// Batch 3 shows the 3 lines before it as unanswered context.
	for _, prompt := range llm.prompts {
		if strings.Contains(prompt, "\n80 (who?): ") {
			if !strings.Contains(prompt, "\n77: narration 77") {
				t.Errorf("batch 3 is missing its leading context lines:\n%s", prompt)
			}
			if strings.Contains(prompt, "(who?): narration") {
				t.Errorf("narration was asked about:\n%s", prompt)
			}
		}
	}
}
