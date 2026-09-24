package speakerattr

import (
	"slices"
	"strings"
	"testing"
)

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

	headings := []struct{ text, want string }{
		{"World War II ended.", "War two"}, // span is "War II"
		{"Chapter IV begins here.", "Chapter four"},
		{"ACT II", "Act two"},
		{"SCENE III. A wood.", "Scene three"},
	}
	for _, tc := range headings {
		term, ok := findTermFor(tc.text)
		if !ok || len(term.Candidates) != 1 || term.Candidates[0] != tc.want {
			t.Errorf("findPronunciationTerms(%q) = %+v, want single candidate %q", tc.text, term, tc.want)
		}
	}

	noMatch := []string{
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

func TestNumberWords(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "zero"},
		{7, "seven"},
		{40, "forty"},
		{105, "one hundred and five"},
		{1005, "one thousand and five"},
		{7456, "seven thousand four hundred and fifty six"},
		{1000000, "one million"},
		{2000050, "two million and fifty"},
		{1234567, "one million two hundred and thirty four thousand five hundred and sixty seven"},
	}
	for _, tc := range cases {
		if got := numberWords(tc.n); got != tc.want {
			t.Errorf("numberWords(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestNumberTerms covers findNumberTerms end to end through
// findPronunciationTerms: each text should produce exactly one term with
// exactly these candidates.
func TestNumberTerms(t *testing.T) {
	cases := []struct {
		text string
		span string
		want []string
	}{
		{"He paid 7456 coins.", "7456", []string{"seven thousand four hundred and fifty six"}},
		{"About 7,456 of them.", "7,456", []string{"seven thousand four hundred and fifty six"}},
		{"Technique 1/3", "1/3", []string{"one of three"}},
		{"Strain 0/3", "0/3", []string{"zero of three"}},
		{"Technique 3/3", "3/3", []string{"three of three"}},
		{"Capacity: 2/3", "2/3", []string{"two of three"}},
		{"He owned 2/3 of it.", "2/3", []string{"two thirds"}},
		{"Capacity: 8/12", "8/12", []string{"eight of twelve"}},
		{"Mana is at 45/300 of max.", "45/300", []string{"forty five three hundredths"}},
		{"Half of it, 1/2, was gone.", "1/2", []string{"one half"}},
		{"Nearly 3/4 of them.", "3/4", []string{"three quarters"}},
		// A date is only offered after a date-style preposition.
		{"It was due on 3/4.", "3/4", []string{"three quarters", "March the fourth"}},
		{"“On 12/25, we left.”", "12/25", []string{"twelve twenty fifths", "December the twenty-fifth"}},
		{"It was 3.5 metres.", "3.5", []string{"three point five"}},
		{"Her 21st birthday.", "21st", []string{"twenty first"}},
		{"The 100th floor.", "100th", []string{"one hundredth"}},
		{"Floor 12, then 13.", "12", []string{"twelve"}},
		{"Released November 1, 1998 at last.", "1998", []string{"nineteen ninety eight"}},
		{"In 2001, the project began.", "2001", []string{"two thousand and one"}},
		{"Updated in 2025.", "2025", []string{"twenty twenty five"}},
		{"Built in 1905.", "1905", []string{"nineteen oh five"}},
		{"Built in 1900.", "1900", []string{"nineteen hundred"}},
		{"He owed 1500 gold.", "1500", []string{"one thousand five hundred"}},
		{"Since 1500 gold coins were lost.", "1500", []string{"one thousand five hundred"}},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			var term pronunciationTerm
			for _, tt := range findPronunciationTerms([]ParagraphInput{{Idx: 0, Text: tc.text}}) {
				if tt.Text == tc.span {
					term = tt
				}
			}
			if term.Text != tc.span || !slices.Equal(term.Candidates, tc.want) {
				t.Errorf("got %q %v, want %q %v", term.Text, term.Candidates, tc.span, tc.want)
			}
		})
	}

	skipped := []string{
		"It happened on 3/4/2024.", // full date
		"Meet at 3:45.",            // time
		"See section 1.2.3.",       // section number
		"See paragraph 1.E.8 now.", // Gutenberg-style section label
		"Version v2.x shipped.",    // glued to a letter and a dot
		"Agent 007 reporting.",     // leading zero
		"An A4 page.",              // glued to a letter
		"A 3D model.",              // glued to a letter
		"It cost $5.",              // currency
	}
	for _, text := range skipped {
		if terms := findPronunciationTerms([]ParagraphInput{{Idx: 0, Text: text}}); len(terms) != 0 {
			t.Errorf("findPronunciationTerms(%q) = %+v, want no matches", text, terms)
		}
	}
}

// TestNumberTermOverlap: "No. 5" keeps its own "No." term and still gets
// the number spelled out, as two non-overlapping substitutions.
func TestNumberTermOverlap(t *testing.T) {
	terms := findPronunciationTerms([]ParagraphInput{{Idx: 0, Text: "Room No. 5 was empty."}})
	if len(terms) != 2 || terms[0].Text != "No." || terms[1].Text != "5" || terms[1].Candidates[0] != "five" {
		t.Errorf("got %+v, want No. then 5", terms)
	}
}

func TestResolveHomographByGrammar(t *testing.T) {
	cases := []struct {
		text    string
		isQuote bool
		want    int
		wantOK  bool
	}{
		{"I couldn’t read it.", true, 0, true},
		{"She had read it twice.", false, 1, true},
		{"He was well-read.", true, 1, true},
		{"The sign read: Keep Out.", false, 1, true},
		{"It was a good read.", true, 0, true},
		{"Fritz read the note.", false, 1, true},
		{"I read that somewhere.", true, 0, false},
		{"The wind picked up.", false, 0, true},
		{"He used Second Wind.", false, 0, true},
		{"The party began to wind down.", false, 1, true},
		{"I will wind thee in my arms.", false, 1, true},
		{"Wind, the old man said, is fickle.", false, 0, true},
		{"It was heavy as lead.", false, 1, true},
		{"A lead-lined box.", false, 1, true},
		{"Lead the way.", true, 0, true},
		{"She took the lead.", false, 0, true},
		{"He lead them home.", false, 0, false},
		{"A tear rolled down her cheek.", false, 1, true},
		{"There was a tear in the fabric.", false, 0, true},
		{"The claws would tear him apart.", false, 0, true},
		{"Her tear-stained face.", false, 1, true},
		{"He gave a slight bow.", false, 1, true},
		{"It is proper to bow.", true, 1, true},
		{"She drew her bow and loosed an arrow.", false, 0, true},
		{"Waves broke over the ship’s bow.", false, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			term, ok := findTermFor(tc.text)
			if !ok || term.Kind == "" {
				t.Fatalf("no homograph term found")
			}
			got, gotOK := resolveHomographByGrammar(term.Kind, tc.text, term.Offset, term.Offset+term.Length, tc.isQuote)
			if gotOK != tc.wantOK || (gotOK && got != tc.want) {
				t.Errorf("got (%d, %v), want (%d, %v)", got, gotOK, tc.want, tc.wantOK)
			}
		})
	}
}

func TestMarkedSnippet(t *testing.T) {
	// Short paragraphs are shown whole; long ones are cut back to the
	// sentence around the term.
	pad := strings.Repeat("Filler words go on and on. ", 12)
	text := pad + "Then the wind blew hard across the plain. " + pad
	i := strings.Index(text, "wind")
	got := markedSnippet(text, i, i+4)
	want := "… Then the ⟦wind⟧ blew hard across the plain.…"
	if got != want {
		t.Errorf("markedSnippet = %q, want %q", got, want)
	}
}

func TestParsePronunciationChoices(t *testing.T) {
	terms := []pronunciationTerm{
		{ID: 0, Candidates: []string{"a", "b"}},
		{ID: 1, Candidates: []string{"a", "b"}},
		{ID: 2, Candidates: []string{"a", "b"}},
	}
	got := parsePronunciationChoices("Sure:\n```json\n{\"0\": \"B\", \"1\": \"a\", \"2\": \"C\", \"9\": \"B\"}\n```", terms)
	want := map[int]int{0: 1, 1: 0, 2: 0}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("choice[%d] = %d, want %d", id, got[id], w)
		}
	}
}
