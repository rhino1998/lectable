package speakerattr

import (
	"strings"
	"testing"
)

// para builds one source paragraph's segments from a compact spec: each
// part starting with “ is a quote, anything else narration.
func para(idx int, parts ...string) []ParagraphInput {
	out := make([]ParagraphInput, len(parts))
	for i, p := range parts {
		out[i] = ParagraphInput{Idx: idx + i, Text: p, Inline: i > 0, IsQuote: strings.HasPrefix(p, "“")}
	}
	return out
}

var testCast = NewSpeakerNameResolver(
	[]string{"Fritz", "Bert", "Sid", "Steve", "Greg", "Naomi", "Veronica", "Lynn", "Jagged Nic", "The Duskmoth", "Someone", "Fritz and Bert"},
	map[string][]string{"Bert": {"Albert"}},
)

func TestRefineWithSpeechTags(t *testing.T) {
	cases := []struct {
		name  string
		paras []ParagraphInput
		model string // the model's answer for the quote under test
		quote int    // idx of the quote under test
		want  string
	}{
		// Trailing named tags beat the model.
		{"trailing name verb", para(0, "“I didn’t know you could do that,”", "Bert said."), "Fritz", 0, "Bert"},
		{"trailing verb name", para(0, "“Over there,”", "said Fritz, pointing."), "Bert", 0, "Fritz"},
		{"trailing alias", para(0, "“You good?”", "Albert asked."), "Fritz", 0, "Bert"},
		{"trailing single-word of full name", para(0, "“Drop it, then swim,”", "Nic said."), "Steve", 0, "Jagged Nic"},
		{"trailing tag with adverb", para(0, "“Walls and a Spire,”", "Sid whispered as she looked."), "Fritz", 0, "Sid"},
		{"junk roster name never resolves", para(0, "“Who doesn’t?”", "Someone answered from a corner."), "Steve", 0, "Steve"},
		{"joint tag names no one", para(0, "“Sid,”", "Toby and Jane said together."), "Sid", 0, "Sid"},

		// Lead-in tags.
		{"lead-in name verb", para(0, "Fritz replied wistfully,", "“If we Climb together, yes.”"), "", 1, "Fritz"},
		{"lead-in participle picks last subject", para(0, "Sid smirked and Bert grinned saying,", "“Oh, you were right Sid.”"), "Sid", 1, "Bert"},
		{"lead-in ignores name after and", para(0, "Fritz caught the spear, then Bert was on top of Steve and yelling,", "“Truce!”"), "Steve", 1, "Bert"},
		{"lead-in object is not the subject", para(0, "Bert slapped Fritz on the back and asked,", "“Which Door then?”"), "Bert", 1, "Bert"},
		{"lead-in who clause", para(0, "Fritz looked worriedly to Bert who just intoned,", "“Worry not.”"), "Fritz", 1, "Bert"},
		{"lead-in name right before verb", para(0, "Bert looked over them, but before he could speak Sid interjected,", "“How about a barricade?”"), "Bert", 1, "Sid"},
		{"lead-in list after object", para(0, "Fritz met the gazes of Veronica, Lynn and Naomi then asked,", "“Who leads?”"), "Fritz", 1, "Fritz"},
		{"lead-in pronoun after object name", para(0, "Not thinking he offered it to the Duskmoth saying", "“The fang of a blight hound.”"), "Fritz", 1, "Fritz"},

		// Continuation within one paragraph.
		{"continuation after tag", para(0, "“Maybe,”", "Sid hedged.", "“But if I get a vantage point…”"), "Fritz", 2, "Sid"},
		{"continuation blocked by other actor", para(0, "“Hi,”", "Fritz said. Bert rounded on Fritz.", "“You skulg-sucking squid!”"), "Bert", 2, "Bert"},
		{"continuation blocked by pronoun tag", para(0, "“Hello,”", "Fritz whispered.", "“Hello,”", "She replied softly."), "Sid", 2, "Sid"},
		{"possessive actor blocks continuation", para(0, "“Nice,”", "Fritz said. Greg’s voice rumbled out.", "“Whoa! My voice.”"), "Greg", 2, "Greg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			speakers := map[int]string{}
			for _, p := range tc.paras {
				if p.IsQuote {
					speakers[p.Idx] = "Unknown"
				}
			}
			speakers[tc.quote] = tc.model
			// Earlier quotes in the paragraph keep whatever the tag rule
			// assigns them; seed them with the model's answer too.
			out, _ := RefineWithSpeechTags(tc.paras, speakers, testCast)
			if got := out[tc.quote]; got != tc.want {
				t.Errorf("speaker = %q, want %q (all: %v)", got, tc.want, out)
			}
		})
	}
}

func TestRefineWithSpeechTagsContinuedSpeech(t *testing.T) {
	// Bert's speech runs over three paragraphs, the first two left
	// unclosed; the model took the untagged continuations for others.
	var paras []ParagraphInput
	paras = append(paras, para(0, "“Well,”", "Bert said.", "“I made it to the Refuge.")...)
	paras = append(paras, para(3, "“I’d forgotten how beautiful it is.")...)
	paras = append(paras, para(4, "“The passes were carved to be held.”")...)
	paras = append(paras, para(5, "“And?”")...)
	paras = append(paras, para(6, "“Nothing,”", "Sid said.", "“They just stood there.")...)
	paras = append(paras, para(9, "“Then they left.”", "Fritz said.")...)
	speakers := map[int]string{0: "Bert", 2: "Bert", 3: "Sid", 4: "Unknown", 5: "Fritz", 6: "Sid", 8: "Sid", 9: "Unknown"}
	out, _ := RefineWithSpeechTags(paras, speakers, testCast)
	want := map[int]string{
		3: "Bert",  // continues the unclosed speech
		4: "Bert",  // and again
		5: "Fritz", // the speech closed at 4 - a new line keeps the model's answer
		9: "Fritz", // its own tag beats the continuation
	}
	for idx, w := range want {
		if out[idx] != w {
			t.Errorf("speaker[%d] = %q, want %q (all: %v)", idx, out[idx], w, out)
		}
	}
}

