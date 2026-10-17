package speakerattr

import (
	"context"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// recordingDecider answers every decide step with its first option and
// records the steps and prompt it was sent.
type recordingDecider struct {
	steps []ttsproto.LLMDecideStep
	user  string
}

func (d *recordingDecider) LLMGenerate(context.Context, string, string, float32, int) (string, error) {
	return "", nil
}

func (d *recordingDecider) LLMDecide(_ context.Context, _, user string, steps []ttsproto.LLMDecideStep) ([]ttsproto.LLMDecision, error) {
	d.steps, d.user = steps, user
	out := make([]ttsproto.LLMDecision, len(steps))
	for i, s := range steps {
		out[i] = ttsproto.LLMDecision{Choice: 0, Probs: make([]float64, len(s.Options)), Mass: 1}
		out[i].Probs[0] = 1
	}
	return out, nil
}

func TestSeedInReplyWritesForcedAnswersIntoNextPrefix(t *testing.T) {
	d := &recordingDecider{}
	c := &Client{llm: d}
	roster := Roster{Names: []string{"Fritz", "Bert"}, Pronouns: map[string]string{"Fritz": "he/him", "Bert": "he/him"}, Prominent: []string{"Fritz", "Bert"}}
	lines := []ParagraphInput{
		{Idx: 47, IsQuote: true, Text: "“Bert?”", Forced: "Fritz"},
		{Idx: 49, IsQuote: true, Text: "“Yes?”", Forced: "Bert"},
		{Idx: 51, IsQuote: true, Text: "“When we asked you,”"},
		{Idx: 53, IsQuote: true, Text: "“Fine.”", Forced: "Fritz"},
	}
	answer := map[int]bool{47: true, 49: true, 51: true, 53: true}
	out, err := c.attributeBatchDecide(context.Background(), "B", "C", roster, lines, answer)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.steps) != 1 {
		t.Fatalf("got %d decide steps, want 1 (only the unforced line): %+v", len(d.steps), d.steps)
	}
	if want := "47: Fritz | he\n49: Bert | he\n51:"; d.steps[0].Prefix != want {
		t.Errorf("prefix = %q, want %q", d.steps[0].Prefix, want)
	}
	// The prompt is plain decide's: forced lines are still asked.
	for _, want := range []string{"47 (who?):", "49 (who?):", "51 (who?):", "53 (who?):"} {
		if !strings.Contains(d.user, want) {
			t.Errorf("prompt lacks %q:\n%s", want, d.user)
		}
	}
	for idx, want := range map[int]string{47: "Fritz", 49: "Bert", 53: "Fritz"} {
		if out[idx].Speaker != want {
			t.Errorf("line %d = %q, want forced %q", idx, out[idx].Speaker, want)
		}
	}
	if _, ok := out[51]; !ok {
		t.Error("the decided line has no answer")
	}
}

func TestSeedInReplyAllForcedAsksNothing(t *testing.T) {
	d := &recordingDecider{}
	c := &Client{llm: d}
	lines := []ParagraphInput{{Idx: 1, IsQuote: true, Forced: "Sid"}}
	out, err := c.attributeBatchDecide(context.Background(), "B", "C", Roster{Names: []string{"Sid"}}, lines, map[int]bool{1: true})
	if err != nil || out[1].Speaker != "Sid" || d.steps != nil {
		t.Fatalf("out=%v err=%v steps=%v, want Sid with no decide call", out, err, d.steps)
	}
}

func TestConfiguredSeedUnderDecideIsSeedInReply(t *testing.T) {
	d := &recordingDecider{}
	c := &Client{llm: d, cfg: Config{Seed: true, Decide: true}}
	roster := Roster{Names: []string{"Sid", "Fritz"}, Prominent: []string{"Sid", "Fritz"}}
	paragraphs := []ParagraphInput{
		{Idx: 0, IsQuote: true, Text: "“No mana, let me rest,”"},
		{Idx: 1, Inline: true, Text: "Sid replied."},
		{Idx: 2, Text: "Fritz looked at the cave."},
		{Idx: 3, IsQuote: true, Text: "“Then we wait.”"},
	}
	if _, _, _, _, _, err := c.AttributeChapterWith(context.Background(), "B", "C", roster, paragraphs, nil, AttributeOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.user, "0 (who?):") {
		t.Errorf("the seeded line isn't asked in the prompt:\n%s", d.user)
	}
	if len(d.steps) != 1 || !strings.HasPrefix(d.steps[0].Prefix, "0: Sid") {
		t.Errorf("steps = %+v, want one question whose prefix writes the seeded 0: Sid", d.steps)
	}
}
