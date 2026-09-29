package speakerattr

import (
	"context"
	"reflect"
	"testing"
)

func TestSceneStarts(t *testing.T) {
	window := []ParagraphInput{{Idx: 10}, {Idx: 11}, {Idx: 12, Inline: true}, {Idx: 13}, {Idx: 14}}
	got := sceneStarts([]rawScene{{Start: 13, Location: "b"}, {Start: 12}, {Start: 11, Location: "a"}, {Start: 99}}, window)
	// 11 becomes the window's first paragraph, 12 is mid-paragraph, 99 is
	// outside the window.
	if len(got) != 2 || got[0].Start != 10 || got[0].Location != "a" || got[1].Start != 13 {
		t.Fatalf("sceneStarts = %+v", got)
	}
	if got := sceneStarts(nil, window); len(got) != 1 || got[0].Start != 10 {
		t.Fatalf("no scenes: %+v", got)
	}
}

func TestParseSummary(t *testing.T) {
	raw, err := parseSummary("```json\n{\"summary\": \"Bert and Sid argue.\", \"scenes\": [{\"start\": 0, \"location\": \"the inn\", \"time\": \"night\", \"present\": [\"Bert\"]}], \"cast\": [{\"name\": \"Bert\", \"role\": false, \"pronoun\": \"he\", \"refs\": [\"the old man\"]}]}\n```")
	if err != nil || raw.Summary != "Bert and Sid argue." || len(raw.Scenes) != 1 || raw.Cast[0].Refs[0] != "the old man" {
		t.Fatalf("parseSummary = %+v, %v", raw, err)
	}
	if _, err := parseSummary(`{"summary": ""}`); err == nil {
		t.Fatal("an empty summary should fail (and so retry)")
	}
}

func TestWithCastAndRefs(t *testing.T) {
	cast := []CastMember{{Name: "Mocar", Pronouns: PronounsHe, Refs: []string{"the old sailor"}}, {Name: "Guard", Role: true}}
	r := Roster{Names: []string{"Bert"}}.WithCast(cast)
	if !reflect.DeepEqual(r.Prominent, []string{"Mocar", "Guard"}) || !r.Roles["Guard"] || r.Pronouns["Mocar"] != PronounsHe || r.Descriptions["Mocar"] != "in this chapter called the old sailor" {
		t.Fatalf("WithCast = %+v", r)
	}
	got := ResolveCastRefs(map[int]string{1: "The old sailor", 2: "old sailor", 3: "Bert"}, cast)
	if got[1] != "Mocar" || got[2] != "Mocar" || got[3] != "Bert" {
		t.Fatalf("ResolveCastRefs = %v", got)
	}
}

func TestSceneAt(t *testing.T) {
	s := &ChapterSummary{Scenes: []Scene{{StartIdx: 0, Location: "a"}, {StartIdx: 20, Location: "b"}}}
	if s.SceneAt(5).Location != "a" || s.SceneAt(20).Location != "b" || s.SceneAt(99).Location != "b" {
		t.Fatal("SceneAt")
	}
	var none *ChapterSummary
	if none.SceneAt(3) != nil {
		t.Fatal("nil summary")
	}
}

func TestMergeRefsDropsPossessives(t *testing.T) {
	got := mergeRefs(nil, []string{"his boots", "the old sailor", "Her scarf", "he", "The old sailor", "Rosie said"})
	if !reflect.DeepEqual(got, []string{"the old sailor"}) {
		t.Fatalf("mergeRefs = %v", got)
	}
}

func TestResolveCastRefsDropsSharedRefs(t *testing.T) {
	cast := []CastMember{{Name: "Fritz", Refs: []string{"the nobleman"}}, {Name: "Count", Refs: []string{"the nobleman", "the old merfolk"}}}
	got := ResolveCastRefs(map[int]string{1: "the nobleman", 2: "the old merfolk"}, cast)
	if got[1] != "the nobleman" || got[2] != "Count" {
		t.Fatalf("ResolveCastRefs = %v", got)
	}
}

// summaryOnlyLLM returns a fixed summary reply.
type summaryOnlyLLM string

func (l summaryOnlyLLM) LLMGenerate(context.Context, string, string, float32, int) (string, error) {
	return string(l), nil
}

// A speaker the text tags is added to the cast even when the model left
// them out (it once returned no cast for a chapter set in a mind-space).
func TestSummarizeChapterAddsTaggedSpeakers(t *testing.T) {
	paras := []ParagraphInput{
		{Idx: 0, Text: "Chapter 50"},
		{Idx: 1, Text: "“So,”", IsQuote: true},
		{Idx: 2, Text: "Fritz started as he paced.", Inline: true},
		{Idx: 3, Text: "“Really?!”", IsQuote: true},
		{Idx: 4, Text: "Bert exclaimed.", Inline: true},
	}
	roster := Roster{Names: []string{"Fritz", "Bert", "Sid"}, Pronouns: map[string]string{"Bert": PronounsHe}}
	llm := summaryOnlyLLM(`{"summary": "They talk in the Sanctum.", "scenes": [{"start": 0, "location": "the Sanctum", "time": "", "present": []}], "cast": []}`)
	got, err := NewClient(Config{}, llm).SummarizeChapter(context.Background(), "B", "C", nil, roster, paras)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range got.Cast {
		names = append(names, m.Name)
	}
	if !reflect.DeepEqual(names, []string{"Fritz", "Bert"}) || got.Cast[1].Pronouns != PronounsHe {
		t.Fatalf("cast = %+v, want the tagged Fritz and Bert", got.Cast)
	}
}
