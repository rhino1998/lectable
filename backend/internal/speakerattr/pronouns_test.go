package speakerattr

import "testing"

func TestParseAttributionLinesPronouns(t *testing.T) {
	got, err := parseAttributionLines("12: Gandalf | he\n14: Mira | She/Her\n15: Guard [role]\n16: Unknown\n17: Bert | ?", map[int]bool{12: true, 14: true, 15: true, 16: true, 17: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]Attributed{
		12: {Speaker: "Gandalf", Pronouns: PronounsHe},
		14: {Speaker: "Mira", Pronouns: PronounsShe},
		15: {Speaker: "Guard [role]", Role: true},
		16: {Speaker: "Unknown"},
		17: {Speaker: "Bert"},
	}
	for idx, w := range want {
		if got[idx] != w {
			t.Errorf("%d: got %+v, want %+v", idx, got[idx], w)
		}
	}
}

func TestPronounVotesMajority(t *testing.T) {
	v := pronounVotes{}
	v.add("Mira", PronounsShe)
	v.add("Mira", PronounsShe)
	v.add("Mira", PronounsHe)
	v.add("Toby", PronounsHe)
	v.add("Toby", PronounsShe)
	v.add("Unknown", PronounsHe)
	got := v.winners()
	if len(got) != 1 || got["Mira"] != PronounsShe {
		t.Fatalf("got %v, want only Mira=she/her", got)
	}
}
