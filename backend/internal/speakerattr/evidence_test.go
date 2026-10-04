package speakerattr

import (
	"reflect"
	"strings"
	"testing"
)

func TestTraitStatements(t *testing.T) {
	paragraphs := []string{
		"The door banged. Bert was a broad lad of nineteen, all shoulders. He grinned.",
		"Albert was a clerk in the counting house.",
		"Bert was walking to the docks.",
		"Tallie, a towering woman, laughed at Bert's voice.",
	}
	got := TraitStatements(paragraphs, []string{"Bert"}, 10)
	want := []string{
		"Bert was a broad lad of nineteen, all shoulders.",
		"Tallie, a towering woman, laughed at Bert's voice.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TraitStatements = %q, want %q", got, want)
	}
	if phrases := TraitPhrases([]string{"Bert", "Al"}); len(phrases) != len(traitSuffixes) || !strings.HasPrefix(phrases[0], "Bert") {
		t.Fatalf("TraitPhrases kept a two-letter alias: %q", phrases)
	}
}
