package speakerattr

import (
	"strings"
	"testing"
)

func TestAttributionPromptExtras(t *testing.T) {
	lines := []ParagraphInput{
		{Idx: 47, IsQuote: true, Text: "“Bert?”", Known: "Fritz", Seeded: true},
		{Idx: 49, IsQuote: true, Text: "“Yes?”"},
		{Idx: 51, IsQuote: true, Text: "“When we asked you,”", Known: "Fritz"},
	}
	answer := map[int]bool{49: true}
	roster := Roster{Names: []string{"Fritz", "Bert"}, prevNext: true, taggedSeeds: true}
	got := (&Client{}).attributionUserPrompt("B", "C", roster, lines, answer)
	for _, want := range []string{
		"47 (tagged: Fritz): “Bert?”",
		"49 (who?): “Yes?”\n    prev: Fritz (47) · next: Fritz (51)\n",
		"51 (Fritz): “When we asked you,”",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	plain := (&Client{}).attributionUserPrompt("B", "C", Roster{Names: roster.Names}, lines, answer)
	if strings.Contains(plain, "prev:") || strings.Contains(plain, "tagged:") {
		t.Errorf("default prompt carries an extra:\n%s", plain)
	}
	sys := roster.attributionSystemPrompt()
	if !strings.HasPrefix(sys, systemPrompt) || !strings.Contains(sys, "prev: Mira") || !strings.Contains(sys, "tagged: Mira") {
		t.Error("system prompt lacks an extra's rule")
	}
	if (Roster{}).attributionSystemPrompt() != systemPrompt {
		t.Error("default system prompt changed")
	}
}
