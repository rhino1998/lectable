package speakerattr

import (
	"reflect"
	"testing"
)

func TestVoiceClues(t *testing.T) {
	tags := []string{
		"Sid's gruff voice said from behind.",
		"he said, a tremble creeping into his normally steady voice.",
		"Bert answered, though he didn't bother to lower his voice.",
		"Vee's calming voice interrupted Sid's furious thoughts.",
		"Bert corrected, a hint of fatigue in his voice.",
		"she said, turning to the door.",
		"he asked, and she repeated it, her voice strangely melodic.",
	}
	paragraphs := []string{
		"The door opened. Sid was nineteen years old and looked it.",
		"The goblin screamed in a high-pitched voice and looked up at Sid.",
		"Far away a baritone sang. Much later, after the storm had passed and the crew had eaten and slept and woken again to grey light, the watch changed and Sid went below.",
	}
	got := VoiceClues(tags, paragraphs, []string{"Sid"}, nil, "he/him", 8)
	want := []string{
		"Sid's gruff voice said from behind.",
		"he said, a tremble creeping into his normally steady voice.",
		"Sid was nineteen years old and looked it.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("VoiceClues = %q, want %q", got, want)
	}
	if got := VoiceClues(tags, paragraphs, []string{"Sid"}, nil, "he/him", 1); len(got) != 1 {
		t.Fatalf("limit 1 returned %d clues", len(got))
	}
}

func TestGenderContradicts(t *testing.T) {
	cases := []struct {
		instruct, pronouns string
		want               bool
	}{
		{"Speak as a tiny woman. Use a high-pitched voice to reflect her size.", "he/him", true},
		{"Speak as a middle-aged man with a General American accent.", "he/him", false},
		{"Speak as a young woman, brisk and human.", "she/her", false},
		{"Speak as a teenage boy.", "she/her", true},
		{"Speak as a young adult non-human entity with a flat timbre.", "he/him", true},
		{"Speak as a tiny woman.", "they/them", false},
		{"Speak as a tiny woman.", "", false},
	}
	for _, c := range cases {
		if got := genderContradicts(c.instruct, c.pronouns); got != c.want {
			t.Errorf("genderContradicts(%q, %q) = %v, want %v", c.instruct, c.pronouns, got, c.want)
		}
	}
}

func TestStatedAges(t *testing.T) {
	lines := []SpokenLine{
		{Text: "“I am nineteen years old and I know my own mind.”"},
		{Text: "“I’m twenty feet from the door.”"},
		{Text: "“I was twelve when the war came.”"},
		{Text: "“I’m one of them.”"},
		{Text: "“I…I’m seventy-six, this past spring,”", Asked: "“Look, how old are you, Yen?”"},
		{Text: "“Old enough.”", Asked: "“And how old are you?”"},
		{Text: "“I'm fifty-fifty on that.”", Asked: "“Will it hold?”"},
		{Text: "“I’m a hundred and four.”"},
	}
	want := []string{
		"“I…I’m seventy-six, this past spring,” [answering: “Look, how old are you, Yen?”]",
		"“Old enough.” [answering: “And how old are you?”]",
		"“I am nineteen years old and I know my own mind.”",
		"“I’m a hundred and four.”",
	}
	if got := StatedAges(lines, 8); !reflect.DeepEqual(got, want) {
		t.Fatalf("StatedAges = %q, want %q", got, want)
	}
	if got := StatedAges(lines, 1); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("limit 1 = %q", got)
	}
}

// A first name someone else in the roster goes by tells nothing of this
// character: the orphan Thomas's age is not Thomas Oh's.
func TestVoiceCluesSkipSharedFirstName(t *testing.T) {
	paragraphs := VoiceParagraphs([]string{"Thomas, an eight-year-old boy protested."})
	if got := VoiceClues(nil, paragraphs, []string{"Thomas Oh"}, nil, "he/him", 8); len(got) != 1 {
		t.Fatalf("unshared first name: got %q, want the sentence", got)
	}
	shared := SharedNames([]string{"Thomas (orphan)", "Will Oh"})
	if got := VoiceClues(nil, paragraphs, []string{"Thomas Oh"}, shared, "he/him", 8); len(got) != 0 {
		t.Fatalf("shared first name: got %q, want none", got)
	}
}
