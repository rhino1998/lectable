package store

import "testing"

func TestParagraphUnvoiced(t *testing.T) {
	for text, want := range map[string]bool{
		"…":           true,
		"“…”":         true,
		"“...”":       true,
		"\"...\"":     true,
		"……":          true,
		". . .":       true,
		"..":          true,
		".":           false,
		"":            false,
		"“”":          false,
		"?":           false,
		"“…?”":        false,
		"“Well…”":     false,
		"… 3":         false,
		"????: ?????": false,
	} {
		if got := (Paragraph{Text: text}).Unvoiced(); got != want {
			t.Errorf("Unvoiced(%q) = %v, want %v", text, got, want)
		}
	}
}
