package speakerattr

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSplitRoleIndex(t *testing.T) {
	for _, c := range []struct {
		in, label string
		index     int
	}{
		{"Guard 2", "Guard", 2},
		{"Guard 2 [role]", "Guard [role]", 2},
		{"Guard [role] 2", "Guard [role]", 2},
		{"Guard #1 [role]", "Guard [role]", 1},
		{"Guard No. 3", "Guard", 3},
		{"Guard Two [role]", "Guard [role]", 2},
		{"Guard One [role]", "Guard One [role]", 0}, // see splitRoleOne
		{"the Old One", "the Old One", 0},
		{"Second Guard [role]", "Guard [role]", 2},
		{"the second guard", "the guard", 2},
		{"Scale Guard 2", "Scale Guard", 2},
		{"Guard", "Guard", 0},
		{"Guard [role]", "Guard [role]", 0},
		{"Guard 12", "Guard 12", 0}, // out of range: left alone
		{"Guard 0", "Guard 0", 0},
		{"the second", "the second", 0},
		{"Second", "Second", 0},
		{"2", "2", 0},
		{"Bert", "Bert", 0},
	} {
		label, index := SplitRoleIndex(c.in)
		if label != c.label || index != c.index {
			t.Errorf("SplitRoleIndex(%q) = %q, %d; want %q, %d", c.in, label, index, c.label, c.index)
		}
	}
}

// numberedGuardsLLM answers a two-guard exchange with numbered role labels,
// and a named character with a number that is part of the name.
type numberedGuardsLLM struct{}

func (numberedGuardsLLM) LLMGenerate(_ context.Context, _, _ string, _ float32, _ int) (string, error) {
	return "0: Guard 1 [role]\n1: Guard 2 [role]\n2: Bert | he\n3: Guard 1 [role]\n4: the second guard\n5: Unit 7\n6: Guard [role]", nil
}

