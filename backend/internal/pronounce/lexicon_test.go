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
		{"NYX and nyx.", "NICKS and Nicks."},                   // all-caps original -> all-caps
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

func TestLexiconPrefixRules(t *testing.T) {
	lex := []LexiconEntry{{Term: "gaol", Replacement: "jail", Prefix: true}}
	cases := map[string]string{
		"The gaol was cold.":           "The jail was cold.",
		"Gaolers and GAOLS alike.":     "Jailers and JAILS alike.",
		"A gaolbird, gaoled at last.":  "A jailbird, jailed at last.",
		"Nothing about a rogaol here.": "Nothing about a rogaol here.", // still a word start
	}
	for in, want := range cases {
		if got := Apply(in, nil, LexiconSubstitutions(in, lex)); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestLexiconMatchedWords(t *testing.T) {
	rule := LexiconEntry{Term: "gaol", Replacement: "jail", Prefix: true}
	got := LexiconMatchedWords("The gaoler left the Gaol; gaols everywhere, rogaol not.", rule)
	if !reflect.DeepEqual(got, []string{"gaoler", "Gaol", "gaols"}) {
		t.Errorf("prefix = %q", got)
	}
	whole := LexiconEntry{Term: "Nyx", Replacement: "Nicks"}
	if got := LexiconMatchedWords("Nyx and Nyxie", whole); !reflect.DeepEqual(got, []string{"Nyx"}) {
		t.Errorf("whole word = %q", got)
	}
}

// TestDemesneRule pins the All-books rule that replaced speakerattr's old
// hard-coded demesne respelling, with the same results.
func TestDemesneRule(t *testing.T) {
	lex := []LexiconEntry{{Term: "demesne", Replacement: "domain", Prefix: true}}
	for in, want := range map[string]string{
		"He walked the bounds of his demesne.": "He walked the bounds of his domain.",
		"The lords' demesnes stretched east.":  "The lords' domains stretched east.",
		"Demesne law applied there.":           "Domain law applied there.",
		"THE DEMESNES OF THE KING":             "THE DOMAINS OF THE KING",
		"The demesne's borders held.":          "The domain's borders held.",
	} {
		if got := Apply(in, nil, LexiconSubstitutions(in, lex)); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
