package speakerattr

import "testing"

func TestCastingAccent(t *testing.T) {
	for in, want := range map[string]string{
		"Speak as a young man. Adopt a General American accent.":                    "General American",
		"Speak as an old dwarf with a thick Scottish accent and a gravelly voice.":  "Scottish",
		"Speak as a goblin. Adopt a guttural Cockney accent.":                       "Cockney",
		"Speak as an elf in a plummy aristocratic Received Pronunciation accent.":   "Received Pronunciation",
		"Speak as an elf, in refined Received Pronunciation.":                       "Received Pronunciation",
		"Speak as a lord. Use an aristocratic RP accent.":                           "Received Pronunciation",
		"Speak as a woman. Use a standard British accent.":                          "British",
		"Speak as a farmer with a broad rural accent.":                              "West Country",
		"Speak as a trader with a soft Drathian accent.":                            "Drathian",
		"Speak as a calm narrator, measured and warm.":                              "",
		"Speak as a guard. His accent is hard to place, a rough, clipped delivery.": "",
	} {
		if got := CastingAccent(in); got != want {
			t.Errorf("CastingAccent(%q) = %q, want %q", in, got, want)
		}
	}
}