// A numbered role answer is stored as the bare role label with the number
// beside it, and a number on anything that isn't a role stays in the name.
func TestAttributeChapterRoleNumbers(t *testing.T) {
	paras := []ParagraphInput{
		{Idx: 0, Text: "“Halt.”", IsQuote: true},
		{Idx: 1, Text: "“Who goes there?”", IsQuote: true},
		{Idx: 2, Text: "“Only me.”", IsQuote: true},
		{Idx: 3, Text: "“Name?”", IsQuote: true},
		{Idx: 4, Text: "“Let him through.”", IsQuote: true},
		{Idx: 5, Text: "“Acknowledged.”", IsQuote: true},
		{Idx: 6, Text: "“Move along.”", IsQuote: true},
	}
	out, indexes, roles, _, _, err := NewClient(Config{}, numberedGuardsLLM{}).AttributeChapter(context.Background(), "Book", "Ch", Roster{Names: []string{"Bert"}}, paras, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantSpeakers := map[int]string{0: "Guard", 1: "Guard", 2: "Bert", 3: "Guard", 4: "Guard", 5: "Unit 7", 6: "Guard"}
	wantIndexes := map[int]int{0: 1, 1: 2, 3: 1, 4: 2}
	if !maps.Equal(out, wantSpeakers) {
		t.Errorf("speakers = %v, want %v", out, wantSpeakers)
	}
	if !maps.Equal(indexes, wantIndexes) {
		t.Errorf("indexes = %v, want %v", indexes, wantIndexes)
	}
	if !roles["Guard"] || roles["Unit 7"] {
		t.Errorf("roles = %v, want Guard only", roles)
	}

	// A reader-set line keeps the reader's own number.
	paras[1].Known, paras[1].KnownIndex = "Guard", 3
	_, indexes, _, _, _, err = NewClient(Config{}, numberedGuardsLLM{}).AttributeChapter(context.Background(), "Book", "Ch", Roster{Names: []string{"Bert", "Guard"}, Roles: map[string]bool{"Guard": true}}, paras, nil)
	if err != nil {
		t.Fatal(err)
	}
	if indexes[1] != 3 {
		t.Errorf("reader-set line's index = %d, want 3", indexes[1])
	}
}

// A role name that carries its own number - from a chapter's cast, or an
// older roster - comes out as that number of the plain label.
func TestAttributeChapterNumberedRoleNames(t *testing.T) {
	paras := []ParagraphInput{
		{Idx: 0, Text: "“Put him down.”", IsQuote: true},
		{Idx: 1, Text: "“Please.”", IsQuote: true},
	}
	llm := scriptedLLM(func(idx int, _ string) string {
		return map[int]string{0: "Thug 1 [role]", 1: "Thug 2"}[idx]
	})
	roster := Roster{Names: []string{"Thug", "Thug 1", "Thug 2"}, Roles: map[string]bool{"Thug": true, "Thug 1": true, "Thug 2": true}}
	out, indexes, roles, _, _, err := NewClient(Config{}, llm).AttributeChapter(context.Background(), "Book", "Ch", roster, paras, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int]string{0: "Thug", 1: "Thug"}; !maps.Equal(out, want) {
		t.Errorf("speakers = %v, want %v", out, want)
	}
	if want := map[int]int{0: 1, 1: 2}; !maps.Equal(indexes, want) {
		t.Errorf("indexes = %v, want %v", indexes, want)
	}
	if !roles["Thug"] || roles["Thug 1"] || roles["Thug 2"] {
		t.Errorf("roles = %v, want Thug only", roles)
	}
}

// Two guards talking across a batch seam: the second batch, asked on its
// own, numbers the guard it opens with 1; shown the lines before the seam
// with their numbers, it keeps him as 2.
func TestAttributeChapterNumbersAcrossSeam(t *testing.T) {
	var paras []ParagraphInput
	for i := 0; i < maxBatchParagraphs+5; i++ {
		paras = append(paras, ParagraphInput{Idx: i, Text: fmt.Sprintf("“Line %d.”", i), IsQuote: true})
	}
	last := maxBatchParagraphs - 1
	seamShown := fmt.Sprintf("%d (Guard 2):", last)
	var asked atomic.Int32 // prompts asking about the second batch's first line
	llm := scriptedLLM(func(idx int, user string) string {
		if idx == last+1 {
			asked.Add(1)
		}
		switch {
		case idx == last-1:
			return "Guard 1 [role]"
		case idx == last:
			return "Guard 2 [role]"
		case idx == last+1 && strings.Contains(user, seamShown):
			return "Guard 2 [role]"
		case idx == last+1:
			return "Guard 1 [role]"
		}
		return "Bert"
	})
	out, indexes, _, _, _, err := NewClient(Config{}, llm).AttributeChapter(context.Background(), "Book", "Ch", Roster{Names: []string{"Bert"}}, paras, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out[last+1] != "Guard" || indexes[last+1] != 2 {
		t.Errorf("line after the seam = %q #%d, want Guard #2", out[last+1], indexes[last+1])
	}
	if out[last+2] != "Bert" {
		t.Errorf("a line the role doesn't touch = %q, want its first answer Bert", out[last+2])
	}
	if n := asked.Load(); n != 2 {
		t.Errorf("the second batch was asked %d times, want 2 (once, then once more for the seam)", n)
	}
	if indexes[last-1] != 1 || indexes[last] != 2 {
		t.Errorf("indexes before the seam = %d, %d; want 1, 2", indexes[last-1], indexes[last])
	}
}

// scriptedLLM answers every "(who?)" line of an attribution prompt with
// answer(idx, prompt).
type scriptedLLM func(idx int, user string) string

var whoLineRe = regexp.MustCompile(`(?m)^(\d+) \(who\?\):`)

func (f scriptedLLM) LLMGenerate(_ context.Context, _, user string, _ float32, _ int) (string, error) {
	var b strings.Builder
	for _, m := range whoLineRe.FindAllStringSubmatch(user, -1) {
		idx, _ := strconv.Atoi(m[1])
		fmt.Fprintf(&b, "%d: %s\n", idx, f(idx, user))
	}
	return b.String(), nil
}

func TestSplitRoleOne(t *testing.T) {
	names := []string{"Thug Two", "Old One", "Bert"}
	if label, ok := splitRoleOne("Thug One [role]", slices.Values(names)); !ok || label != "Thug [role]" {
		t.Errorf(`splitRoleOne("Thug One [role]") = %q, %v; want "Thug [role]", true`, label, ok)
	}
	if label, ok := splitRoleOne("Old One", slices.Values(names)); ok {
		t.Errorf(`splitRoleOne("Old One") = %q, true; want it left alone`, label)
	}
}

func TestShareParagraphNumbers(t *testing.T) {
	paras := []ParagraphInput{
		{Idx: 0, Text: "“Not before me,”", IsQuote: true},
		{Idx: 1, Text: "the guard with the lantern growled,", Inline: true},
		{Idx: 2, Text: "“He just cost me a gold triad.”", IsQuote: true, Inline: true},
		{Idx: 3, Text: "“How’s that?”", IsQuote: true},
		{Idx: 4, Text: "“Easy,”", IsQuote: true},
		{Idx: 5, Text: "“Quiet,”", IsQuote: true, Inline: true},
	}
	speakers := map[int]string{0: "Guard", 2: "Guard", 3: "Guard", 4: "Guard", 5: "Guard"}
	indexes := map[int]int{2: 1, 4: 2, 5: 3}
	shareParagraphNumbers(paras, speakers, indexes)
	if want := map[int]int{0: 1, 2: 1, 4: 2, 5: 3}; !maps.Equal(indexes, want) {
		t.Errorf("indexes = %v, want %v", indexes, want)
	}
}