func TestRefineWithSpeechTagsOnlyTouchesGivenLines(t *testing.T) {
	paras := para(0, "“I didn’t know you could do that,”", "Bert said.")
	out, changes := RefineWithSpeechTags(paras, map[int]string{}, testCast)
	if len(out) != 0 || len(changes) != 0 {
		t.Fatalf("a line not passed in must never be set: out=%v changes=%v", out, changes)
	}
}

func TestSanitizeSpeaker(t *testing.T) {
	cases := []struct {
		raw      string
		role     bool
		want     string
		wantRole bool
	}{
		{"Fritz and Bert", false, "Unknown", false},
		{"Bert & Sid", false, "Unknown", false},
		{"Carter, Rosie", false, "Unknown", false},
		{"Sid/Bert", false, "Unknown", false},
		{"the two clerks", false, "Unknown", false},
		{"Both of them", false, "Unknown", false},
		{"No one", false, "Unknown", false},
		{"None", false, "Unknown", false},
		{"Yes", false, "Unknown", false},
		{"his foe", false, "Unknown", false},
		{"her Captain", false, "Unknown", false},
		{"unknown", true, "Unknown", false},
		{"narrator", false, "Narrator", false},
		{"she", false, "Unknown", false},
		{"Thug [role]", false, "Thug", true},
		{"Captain Mocar (a weathered sea captain)", false, "Captain Mocar", false},
		{"the guard", true, "Guard", true},
		{"The Duskmoth", false, "The Duskmoth", false},
		{"Runs-With-Purpose", false, "Runs-With-Purpose", false},
		{"Mr. Dale", false, "Mr. Dale", false},
	}
	for _, tc := range cases {
		got, role := sanitizeSpeaker(tc.raw, tc.role)
		if got != tc.want || role != tc.wantRole {
			t.Errorf("sanitizeSpeaker(%q, %v) = %q, %v; want %q, %v", tc.raw, tc.role, got, role, tc.want, tc.wantRole)
		}
	}
}

func TestIsGroupSpeaker(t *testing.T) {
	for _, name := range []string{"Bert and Sid", "Bert & Sid", "Bert, Sid", "Bert/Sid", "the two clerks", "Several guards"} {
		if !IsGroupSpeaker(name) {
			t.Errorf("IsGroupSpeaker(%q) = false", name)
		}
	}
	for _, name := range []string{"Bert", "Unknown", "", "Jagged Nic", "No one", "Mr. Dale"} {
		if IsGroupSpeaker(name) {
			t.Errorf("IsGroupSpeaker(%q) = true", name)
		}
	}
}

func TestResolverCanonical(t *testing.T) {
	if got := testCast.Canonical("albert"); got != "Bert" {
		t.Errorf("Canonical(albert) = %q", got)
	}
	if got := testCast.Canonical("Nic"); got != "Nic" {
		t.Errorf("Canonical must only fold exact names/aliases, got %q", got)
	}
}

func TestFirstPersonTags(t *testing.T) {
	paras := []ParagraphInput{
		{Idx: 0, Text: "“It’s dead,”", IsQuote: true},
		{Idx: 1, Text: "I whispered without thinking.", Inline: true},
		{Idx: 2, Text: "Dave grinned and I said,"},
		{Idx: 3, Text: "“Yeah.”", IsQuote: true, Inline: true},
	}
	base := NewSpeakerNameResolver([]string{"Steve", "Dave"}, nil)
	if got := TaggedSpeakers(paras, base); len(got) != 0 {
		t.Fatalf("without a first-person narrator, I tags name no one: %v", got)
	}
	got, _ := RefineWithSpeechTags(paras, map[int]string{0: "Unknown", 3: "Unknown"}, base.WithFirstPerson("Steve"))
	if got[0] != "Steve" || got[3] != "Steve" {
		t.Fatalf("I tags = %v, want Steve for both", got)
	}
	// Arise ch. 8: the model gave the narrator his friend's "he snapped"
	// line. A he/she tag is never the "I"; other answers are left alone.
	he := []ParagraphInput{
		{Idx: 0, Text: "“Yeah, man, I packed them both up!”", IsQuote: true},
		{Idx: 1, Text: "he snapped, hurrying up the stairs.", Inline: true},
		{Idx: 2, Text: "“What?”", IsQuote: true},
		{Idx: 3, Text: "she asked.", Inline: true},
	}
	got, _ = RefineWithSpeechTags(he, map[int]string{0: "Steve", 2: "Marie"}, base.WithFirstPerson("Steve"))
	if got[0] != "Unknown" || got[2] != "Marie" {
		t.Fatalf("he-tagged narrator line = %v, want Unknown and Marie kept", got)
	}
	if got, _ := RefineWithSpeechTags(he, map[int]string{0: "Steve"}, base); got[0] != "Steve" {
		t.Fatalf("third-person book: %v, want the answer kept", got)
	}
	if firstPersonAnswer("Protagonist", "Steve") != "Steve" || firstPersonAnswer("Protagonist", "") != "Protagonist" || firstPersonAnswer("Dave", "Steve") != "Dave" {
		t.Fatal("firstPersonAnswer")
	}
}
