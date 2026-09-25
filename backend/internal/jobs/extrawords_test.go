package jobs

import "testing"

func TestExtraWordRun(t *testing.T) {
	const line = "Well, look at you. All grown up and still grinning like a fool."
	for _, tc := range []struct {
		name, expected, transcript string
		want                       int
	}{
		{"exact", line, line, 0},
		{"asr formatting only", line, "well look at you all grown-up and still grinning like a fool", 0},
		{"curly apostrophe", "It’s good to see you.", "It's good to see you.", 0},
		{"delivery tags ignored", "Wait <|pause:short|> for me.", "Wait for me.", 0},
		{"misheard word", line, "Well, look at you. All grown up and still grinding like a fool.", 0},
		{"dropped words aren't extra", line, "Well, look at you.", 0},
		{"trailing extra", line, line + " Enough! I've had it with your excuses.", 7},
		{"leading extra", line, "Get out of my sight. " + line, 5},
		{"repeated phrase", line, "Well, look at you. Look at you. All grown up and still grinning like a fool.", 3},
		{"scattered one-word slips", line, "Oh well, look at you. All grown up and uh still grinning like a fool.", 1},
		{"empty transcript", line, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extraWordRun(tc.expected, tc.transcript); got != tc.want {
				t.Errorf("extraWordRun = %d, want %d", got, tc.want)
			}
		})
	}
}
