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
