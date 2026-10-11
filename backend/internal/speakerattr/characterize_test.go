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
	instruct, _, err := c.CharacterizeVoice(context.Background(), "Sessi", false, "he/him", nil, CharacterEvidence{}, []CastVoice{{Name: "Mizhu", Instruct: mizhu}}, CastingOptions{})
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

// A model that keeps copying whichever co-stars it is shown still ends up
// with a casting: each retry hides the co-star it copied, and the last
// attempt shows none, so nothing it writes there can count as a copy.
func TestCharacterizeVoiceHidesCopiedCostars(t *testing.T) {
	bob := "Speak as a middle-aged dwarf with a Scottish accent. Use a lower pitch with a rough, gravelly timbre to convey a sturdy, earthy presence."
	oren := "Speak as an elderly dwarf man. Use a deep, resonant pitch with a broad Scottish accent. Keep the pace moderate and rhythmic, warm and paternal."
	ref := "The morning light always feels like a gift to me. I try to thank everyone who makes the day possible. It is good to be among friends who listen."
	reply := func(instruct string) string {
		return `{"evidence": "none", "instruct": "` + instruct + `", "refLine": "` + ref + `"}`
	}
	llm := &replyLLM{replies: []string{reply(bob), reply(oren), reply(bob)}}
	c := NewClient(Config{}, llm)
	costars := []CastVoice{{Name: "Bob", Instruct: bob}, {Name: "Oren", Instruct: oren}}
	instruct, _, err := c.CharacterizeVoice(context.Background(), "Dwarf", true, "he/him", nil, CharacterEvidence{}, costars, CastingOptions{})
	if err != nil {
		t.Fatalf("CharacterizeVoice: %v", err)
	}
	if instruct != bob {
		t.Fatalf("instruct = %q, want the last attempt's casting", instruct)
	}
	if len(llm.users) != 3 {
		t.Fatalf("calls = %d, want 3", len(llm.users))
	}
	if !strings.Contains(llm.users[0], "- Bob: ") || !strings.Contains(llm.users[0], "- Oren: ") {
		t.Fatalf("first attempt lacks a co-star:\n%s", llm.users[0])
	}
	if strings.Contains(llm.users[1], "- Bob: ") || !strings.Contains(llm.users[1], "- Oren: ") {
		t.Fatalf("second attempt should show Oren only:\n%s", llm.users[1])
	}
	if strings.Contains(llm.users[2], "Already cast") {
		t.Fatalf("last attempt still shows co-stars:\n%s", llm.users[2])
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
	got := OriginClues(paras, []string{"Ingrid"}, nil, 5)
	if len(got) != 1 || got[0] != "Unfortunately, outside he bumped into the second woman, Ingrid, a Danish archeologist." {
		t.Fatalf("Ingrid clues = %q", got)
	}
	if got := OriginClues(paras, []string{"Lars Machmüller"}, nil, 5); len(got) != 1 || got[0] != "Lars whispered in Danish, scribbling away." {
		t.Fatalf("Lars clues = %q", got)
	}
	if got := OriginClues(paras, []string{"Lord Ballast"}, nil, 5); len(got) != 1 {
		t.Fatalf("Lord Ballast clues = %q", got)
	}
	// A nationality in the next sentence is someone else's.
	next := OriginParagraphs([]string{"West stood in the middle, grinning widely. The huge Nigerian clapped him on the back."})
	if got := OriginClues(next, []string{"West"}, nil, 5); len(got) != 0 {
		t.Fatalf("a clue crossed a sentence end: %q", got)
	}
	if got := OriginClues(paras, []string{"Lord Scarford"}, nil, 5); len(got) != 0 {
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
		{"a young man with a General American accent", "Major General Franklin Hoenikker", false},
		{"deeper than General Hoenikker", "Major General Franklin Hoenikker", true},
	} {
		if got := mentionsName(c.text, c.name); got != c.want {
			t.Errorf("mentionsName(%q, %q) = %v, want %v", c.text, c.name, got, c.want)
		}
	}
}

// TestCharacterizeVoiceLookups covers the lookup loop: a {"lookup": ...}
// reply is searched and its results reach the next turn, and the reply
// after that is the casting.
func TestCharacterizeVoiceLookups(t *testing.T) {
	own := "Speak as an ancient woman with a low, smooth voice and a measured, deliberate pace. Adopt a Greek accent speaking English. She sounds blunt and commanding."
	ref := "I have watched many harbors fill and empty. The sea keeps what it is given. You learn to wait for the tide."
	llm := &replyLLM{replies: []string{
		`{"lookup": [["Scylla", "Greek"], ["", "  "], ["Scylla", "voice", "accent", "extra"]]}`,
		`{"evidence": "'a Greek demigoddess'", "instruct": "` + own + `", "refLine": "` + ref + `"}`,
	}}
	c := &Client{llm: llm}
	var asked [][]string
	opts := CastingOptions{
		Lookup: func(_ context.Context, terms []string) ([]string, error) {
			asked = append(asked, terms)
			return []string{"She's a Greek demigoddess or whatever!"}, nil
		},
	}
	instruct, _, err := c.CharacterizeVoice(context.Background(), "Scylla", false, "she/her", nil, CharacterEvidence{}, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if instruct != own {
		t.Fatalf("instruct = %q", instruct)
	}
	if len(asked) != 2 || strings.Join(asked[0], "+") != "Scylla+Greek" || len(asked[1]) != 3 {
		t.Fatalf("lookups asked = %q", asked)
	}
	if len(llm.users) != 2 || !strings.Contains(llm.users[1], "Scylla + Greek:\n- She's a Greek demigoddess or whatever!") {
		t.Fatalf("second turn lacks the lookup results:\n%s", llm.users[len(llm.users)-1])
	}
}
