package speakerattr

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/emotions"
)

func TestParseEmotionLines(t *testing.T) {
	dialogue := map[int]bool{3: true, 4: true, 7: true, 9: true}
	content := strings.Join([]string{
		"3: angry",
		"4: Whisper.",         // case and trailing punctuation tolerated
		"5: sad",              // narration line - dropped
		"7: elated",           // not a known emotion - dropped
		"9: shout (he yells)", // trailing commentary - first word kept
		"12: warm",            // not in this batch - dropped
		"garbage line",
	}, "\n")
	got := parseEmotionLines(content, dialogue)
	want := map[int]string{3: "angry", 4: "whisper", 9: "shout"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseEmotionLines = %v, want %v", got, want)
	}
	if got := parseEmotionLines("", dialogue); len(got) != 0 {
		t.Fatalf("empty reply should mean all neutral, got %v", got)
	}
}

// TestEmotionSystemPromptListsEveryEmotion guards the prompt against
// drifting from internal/emotions - a label the model is never offered can
// never be produced, and one it's offered but the app doesn't know is
// silently dropped.
func TestEmotionSystemPromptListsEveryEmotion(t *testing.T) {
	for _, e := range emotions.All {
		if !strings.Contains(emotionSystemPrompt, "- "+e.ID+": ") {
			t.Errorf("emotionSystemPrompt is missing %q", e.ID)
		}
	}
}
