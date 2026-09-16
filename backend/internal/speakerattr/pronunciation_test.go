package speakerattr

import "testing"

// findTextFor returns the first pronunciationTerm found for text whose
// Candidates include want as an option, or ("", nil, false) if none did -
// a small helper so each homograph case below can assert both that the
// term was found and what its own candidate list looks like.
func findTermFor(text string) (pronunciationTerm, bool) {
	terms := findPronunciationTerms([]ParagraphInput{{Idx: 0, Text: text}})
	if len(terms) == 0 {
		return pronunciationTerm{}, false
	}
	return terms[0], true
}

func TestHomographDetection(t *testing.T) {
	cases := []struct {
		name           string
		text           string
		wantCandidates []string
	}{
		{"read lowercase", "I read the book every night.", []string{"reed", "red"}},
		{"Read capitalized", "Read carefully before you sign.", []string{"Reed", "Red"}},
		{"lead lowercase", "She will lead the expedition.", []string{"leed", "led"}},
		{"wind lowercase", "The wind picked up outside.", []string{"winned", "wined"}},
		{"tear lowercase", "A tear rolled down her cheek.", []string{"tare", "tier"}},
		{"bow lowercase", "He drew his bow and aimed.", []string{"beau", "bough"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			term, ok := findTermFor(tc.text)
			if !ok {
				t.Fatalf("findPronunciationTerms(%q): no term found", tc.text)
			}
			if len(term.Candidates) != 2 || term.Candidates[0] != tc.wantCandidates[0] || term.Candidates[1] != tc.wantCandidates[1] {
				t.Errorf("findPronunciationTerms(%q) candidates = %v, want %v", tc.text, term.Candidates, tc.wantCandidates)
			}
		})
	}
}

// TestHomographWordBoundaries guards against a homograph pattern matching
// inside an unrelated longer word - "read" inside "already", "lead" inside
// "leader", "wind" inside "window", "tear" inside "tearful", "bow" inside
// "elbow" - none of these should ever produce a pronunciation term.
func TestHomographWordBoundaries(t *testing.T) {
	texts := []string{
		"They had already left by then.",
		"She is a natural leader.",
		"He looked out the window.",
		"The movie was tearful and long.",
		"He rested his elbow on the table.",
		"The bowl was empty.",
	}
	for _, text := range texts {
		if terms := findPronunciationTerms([]ParagraphInput{{Idx: 0, Text: text}}); len(terms) != 0 {
			t.Errorf("findPronunciationTerms(%q) = %+v, want no matches", text, terms)
		}
	}
}

func TestCaseMatch(t *testing.T) {
	if got := caseMatch("Read", "red"); got != "Red" {
		t.Errorf("caseMatch(%q, %q) = %q, want %q", "Read", "red", got, "Red")
	}
	if got := caseMatch("read", "red"); got != "red" {
		t.Errorf("caseMatch(%q, %q) = %q, want %q", "read", "red", got, "red")
	}
}

// TestFixedAbbreviationExpansion covers the single-candidate, never-hits-
// the-LLM abbreviation expansions (etc./e.g./i.e./vs./approx./Jr./Sr.) -
// each should resolve to exactly one reading.
func TestFixedAbbreviationExpansion(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"He brought apples, oranges, etc. to the party.", "et cetera"},
		{"Bring warm clothes, e.g. a coat.", "for example"},
		{"It was late, i.e. past midnight.", "that is"},
		{"It was Reds vs. Blues that day.", "versus"},
		{"It took approx. an hour.", "approximately"},
		{"John Smith Jr. arrived first.", "Junior"},
		{"John Smith Sr. arrived last.", "Senior"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			term, ok := findTermFor(tc.text)
			if !ok {
				t.Fatalf("findPronunciationTerms(%q): no term found", tc.text)
			}
			if len(term.Candidates) != 1 || term.Candidates[0] != tc.want {
				t.Errorf("findPronunciationTerms(%q) candidates = %v, want [%q]", tc.text, term.Candidates, tc.want)
			}
		})
	}
}

// TestRomanNumeralDetection covers romanNumeralCandidates: a plausible
// person-plus-numeral pair gets an ordinal option, a denylisted leading
// word or a single-letter numeral (I/V/X alone) never does.
func TestRomanNumeralDetection(t *testing.T) {
	term, ok := findTermFor("Henry VIII married six times.")
	if !ok {
		t.Fatalf("findPronunciationTerms: no term found for %q", "Henry VIII married six times.")
	}
	want := []string{"Henry VIII", "Henry the eighth"}
	if len(term.Candidates) != 2 || term.Candidates[0] != want[0] || term.Candidates[1] != want[1] {
		t.Errorf("candidates = %v, want %v", term.Candidates, want)
	}

	noMatch := []string{
		"World War II ended in 1945.",   // denylisted leading word
		"Chapter IV begins here.",       // denylisted leading word
		"Henry V won at Agincourt.",     // single-letter numeral excluded
		"Model X rolled off the line.",  // single-letter numeral excluded
		"Elizabeth is coming to visit.", // no numeral at all
	}
	for _, text := range noMatch {
		if terms := findPronunciationTerms([]ParagraphInput{{Idx: 0, Text: text}}); len(terms) != 0 {
			t.Errorf("findPronunciationTerms(%q) = %+v, want no matches", text, terms)
		}
	}
}

func TestRomanToIntRoundTrip(t *testing.T) {
	for n := 2; n < len(dayOrdinals); n++ {
		r := intToRoman(n)
		got, ok := romanToInt(r)
		if !ok || got != n {
			t.Errorf("romanToInt(intToRoman(%d)=%q) = %d, %v, want %d, true", n, r, got, ok, n)
		}
	}
}
