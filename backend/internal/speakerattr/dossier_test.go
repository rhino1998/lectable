package speakerattr

import "testing"

func TestKindMentions(t *testing.T) {
	cases := []struct {
		text, name, word string
	}{
		{"“—find it. Anguin is a Goblin, so look low for evidence of a struggle.”", "Anguin", "goblin"},
		{"Kikri, the Elf, whispered to her Dwarven friend.", "Kikri", "elf"},
		{"The Hobgoblin, Heva, was older than he first noticed.", "Heva", "hobgoblin"},
		{"He was reminded of Grimmar, the Frost Giant chieftain.", "Grimmar", "frost giant"},
		{"Tern, the old Elf, straightened his bloodied robes.", "Tern", "elf"},
		{"But Loth was a kobold, and they had no way of telling her age.", "But Loth", "kobold"},
	}
	for _, c := range cases {
		found := false
		for _, m := range KindMentions(c.text) {
			if m.Name == c.name && m.Word == c.word {
				found = true
				if m.Sentence == "" {
					t.Errorf("%q: no sentence", c.text)
				}
			}
		}
		if !found {
			t.Errorf("%q: want %s/%s, got %+v", c.text, c.name, c.word, KindMentions(c.text))
		}
	}
	// Lower case after "was a", a kind people are called is a figure of
	// speech; capitalised, or set beside the name, it is taken.
	if got := KindMentions("For the most part, Will was a ghost, and always got the upper hand."); len(got) != 0 {
		t.Errorf("figure of speech: got %+v", got)
	}
	if got := KindMentions("the rain fell on the empty road all night."); len(got) != 0 {
		t.Errorf("plain narration: got %+v", got)
	}
}

func TestAgePhrase(t *testing.T) {
	for _, s := range []string{
		"The scrawny twelve year old was on his knees.",
		"Thomas, an eight-year-old boy protested.",
		"“I’m fifteen, asshole.”",
		"She had just turned sixteen.",
		"“Foolish worm, I’m two hundred years old.”",
		"A woman in her late forties answered the door.",
		"She was twenty-two and already tired of it.",
	} {
		if !AgePhrase(s) {
			t.Errorf("AgePhrase(%q) = false", s)
		}
	}
	for _, s := range []string{
		"He was ten feet from the door.",
		"She was three years into her apprenticeship.",
		"They were two of the best.",
		"The door was open.",
	} {
		if AgePhrase(s) {
			t.Errorf("AgePhrase(%q) = true", s)
		}
	}
}

func TestTimePhrase(t *testing.T) {
	for _, s := range []string{
		"Over the last few years, his brain had gradually adjusted.",
		"Three years later, the walls were finished.",
		"It had been two years since the siege.",
		"“Will, it’s been five years.”",
	} {
		if !TimePhrase(s) {
			t.Errorf("TimePhrase(%q) = false", s)
		}
	}
	if TimePhrase("He waited three minutes and left.") {
		t.Error("minutes are not a gap")
	}
}
