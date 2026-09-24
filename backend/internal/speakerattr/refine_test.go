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
