package speakerattr

import "testing"

func TestScareQuoteCandidates(t *testing.T) {
	ps := []ParagraphInput{
		// "my “friends” were in danger" - mid-sentence.
		{Idx: 0, Text: "The last time, my"},
		{Idx: 1, Text: "“friends”", IsQuote: true, Inline: true},
		{Idx: 2, Text: "were in danger.", Inline: true},
		// "“Why?” Ingrid asked." - dialogue opening its paragraph.
		{Idx: 3, Text: "“Why?”", IsQuote: true},
		{Idx: 4, Text: "Ingrid asked.", Inline: true},
		// "She declared, “Getting married!”" - after a comma.
		{Idx: 5, Text: "She declared,"},
		{Idx: 6, Text: "“Getting married!”", IsQuote: true, Inline: true},
		// "“Toy yachts” lined the harbor." - short, bare, sentence goes on.
		{Idx: 7, Text: "“Toy yachts”", IsQuote: true},
		{Idx: 8, Text: "lined the harbor.", Inline: true},
		// A standalone line of dialogue.
		{Idx: 9, Text: "“Run!”", IsQuote: true},
	}
	got := scareQuoteCandidates(ps)
	want := map[int]bool{1: true, 7: true}
	if len(got) != len(want) || !got[1] || !got[7] {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

func TestParseScareQuoteVerdicts(t *testing.T) {
	asked := []int{3, 5, 9}
	got, err := parseScareQuoteVerdicts("3: not spoken\n5 (?): Spoken\n9 - Not Spoken\n12: not spoken\n", asked)
	if err != nil || !got[3] || got[5] || !got[9] || got[12] {
		t.Fatalf("verdicts = %v, %v (want 3 and 9 only; 12 wasn't asked)", got, err)
	}
	if got, err := parseScareQuoteVerdicts(`[{"idx": 5}]`, asked); err != nil || !got[5] {
		t.Fatalf("old JSON shape = %v, %v", got, err)
	}
	if _, err := parseScareQuoteVerdicts("I can't tell.", asked); err == nil {
		t.Fatal("a reply with no verdicts should fail (and so retry)")
	}
	// A miscopied number (214 answered as "14") is taken by position when
	// the reply has one verdict per asked line.
	if got, err := parseScareQuoteVerdicts("14: not spoken", []int{214}); err != nil || !got[214] || len(got) != 1 {
		t.Fatalf("miscopied single number = %v, %v (want 214)", got, err)
	}
	if got, err := parseScareQuoteVerdicts("3: spoken\n15: not spoken\n9: spoken", asked); err != nil || !got[5] || len(got) != 1 {
		t.Fatalf("miscopied middle number = %v, %v (want 5)", got, err)
	}
	// Asked numbers out of place, or a count that doesn't match: by number only.
	if got, err := parseScareQuoteVerdicts("5: not spoken\n3: spoken\n12: not spoken", asked); err != nil || !got[5] || len(got) != 1 {
		t.Fatalf("out-of-order reply = %v, %v (want 5 only)", got, err)
	}
	if _, err := parseScareQuoteVerdicts("14: not spoken\n15: spoken", []int{214}); err == nil {
		t.Fatal("two unasked verdicts for one asked line should fail")
	}
	// Only "spoken" verdicts, for lines that weren't asked: nothing is
	// flagged whichever lines were meant.
	if got, err := parseScareQuoteVerdicts("132: spoken", []int{122, 152}); err != nil || len(got) != 0 {
		t.Fatalf("unasked spoken verdict = %v, %v (want no flags)", got, err)
	}
}
