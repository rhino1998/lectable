package httpapi

import (
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
)

func TestSplitInvalidCharacters(t *testing.T) {
	valid, invalid := splitInvalidCharacters([]store.Character{
		{Name: "Gandalf"},
		{Name: "he", Invalid: true},
		{Name: "Guard", Invalid: true},
		{Name: "guard"},
	})
	if len(valid) != 2 || valid[0].Name != "Gandalf" || valid[1].Name != "guard" {
		t.Fatalf("valid = %+v; want Gandalf, guard", valid)
	}
	for name, want := range map[string]bool{
		"he":      true,
		"He":      true, // case-folded
		"Guard":   true,
		"guard":   false, // a valid character's exact name wins
		"Gandalf": false,
		"Frodo":   false,
	} {
		if got := invalid.has(name); got != want {
			t.Errorf("has(%q) = %v; want %v", name, got, want)
		}
	}
}
