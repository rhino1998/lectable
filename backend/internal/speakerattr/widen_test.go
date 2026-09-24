package speakerattr

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// fakeAttributor answers every "N (who?): text" line in an attribution
// prompt with speaker(idx, window) as a plain "N: speaker" reply line,
// recording each call's window of line idxs (answered or context).
type fakeAttributor struct {
	speaker func(idx int, window []int) string
	calls   [][]int
}

var promptLine = regexp.MustCompile(`(?m)^(\d+)( \(who\?\))?: `)

func (f *fakeAttributor) LLMGenerate(_ context.Context, _, user string, _ float32, _ int) (string, error) {
	var window, asked []int
	for _, m := range promptLine.FindAllStringSubmatch(user, -1) {
		idx, _ := strconv.Atoi(m[1])
		window = append(window, idx)
		if m[2] != "" {
			asked = append(asked, idx)
		}
	}
	f.calls = append(f.calls, window)
	var out strings.Builder
	for _, idx := range asked {
		fmt.Fprintf(&out, "%d: %s\n", idx, f.speaker(idx, window))
	}
	return out.String(), nil
}

func chapterOf(n int) []ParagraphInput {
	all := make([]ParagraphInput, n)
	for i := range all {
		all[i] = ParagraphInput{Idx: i, Text: fmt.Sprintf("line %d", i), IsQuote: true}
	}
	return all
}

func contains(window []int, idx int) bool {
	for _, w := range window {
		if w == idx {
			return true
		}
	}
	return false
}

func TestAttributeWithWideningContextGrowsUntilAccepted(t *testing.T) {
	// Line 50's speaker is only identifiable once line 15 is in view -
	// more than radius 20 away, within radius 40.
	fake := &fakeAttributor{speaker: func(idx int, window []int) string {
		if idx == 50 && contains(window, 15) {
			return "Alice"
		}
		return "Bogus"
	}}
	c := NewClient(Config{}, fake)
	out, _, err := c.AttributeWithWideningContext(context.Background(), "Book", "Ch", Roster{}, chapterOf(200), []int{50}, func(s string) bool { return s == "Bogus" })
	if err != nil {
		t.Fatal(err)
	}
	if out[50] != "Alice" {
		t.Fatalf("out[50] = %q; want Alice", out[50])
	}
	if len(fake.calls) != 2 {
		t.Fatalf("calls = %d; want 2 (radius 20 rejected, radius 40 accepted)", len(fake.calls))
	}
	if first := fake.calls[0]; first[0] != 30 || first[len(first)-1] != 70 {
		t.Fatalf("first window = %d-%d; want 30-70", first[0], first[len(first)-1])
	}
}

func TestAttributeWithWideningContextStopsWhenWindowCantGrow(t *testing.T) {
	fake := &fakeAttributor{speaker: func(int, []int) string { return "Bogus" }}
	c := NewClient(Config{}, fake)
	out, _, err := c.AttributeWithWideningContext(context.Background(), "Book", "Ch", Roster{}, chapterOf(10), []int{3, 7}, func(s string) bool { return s == "Bogus" })
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("out = %v; want nothing accepted", out)
	}
	// Radius 20 already spans the whole 10-line chapter, and covers both
	// targets in one call; wider radii would repeat the identical prompt.
	if len(fake.calls) != 1 {
		t.Fatalf("calls = %d; want 1", len(fake.calls))
	}
}

func TestAttributeWithWideningContextSharesWindows(t *testing.T) {
	fake := &fakeAttributor{speaker: func(idx int, _ []int) string { return "Alice" }}
	c := NewClient(Config{}, fake)
	out, _, err := c.AttributeWithWideningContext(context.Background(), "Book", "Ch", Roster{}, chapterOf(300), []int{100, 110, 250}, func(s string) bool { return s == "Bogus" })
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("out = %v; want all three accepted", out)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("calls = %d; want 2 (100 and 110 share one window)", len(fake.calls))
	}
}

func TestWidenWindowRespectsCharBudget(t *testing.T) {
	all := chapterOf(100)
	for i := range all {
		all[i].Text = string(make([]byte, maxWidenWindowChars/10))
	}
	lo, hi := widenWindow(all, 50, 80)
	if n := hi - lo + 1; n > 10 || n < 9 {
		t.Fatalf("window %d-%d (%d lines); want ~10 under the char budget", lo, hi, n)
	}
	if 50-lo > hi-50+1 || hi-50 > 50-lo+1 {
		t.Fatalf("window %d-%d not centred on 50", lo, hi)
	}
}
