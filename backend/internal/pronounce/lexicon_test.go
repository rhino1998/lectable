package pronounce

import (
	"reflect"
	"testing"
)

func TestLexiconSubstitutions(t *testing.T) {
	lex := []LexiconEntry{
		{Term: "Nyx", Replacement: "Nicks"},
		{Term: "Mary", Replacement: "MAH-ree"},
		{Term: "Mary Anne", Replacement: "Marianne"},
		{Term: "SIG", Replacement: "sig", CaseSensitive: true},
	}
	cases := []struct {
		text string
		want string
	}{
		{"Nyx smiled.", "Nicks smiled."},
		{"NYX and nyx.", "Nicks and Nicks."},
		{"Nyxie stayed, Onyx too.", "Nyxie stayed, Onyx too."}, // whole words only
		{"Nyx's blade", "Nicks's blade"},                       // possessive keeps its suffix
		{"Mary Anne met Mary.", "Marianne met MAH-ree."},       // longest term first
		{"SIG vs sig", "sig vs sig"},                           // case-sensitive entry
		{"Café Nyx—now", "Café Nicks—now"},                     // multi-byte neighbors
	}
	for _, c := range cases {
		subs := LexiconSubstitutions(c.text, lex)
		if got := Apply(c.text, nil, subs); got != c.want {
			t.Errorf("%q -> %q, want %q", c.text, got, c.want)
		}
	}
	if subs := LexiconSubstitutions("anything", nil); subs != nil {
		t.Errorf("empty lexicon: %v", subs)
	}
}

func TestMergeLexiconOverridesOverlappingLLMFixes(t *testing.T) {
	lex := []Substitution{{Offset: 4, Length: 3, Replacement: "Saint"}}
	pron := []Substitution{
		{Offset: 0, Length: 3, Replacement: "Doctor"},
		{Offset: 4, Length: 3, Replacement: "Street"}, // same span: lexicon wins
		{Offset: 5, Length: 1, Replacement: "x"},      // partial overlap: dropped
		{Offset: 8, Length: 2, Replacement: "two"},
	}
	want := []Substitution{
		{Offset: 0, Length: 3, Replacement: "Doctor"},
		{Offset: 4, Length: 3, Replacement: "Saint"},
		{Offset: 8, Length: 2, Replacement: "two"},
	}
	if got := MergeLexicon(lex, pron); !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeLexicon = %+v, want %+v", got, want)
	}
	if got := MergeLexicon(nil, pron); !reflect.DeepEqual(got, pron) {
		t.Fatal("no lexicon should return pron unchanged")
	}
}
