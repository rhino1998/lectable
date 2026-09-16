package speakerattr

import (
	"testing"

	"github.com/rhino1998/lectable/backend/internal/deliverytags"
)

func hasTag(insertions []deliverytags.Insertion, tag string) bool {
	for _, ins := range insertions {
		if ins.Tag == tag {
			return true
		}
	}
	return false
}

func TestAddHeuristicSentenceTagsAllCapsShouting(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"single caps word with bang", `"STOP!" she shouted.`, true},
		{"contraction with bang", `"DON'T!" he yelled.`, true},
		{"multi-word caps phrase", `"GET OUT!"`, true},
		{"short caps word with bang", `"NO!"`, true},
		{"acronym without bang - no false positive", "She works for NASA in Houston.", false},
		{"acronym with period - no false positive", "He showed his ID.", false},
		{"ordinary sentence", "It was a quiet afternoon.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := addHeuristicSentenceTags(tc.text, nil)
			if hasTag(got, "<|style:shouting|>") != tc.want {
				t.Errorf("addHeuristicSentenceTags(%q) shouting=%v, want %v (got %+v)", tc.text, !tc.want, tc.want, got)
			}
		})
	}
}

func TestAddHeuristicSentenceTagsDedupesAgainstExisting(t *testing.T) {
	existing := []deliverytags.Insertion{{Offset: 0, Tag: "<|style:shouting|>"}}
	got := addHeuristicSentenceTags(`"STOP!" she shouted.`, existing)
	count := 0
	for _, ins := range got {
		if ins.Tag == "<|style:shouting|>" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 shouting tag after dedup, got %d: %+v", count, got)
	}
}
