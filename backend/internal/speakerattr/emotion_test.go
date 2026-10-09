package speakerattr

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

func TestParseEmotionLines(t *testing.T) {
	dialogue := map[int]bool{3: true, 4: true, 7: true, 9: true}
	content := strings.Join([]string{
		"3: angry",
		"4: Whisper.",         // case and trailing punctuation tolerated
		"5: sad",              // narration line - dropped
		"7: elated",           // not a known emotion - dropped
		"9: shout (he yells)", // trailing commentary - first word kept
		"12: warm",            // not in this batch - dropped
		"garbage line",
	}, "\n")
	got := parseEmotionLines(content, dialogue)
	want := map[int]string{3: "angry", 4: "whisper", 9: "shout"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseEmotionLines = %v, want %v", got, want)
	}
	if got := parseEmotionLines("", dialogue); len(got) != 0 {
		t.Fatalf("empty reply should mean all neutral, got %v", got)
	}
}

// TestEmotionSystemPromptListsEveryEmotion guards the prompt against
// drifting from internal/emotions - a label the model is never offered can
// never be produced, and one it's offered but the app doesn't know is
// silently dropped.
func TestEmotionSystemPromptListsEveryEmotion(t *testing.T) {
	for _, e := range emotions.All {
		if !strings.Contains(emotionSystemPrompt, "- "+e.ID+": ") {
			t.Errorf("emotionSystemPrompt is missing %q", e.ID)
		}
	}
}

// emotionDecider is an llmBackend with a decide call that answers each
// step with the option at choices[step] (0 is neutral).
type emotionDecider struct {
	choices []int
	err     error
	system  string
	steps   []ttsproto.LLMDecideStep
	listed  string // what a generated reply would say
}

func (l *emotionDecider) LLMGenerate(context.Context, string, string, float32, int) (string, error) {
	return l.listed, nil
}

func (l *emotionDecider) LLMDecide(_ context.Context, system, _ string, steps []ttsproto.LLMDecideStep) ([]ttsproto.LLMDecision, error) {
	l.system, l.steps = system, steps
	if l.err != nil {
		return nil, l.err
	}
	out := make([]ttsproto.LLMDecision, len(steps))
	for i := range steps {
		out[i].Choice = l.choices[i]
	}
	return out, nil
}

// TestEmotionChapterDecides checks that a backend with a decide call is
// asked for a verdict on every dialogue line through the dense prompt, and
// that only lines where an emotion won are labeled.
func TestEmotionChapterDecides(t *testing.T) {
	paragraphs := []ParagraphInput{
		{Idx: 3, Text: "“Fine,”", IsQuote: true},
		{Idx: 4, Text: "she hissed.", Inline: true},
		{Idx: 5, Text: "“Sit down.”", IsQuote: true},
	}
	whisper := 0
	for i, e := range emotions.All {
		if e.ID == "whisper" {
			whisper = i + 1
		}
	}
	llm := &emotionDecider{choices: []int{whisper, 0}, listed: "5: angry"}
	out, remaining, err := NewClient(Config{}, llm).EmotionChapter(context.Background(), "Book", "One", paragraphs, nil)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("EmotionChapter: remaining %d, err %v", len(remaining), err)
	}
	if want := map[int]string{3: "whisper"}; !reflect.DeepEqual(out, want) {
		t.Errorf("labels = %v, want %v", out, want)
	}
	if llm.system != emotionDenseSystemPrompt {
		t.Error("decided with a prompt other than the dense one")
	}
	if len(llm.steps) != 2 || llm.steps[0].Prefix != "3:" || llm.steps[1].Prefix != "\n5:" {
		t.Fatalf("steps = %+v, want one each for lines 3 and 5", llm.steps)
	}
	if opts := llm.steps[0].Options; len(opts) != len(emotions.All)+1 || opts[0] != " neutral" || opts[whisper] != " whisper" {
		t.Errorf("options = %q", opts)
	}

	// A decide call that fails falls back to the generated list.
	llm.err = fmt.Errorf("options must each start with a distinct token")
	out, _, err = NewClient(Config{}, llm).EmotionChapter(context.Background(), "Book", "One", paragraphs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int]string{5: "angry"}; !reflect.DeepEqual(out, want) {
		t.Errorf("fallback labels = %v, want %v", out, want)
	}
}
