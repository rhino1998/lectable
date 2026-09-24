package deliverytags

import "testing"

func TestPauseInsertions(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"No pauses here.", "No pauses here."},
		{"Well... I suppose so.", "Well<|prosody:pause|>... I suppose so."},
		{"Well… I suppose so.", "Well<|prosody:pause|>… I suppose so."},
		{"Well. . . I suppose so.", "Well<|prosody:pause|>. . . I suppose so."},
		{"I was going to—wait, what?", "I was going to<|prosody:pause|>—wait, what?"},
		{"Two... separate... pauses.", "Two<|prosody:pause|>... separate<|prosody:pause|>... pauses."},
		// Nothing speakable after the ellipsis: no trailing tag.
		{"I don't know...", "I don't know..."},
		{"“I don't know…”", "“I don't know…”"},
		{"She never finished—", "She never finished—"},
		// Back-to-back punctuation collapses into one pause.
		{"And then—... nothing.", "And then<|prosody:pause|>—... nothing."},
		// Two periods isn't an ellipsis.
		{"Odd.. typo.", "Odd.. typo."},
	}
	for _, tc := range cases {
		got := Merge(tc.text, PauseInsertions(tc.text))
		if got != tc.want {
			t.Errorf("PauseInsertions(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}
