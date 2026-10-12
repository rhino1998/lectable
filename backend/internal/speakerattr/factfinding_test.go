package speakerattr

import "testing"

func TestFactKind(t *testing.T) {
	for in, want := range map[string]string{
		"Kobolds":          "kobold",
		"the Deep Dwarves": "deep dwarf",
		"elves":            "elf",
		"fae":              "fae",
		"C-grade":          "c-grade",
		"humans":           "",
		"Humanity":         "",
		"unknown kind":     "",
		"narrator's kind":  "",
		"unknown race (referred to as 'her people')": "",
		"nonhumans who worship gintax":               "",
		"":                                           "",
	} {
		if got := FactKind(in); got != want {
			t.Errorf("FactKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuotedIn(t *testing.T) {
	passages := []string{FactPassage("“You do realize that the kobold lifespan\nisn’t very long, correct?”"), "Kobolds only live to fifteen or so."}
	if got := QuotedIn("Kobolds only live to fifteen or so.", passages); got != 1 {
		t.Errorf("exact quote: got %d, want 1", got)
	}
	if got := QuotedIn(trimQuote("“You do realize that the kobold lifespan isn’t very long, correct?”"), passages); got != 0 {
		t.Errorf("quote across a line break, in quote marks: got %d, want 0", got)
	}
	if got := QuotedIn("Kobolds live to fifteen.", passages); got != -1 {
		t.Errorf("a paraphrase: got %d, want -1", got)
	}
}
