package speakerattr

import (
	"strings"
	"testing"
)

func TestNoUnknownSystemPromptOffersNoUnknown(t *testing.T) {
	if n := strings.Count(noUnknownSystemPrompt, "Unknown"); n != 1 {
		t.Fatalf("noUnknownSystemPrompt mentions Unknown %d times, want 1 (the rule never to answer it):\n%s", n, noUnknownSystemPrompt)
	}
	if !strings.Contains(noUnknownSystemPrompt, `Never answer "Unknown"`) {
		t.Fatal("noUnknownSystemPrompt lost its rule against answering Unknown")
	}
}
