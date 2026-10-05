package pronounce

import "testing"

func TestFormattingSubstitutions(t *testing.T) {
	cases := []struct{ text, want string }{
		{"• Strength: 5", "Strength: 5"},
		{"•Strength: 5", "Strength: 5"},
		{"◦ A nested item", "A nested item"},
		{"■ Quest complete ■", "Quest complete"},
		{"HP 10 • MP 5 • SP 3", "HP 10, MP 5, SP 3"},
		{"Name | Level | Class", "Name, Level, Class"},
		{"===== Status =====", "Status"},
		{"He stopped. * * * Later, he woke.", "He stopped. Later, he woke."},
		{"A note.† See below.", "A note. See below."},
		{"★★★ rating", "rating"},
		// Loose marks glued to a word are part of it.
		{"He wrote C# and snake_case.", "He wrote C# and snake_case."},
		{"So 2 = 2, then.", "So 2 = 2, then."},
		{"Item #5 of them.", "Item #5 of them."},
		{"Nothing to strip here.", "Nothing to strip here."},
		// Nothing but formatting is left alone.
		{"• • •", "• • •"},
	}
	for _, tc := range cases {
		if got := Apply(tc.text, nil, FormattingSubstitutions(tc.text)); got != tc.want {
			t.Errorf("%q -> %q, want %q", tc.text, got, tc.want)
		}
	}
}
