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
		{"READ upper case", "DO NOT READ THIS.", []string{"REED", "RED"}},
		{"CLOSE upper case", "“CLOSE THE DOOR!”", []string{"CLOHSS", "CLOZE"}},
		{"lead lowercase", "She will lead the expedition.", []string{"leed", "led"}},
		{"wind lowercase", "The wind picked up outside.", []string{"winned", "wined"}},
		{"tear lowercase", "A tear rolled down her cheek.", []string{"tare", "tier"}},
		{"bow lowercase", "He drew his bow and aimed.", []string{"beau", "bough"}},
		{"close lowercase", "Stay close to me.", []string{"clohss", "cloze"}},
		{"wound lowercase", "The wound bled freely.", []string{"woond", "wownd"}},
		{"live lowercase", "They live here.", []string{"liv", "lyve"}},
		{"lives lowercase", "Their lives changed.", []string{"lyves", "livz"}},
		{"minute lowercase", "Wait a minute.", []string{"minnit", "my-newt"}},
		{"tears lowercase", "She burst into tears.", []string{"tiers", "tares"}},
		{"Close capitalized", "Close the door.", []string{"Clohss", "Cloze"}},
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
		"She closed the door and moved closer.",
		"He watched them closely.",
		"His wounds had healed.",
		"She lived alone, a lively woman.",
		"It took ten minutes.",
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
		{"It dealt 2x damage.", "2x", []string{"two times"}},
		{"Experience gain: 10X", "10X", []string{"ten times"}},
		{"Played back at 1.5x speed.", "1.5x", []string{"one point five times"}},
		{"A 3× multiplier.", "3×", []string{"three times"}},
		{"“1x, 2x, 3x.”", "3x", []string{"three times"}},
		{"Released November 1, 1998 at last.", "1998", []string{"nineteen ninety eight"}},
		{"In 2001, the project began.", "2001", []string{"two thousand and one"}},
		{"Updated in 2025.", "2025", []string{"twenty twenty five"}},
		{"Built in 1905.", "1905", []string{"nineteen oh five"}},
		{"Built in 1900.", "1900", []string{"nineteen hundred"}},
		{"He owed 1500 gold.", "1500", []string{"one thousand five hundred"}},
		{"Since 1500 gold coins were lost.", "1500", []string{"one thousand five hundred"}},
		{"There was one man they hanged here in 1782 who had murdered.", "1782", []string{"seventeen eighty two"}},
		{"When McCabe arrived in 1922 and announced it.", "1922", []string{"nineteen twenty two"}},
		{"In 1922 McCabe arrived.", "1922", []string{"nineteen twenty two"}},
		{"“I gave up on you after 1527!”", "1527", []string{"fifteen twenty seven"}},
		{"Prologue – 1614 AD", "1614", []string{"sixteen fourteen"}},
		{"Copyright © 2022 by R J Cajiao", "2022", []string{"twenty twenty two"}},
		{"Copyright Macmillan, 1914, 1915, 1942.", "1942", []string{"nineteen forty two"}},
		{"It ran from 1914 to 1918.", "1918", []string{"nineteen eighteen"}},
		{"In June 1922 it rained.", "1922", []string{"nineteen twenty two"}},
		{"The War of 1812 ended.", "1812", []string{"eighteen twelve"}},
		{"By 1922, he owned it all.", "1922", []string{"nineteen twenty two"}},
		{"Awakened in the year of the Lord, 1719.", "1719", []string{"seventeen nineteen"}},
		// An uncued number before a capitalized word is the LLM's call.
		{"A hearselike 1939 Chrysler limousine.", "1939", []string{"one thousand nine hundred and thirty nine", "nineteen thirty nine"}},
		{"He had 1500 Mana left.", "1500", []string{"one thousand five hundred", "fifteen hundred"}},
		{"He had 2000 Mana left.", "2000", []string{"two thousand"}},
		{"He had 1500 mana left.", "1500", []string{"one thousand five hundred"}},
		{"My mana increased by 1250.", "1250", []string{"one thousand two hundred and fifty"}},
		{"A total of 1782.", "1782", []string{"one thousand seven hundred and eighty two"}},
		{"1300", "1300", []string{"one thousand three hundred"}},
		{"Bullets travel at around 2000 mph.", "2000", []string{"two thousand"}},
		{"In 1500 years it would be dust.", "1500", []string{"one thousand five hundred"}},
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
		"A 2x4 plank.",             // dimensions, not a multiplier
		"A 2xl shirt.",             // glued to more letters
		"Address 0x1F.",            // hex literal
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
		{"It was too close to call.", false, 0, true},
		{"She stepped close.", false, 0, true},
		{"A close-knit family.", false, 0, true},
		{"He held her close.", false, 0, true},
		{"They were close friends.", false, 0, true},
		{"He turned to close the door.", false, 1, true},
		{"Close your eyes.", true, 1, true},
		{"The day drew to a close.", false, 1, true},
		{"At the close of the day.", false, 1, true},
		{"The gap wouldn’t close.", false, 1, true},
		{"He heard the door close behind him.", false, 0, false},
		{"“CLOSE THE DOOR!”", true, 1, true},
		{"“TOO CLOSE!”", true, 0, true},
		{"WARNING: LIVE FIRE AHEAD", false, 1, true},
		{"He had a deep wound.", false, 0, true},
		{"You wound me, sir!", true, 0, true},
		{"The path wound through the hills.", false, 1, true},
		{"He wound his way home.", false, 1, true},
		{"A narrow wound across his chest.", false, 0, true},
		{"They wanted to live.", false, 0, true},
		{"We live in the hills.", true, 0, true},
		{"He swallowed a live grenade.", false, 1, true},
		{"Then it went live.", false, 1, true},
		{"The screen was still live.", false, 1, true},
		{"Let the soldier live.", true, 0, false},
		{"They fought for their lives.", false, 0, true},
		{"Thirty billion lives in this sector.", false, 0, true},
		{"She lives, though she won’t wake.", true, 1, true},
		{"I decide who lives in my lands!", true, 1, true},
		{"He said Dave lives with me.", false, 1, true},
		{"Give me a minute.", true, 0, true},
		{"He noticed the minute details.", false, 1, true},
		{"Some last-minute changes.", false, 0, true},
		{"Do it this very minute.", true, 0, true},
		{"Tears ran down her face.", false, 0, true},
		{"She dashed her tears away.", false, 0, true},
		{"There were tears in the fabric of the world.", false, 1, true},
		{"The beast tears through the wall.", false, 1, true},
		{"Stab, close, stab.", false, 0, false},
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

func TestContextSnippet(t *testing.T) {
	texts := map[int]string{
		4: "The sword rose. He could not roll aside.",
		5: "“Close, close, CLOSE!”",
		6: "I wheezed out, willing the door to close. Then light.",
	}
	i := strings.Index(texts[5], "CLOSE")
	got := contextSnippet(texts, 5, i, i+5)
	want := "The sword rose. He could not roll aside. “Close, close, ⟦CLOSE⟧!” I wheezed out, willing the door to close. Then light."
	if got != want {
		t.Errorf("contextSnippet = %q, want %q", got, want)
	}
	// No neighbours: just the paragraph.
	if got := contextSnippet(map[int]string{5: texts[5]}, 5, i, i+5); got != "“Close, close, ⟦CLOSE⟧!”" {
		t.Errorf("contextSnippet alone = %q", got)
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
