package speakerattr

import (
	"context"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

func TestSeedFromTags(t *testing.T) {
	paras := []ParagraphInput{
		{Idx: 0, Text: "“Halt!”", IsQuote: true},
		{Idx: 1, Text: "Bert said.", Inline: true},
		{Idx: 2, Text: "“Why?”", IsQuote: true},
		{Idx: 3, Text: "“Because,”", IsQuote: true},
		{Idx: 4, Text: "Fritz said.", Inline: true},
		{Idx: 5, Text: "“Fine.”", IsQuote: true, Known: "Sid"},
	}
	got := seedFromTags(paras, Roster{Names: []string{"Bert", "Fritz", "Sid"}})
	want := map[int]string{0: "Bert", 2: "", 3: "Fritz", 5: "Sid"}
	for _, p := range got {
		if w, ok := want[p.Idx]; ok && p.Known != w {
			t.Errorf("line %d: seeded %q, want %q", p.Idx, p.Known, w)
		}
	}
	if paras[0].Known != "" {
		t.Error("seedFromTags changed its input")
	}
}

func TestLineCandidates(t *testing.T) {
	roster := Roster{
		Names:     []string{"Fritz", "Bert", "Sid", "Sidney", "Lord Gresper", "Guard"},
		Roles:     map[string]bool{"Guard": true},
		Pronouns:  map[string]string{"Sid": "she/her", "Fritz": "he/him"},
		Prominent: []string{"Fritz", "Bert", "Sid"},
		Scenes:    []Scene{{StartIdx: 0, Present: []string{"Fritz", "Sid"}}, {StartIdx: 50, Present: []string{"Bert"}}},
	}
	lines := []ParagraphInput{
		{Idx: 0, Text: "Sidney waved at the guard."},
		{Idx: 1, Text: "“No I'm not,”", IsQuote: true},
		{Idx: 2, Text: "She argued.", Inline: true},
	}
	var got []string
	for _, c := range lineCandidates(roster, lines, 1) {
		got = append(got, c.option())
	}
	want := []string{" Fritz | he\n", " Sid | she\n", " Sidney\n", " Guard [role]\n", " Unknown\n"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("options = %q, want %q", got, want)
	}
}

// decideLLM answers every decide step with its option index pick, at mass.
type decideLLM struct {
	pick  int
	mass  float64
	reply string // LLMGenerate's reply, for lines asked again
	steps []ttsproto.LLMDecideStep
}

func (l *decideLLM) LLMGenerate(context.Context, string, string, float32, int) (string, error) {
	return l.reply, nil
}

func (l *decideLLM) LLMDecide(_ context.Context, _, _ string, steps []ttsproto.LLMDecideStep) ([]ttsproto.LLMDecision, error) {
	l.steps = steps
	out := make([]ttsproto.LLMDecision, len(steps))
	for i := range steps {
		out[i] = ttsproto.LLMDecision{Choice: l.pick, Mass: l.mass}
	}
	return out, nil
}

func TestAttributeBatchDecide(t *testing.T) {
	roster := Roster{Names: []string{"Fritz", "Sid"}, Prominent: []string{"Fritz", "Sid"}}
	lines := []ParagraphInput{{Idx: 7, Text: "“Go,”", IsQuote: true}, {Idx: 8, Text: "“No,”", IsQuote: true}}
	answer := map[int]bool{7: true, 8: true}

	llm := &decideLLM{pick: 1, mass: 0.9}
	got, err := NewClient(Config{}, llm).attributeBatchDecide(context.Background(), "B", "C", roster, lines, answer)
	if err != nil {
		t.Fatal(err)
	}
	if got[7].Speaker != "Sid" || got[8].Speaker != "Sid" || llm.steps[1].Prefix != "8:" {
		t.Fatalf("got %+v, steps %+v", got, llm.steps)
	}

	// Too little mass on the list: the line is generated instead.
	llm = &decideLLM{pick: 0, mass: 0.1, reply: "7: Bert\n8: Bert"}
	got, err = NewClient(Config{}, llm).attributeBatchDecide(context.Background(), "B", "C", roster, lines, answer)
	if err != nil {
		t.Fatal(err)
	}
	if got[7].Speaker != "Bert" || got[8].Speaker != "Bert" {
		t.Fatalf("low-mass lines: got %+v, want the generated answers", got)
	}
}
