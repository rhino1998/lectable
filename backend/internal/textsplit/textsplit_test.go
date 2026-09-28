package textsplit

import (
	"reflect"
	"testing"
)

func TestSplitSentences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   \n\t", nil},
		{"no boundary", "Just one clause without an end", []string{"Just one clause without an end"}},
		{"single sentence", "  Hello there.  ", []string{"Hello there."}},
		{"mixed terminators", "Stop! Who goes there? It is I.", []string{"Stop!", "Who goes there?", "It is I."}},
		{"repeated punctuation", "Really?! Yes... fine.", []string{"Really?!", "Yes...", "fine."}},
		{"straight closing quote", `"Run." She ran.`, []string{`"Run."`, "She ran."}},
		{"curly closing quote", "“Run.” She ran.", []string{"“Run.”", "She ran."}},
		{"guillemet and paren", "«Non.» (Fine.) Done", []string{"«Non.»", "(Fine.)", "Done"}},
		{"newline boundary", "One.\nTwo.", []string{"One.", "Two."}},
		// Known, accepted imprecision: abbreviations split too (see
		// sentenceBoundaryRe's doc comment).
		{"abbreviation splits", "Mr. Smith left.", []string{"Mr.", "Smith left."}},
		{"no space after period", "3.14 is pi.", []string{"3.14 is pi."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SplitSentences(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("SplitSentences(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
