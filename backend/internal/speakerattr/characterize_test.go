package speakerattr

import (
	"context"
	"strings"
	"testing"
)

// replyLLM answers each call with the next reply, recording user turns.
type replyLLM struct {
	replies []string
	users   []string
}

func (l *replyLLM) LLMGenerate(_ context.Context, _, user string, _ float32, _ int) (string, error) {
	l.users = append(l.users, user)
	r := l.replies[0]
	if len(l.replies) > 1 {
		l.replies = l.replies[1:]
	}
	return r, nil
}

// TestCharacterizeVoiceRetriesCostarCopy covers the co-star guard: a
// casting copied from an "Already cast" voice is retried, and the co-stars
// reach the user turn.
func TestCharacterizeVoiceRetriesCostarCopy(t *testing.T) {
	mizhu := "Speak as a young male swampkin. Use a higher, lighter pitch with a slightly nasal timbre to reflect his small stature and prominent ears. Adopt a General American accent."
	own := "Speak as a young man with a warm, resonant voice and a measured, thoughtful pace. Adopt a General American accent. He sounds sincere and gently reverent."
	ref := "The morning light always feels like a gift to me. I try to thank everyone who makes the day possible. It is good to be among friends who listen."
	llm := &replyLLM{replies: []string{
		`{"evidence": "none", "instruct": "` + mizhu + `", "refLine": "` + ref + `"}`,
		`{"evidence": "none", "instruct": "` + own + `", "refLine": "` + ref + `"}`,
	}}
	c := NewClient(Config{}, llm)
	instruct, _, err := c.CharacterizeVoice(context.Background(), "Sessi", false, "he/him", nil, nil, nil, nil, []CastVoice{{Name: "Mizhu", Instruct: mizhu}})
	if err != nil {
		t.Fatalf("CharacterizeVoice: %v", err)
	}
	if instruct != own {
		t.Fatalf("instruct = %q, want the retried casting", instruct)
	}
	if len(llm.users) != 2 {
		t.Fatalf("calls = %d, want 2 (one retry)", len(llm.users))
	}
	if !strings.Contains(llm.users[0], "\nAlready cast:\n- Mizhu: Speak as a young male swampkin.") {
		t.Fatalf("user turn lacks the co-star:\n%s", llm.users[0])
	}
}

func TestWordOverlap(t *testing.T) {
	if got := wordOverlap("Speak as a calm man.", "speak as a calm man"); got != 1 {
		t.Fatalf("identical overlap = %v", got)
	}
	if got := wordOverlap("a low voice", "bright quick girl"); got != 0 {
		t.Fatalf("disjoint overlap = %v", got)
	}
}

func TestOriginClues(t *testing.T) {
	paras := OriginParagraphs([]string{
		"Unfortunately, outside he bumped into the second woman, Ingrid, a Danish archeologist. She recognized the staff.",
		"Nothing about anyone's origin here, Ingrid said.",
		"Lars whispered in Danish, scribbling away.",
		"I went from Danish to Russian to Chinese before swearing in Greek.",
		"Lord Ballast spoke with a clipped accent.",
	})
	if len(paras) != 4 {
		t.Fatalf("OriginParagraphs kept %d, want 4", len(paras))
	}
	got := OriginClues(paras, []string{"Ingrid"}, 5)
	if len(got) != 1 || got[0] != "Unfortunately, outside he bumped into the second woman, Ingrid, a Danish archeologist." {
		t.Fatalf("Ingrid clues = %q", got)
	}
	if got := OriginClues(paras, []string{"Lars Machmüller"}, 5); len(got) != 1 || got[0] != "Lars whispered in Danish, scribbling away." {
		t.Fatalf("Lars clues = %q", got)
	}
	if got := OriginClues(paras, []string{"Lord Ballast"}, 5); len(got) != 1 {
		t.Fatalf("Lord Ballast clues = %q", got)
	}
	if got := OriginClues(paras, []string{"Lord Scarford"}, 5); len(got) != 0 {
		t.Fatalf("a title alone matched: %q", got)
	}
}

func TestMentionsName(t *testing.T) {
	for _, c := range []struct {
		text, name string
		want       bool
	}{
		{"distinct from the British Steve and American Jonas", "Steve", true},
		{"a voice unlike Steve Atherton's", "Steve Atherton", true},
		{"a steady, even pace", "Steve", false},
		{"low and grave", "Master Grave", false}, // lower-case "grave" never matches "Grave"
	} {
		if got := mentionsName(c.text, c.name); got != c.want {
			t.Errorf("mentionsName(%q, %q) = %v, want %v", c.text, c.name, got, c.want)
		}
	}
}
