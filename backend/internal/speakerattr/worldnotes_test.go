package speakerattr

import (
	"slices"
	"testing"
)

func TestMatchWorldNotes(t *testing.T) {
	notes := []WorldNote{
		{Key: "kobold", Fact: "A short-lived kind."},
		{Key: "dwarf", Aliases: []string{"dwarven", "dwarves"}, Fact: "Scottish accent."},
		{Key: "elf", Fact: "Refined RP."},
		{Key: "orc", Fact: " "},
	}
	got := MatchWorldNotes(notes, "Loth the Luminary", "The Kobolds of the tower", "a dwarven smith by himself", "an orc")
	// "himself" holds "elf" but not as a word; a note with no fact is skipped.
	want := []string{"kobold: A short-lived kind.", "dwarf: Scottish accent."}
	if !slices.Equal(got, want) {
		t.Fatalf("MatchWorldNotes = %q, want %q", got, want)
	}
	if got := MatchWorldNotes(notes, "a human sailor"); got != nil {
		t.Fatalf("no match: got %q", got)
	}
}
