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
	out, _, _, _, remaining, err := c.AttributeChapter(context.Background(), "Book", "Ch", Roster{Names: []string{"Bert"}}, paras, nil)
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

// descriptorLLM answers every asked line with a description, the way a
// small model does when narration only says "the older man said".
type descriptorLLM struct{}

func (descriptorLLM) LLMGenerate(_ context.Context, _, user string, _ float32, _ int) (string, error) {
	var sb strings.Builder
	for _, line := range strings.Split(user, "\n") {
		if i := strings.Index(line, " (who?):"); i > 0 {
			fmt.Fprintf(&sb, "%s: the older man\n", line[:i])
		}
	}
	return sb.String(), nil
}

// A description the model answers with becomes a role label, even though
// AttributeChapter has already seen it as a "name" from an earlier line.
func TestAttributeChapterTurnsDescriptionsIntoRoles(t *testing.T) {
	var paras []ParagraphInput
	for i := 0; i < 90; i++ {
		paras = append(paras, ParagraphInput{Idx: i, Text: fmt.Sprintf("“line %d”", i), IsQuote: true})
	}
	out, _, roles, _, _, err := NewClient(Config{}, descriptorLLM{}).AttributeChapter(context.Background(), "Book", "Ch", Roster{Names: []string{"Bert"}}, paras, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paras {
		if out[p.Idx] != "Man" {
			t.Fatalf("paragraph %d: speaker %q, want Man", p.Idx, out[p.Idx])
		}
	}
	if !roles["Man"] {
		t.Fatalf("roles = %v, want Man", roles)
	}
}

// The chapter-level roster fields reach every batch prompt: who "I" is, and
// the scene markers with who is present. Both were once dropped when
// AttributeChapter built each batch's roster.
func TestAttributeChapterPromptCarriesNarratorAndScenes(t *testing.T) {
	paras := []ParagraphInput{
		{Idx: 0, Text: "“Go,”", IsQuote: true},
		{Idx: 1, Text: "I said.", Inline: true},
		{Idx: 2, Text: "“Now,”", IsQuote: true},
		{Idx: 3, Text: "he said.", Inline: true},
	}
	roster := Roster{
		Names: []string{"Steve", "Dave", "Kev"}, Prominent: []string{"Steve", "Dave", "Kev"}, FirstPerson: "Steve",
		Scenes: []Scene{{StartIdx: 0, Location: "the flat", Present: []string{"Steve", "Kev"}}, {StartIdx: 2, Present: []string{"Steve", "Dave"}}},
	}
	llm := &recordingLLM{}
	if _, _, _, _, _, err := NewClient(Config{}, llm).AttributeChapter(context.Background(), "Book", "Ch", roster, paras, nil); err != nil {
		t.Fatal(err)
	}
	prompt := llm.prompts[0]
	for _, want := range []string{"narrated in the first person by Steve", "[Scene: the flat; present: Steve, Kev]\n0 (who?)", "[Scene: present: Steve, Dave]\n2 (who?)"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

func TestAttributeChapterUsesReaderCorrections(t *testing.T) {
	paras := []ParagraphInput{
		{Idx: 0, Text: "“Get off it,”", IsQuote: true, Known: "Dave"},
		{Idx: 1, Text: "he said.", Inline: true},
		{Idx: 2, Text: "“Make me.”", IsQuote: true},
	}
	roster := Roster{
		Names: []string{"Bert", "Dave", "Kev", "Zed"},
		Corrections: []Correction{
			{ChapterIdx: 1, Text: "“Oi, you!”", Wrong: "Kev", Right: "Dave"},
			{ChapterIdx: 2, Text: "“Elsewhere.”", Wrong: "Zed", Right: "Mira"},
			{ChapterIdx: 3, Text: "“Not speech.”", Wrong: "Narrator", Right: "Dave"},
		},
	}
	llm := &recordingLLM{}
	got, _, _, _, _, err := NewClient(Config{}, llm).AttributeChapter(context.Background(), "Book", "Ch", roster, paras, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "Dave" || got[2] != "Bert" {
		t.Fatalf("speakers = %v, want the reader's Dave on 0 and the model's Bert on 2", got)
	}
	prompt := llm.prompts[0]
	for _, want := range []string{"0 (Dave): “Get off it,”", "2 (who?)", "- “Oi, you!” - Dave, not Kev\n"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{"0 (who?)", "Elsewhere", "Not speech"} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("prompt has %q:\n%s", unwanted, prompt)
		}
	}
}

func TestRelevantCorrectionsNearestFirstAndCapped(t *testing.T) {
	var corrections []Correction
	for ch := 0; ch < 5; ch++ {
		corrections = append(corrections, Correction{ChapterIdx: ch, Text: fmt.Sprint(ch), Wrong: "Kev", Right: "Dave"})
	}
	corrections = append(corrections, Correction{ChapterIdx: 5, Text: "5", Wrong: "Unknown", Right: "steve"})
	got := relevantCorrections(corrections, []string{"Dave"}, "Steve")
	var texts []string
	for _, c := range got {
		texts = append(texts, c.Text)
	}
	// The narrator counts as relevant (case-insensitively); one wrong->right
	// pair fills at most maxCorrectionsPerPair slots, nearest chapter first.
	if want := []string{"5", "4", "3"}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("relevantCorrections = %v, want %v", texts, want)
	}
}
