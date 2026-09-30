package speakerattr

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
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

func TestWithCastReplacesProminent(t *testing.T) {
	r := Roster{Names: []string{"Petros", "Steve", "Dave"}, Prominent: []string{"Petros", "Steve"}}.WithCast([]CastMember{{Name: "Steve"}, {Name: "Dave"}})
	if !reflect.DeepEqual(r.Prominent, []string{"Steve", "Dave"}) {
		t.Fatalf("Prominent = %v, want only the chapter's cast", r.Prominent)
	}
}

func firstPersonChapter() []ParagraphInput {
	return []ParagraphInput{
		{Idx: 0, Text: "“Jamey...Sultan?”", IsQuote: true},
		{Idx: 1, Text: "he asked me, and I hesitated before nodding."},
		{Idx: 2, Text: "I looked at them, frowning."},
		{Idx: 3, Text: "Dave opened the door."},
		{Idx: 4, Text: "I’m not sure why I bothered."},
	}
}

func TestFirstPersonNarration(t *testing.T) {
	if !FirstPersonNarration(firstPersonChapter()) {
		t.Fatal("first-person chapter read as third person")
	}
	third := []ParagraphInput{
		{Idx: 0, Text: "“I see nothing!”", IsQuote: true},
		{Idx: 1, Text: "Petros groaned."},
		{Idx: 2, Text: "Santiago swirled the wine."},
		{Idx: 3, Text: "The doors opened."},
	}
	if FirstPersonNarration(third) {
		t.Fatal("third-person chapter (with an \"I\" only inside dialogue) read as first person")
	}
}

func TestFoldFirstPersonCast(t *testing.T) {
	roster := Roster{Names: []string{"Steve", "Dave", "Petros"}, Pronouns: map[string]string{"Steve": PronounsHe}, FirstPerson: "Steve"}
	sum := ChapterSummary{
		Scenes: []Scene{{StartIdx: 0, Present: []string{"Jamey", "Marie", "Dave"}}, {StartIdx: 3, Present: []string{"Jamey", "Dave"}}},
		Cast: []CastMember{
			{Name: "Jamey", Pronouns: PronounsHe, Refs: []string{"Jamey...Sultan"}},
			{Name: "Marie", Pronouns: PronounsShe},
			{Name: "Dave", Pronouns: PronounsHe},
			{Name: "Officer", Role: true},
		},
	}
	got, alias := FoldFirstPersonCast(sum, roster, firstPersonChapter())
	if alias != "Jamey" || got.Cast[0].Name != "Steve" || got.Cast[0].Refs[0] != "Jamey...Sultan" {
		t.Fatalf("FoldFirstPersonCast = %+v, %q", got.Cast, alias)
	}
	if !reflect.DeepEqual(got.Scenes[1].Present, []string{"Steve", "Dave"}) || sum.Scenes[1].Present[0] != "Jamey" {
		t.Fatalf("scenes = %+v (input must stay untouched: %+v)", got.Scenes, sum.Scenes)
	}

	// The narrator already in the cast: nothing to fold.
	withSteve := sum
	withSteve.Cast = append([]CastMember{{Name: "Steve"}}, sum.Cast...)
	if _, alias := FoldFirstPersonCast(withSteve, roster, firstPersonChapter()); alias != "" {
		t.Fatalf("folded %q with the narrator already cast", alias)
	}
	// A third-person chapter of a first-person book: its new names are other people.
	third := []ParagraphInput{{Idx: 0, Text: "Santiago swirled the wine."}, {Idx: 1, Text: "Petros groaned."}}
	if _, alias := FoldFirstPersonCast(sum, roster, third); alias != "" {
		t.Fatalf("folded %q in a third-person chapter", alias)
	}
	// Two unknown candidates in the same number of scenes: ambiguous.
	tie := sum
	tie.Scenes = []Scene{{StartIdx: 0, Present: []string{"Jamey", "Marie"}}}
	tie.Cast = []CastMember{{Name: "Jamey"}, {Name: "Marie"}}
	if _, alias := FoldFirstPersonCast(tie, Roster{Names: []string{"Steve"}, FirstPerson: "Steve"}, firstPersonChapter()); alias != "" {
		t.Fatalf("folded %q on a tie", alias)
	}
	// A placeholder for the "I" is always the narrator, even beside them.
	placeholder := ChapterSummary{Scenes: []Scene{{Present: []string{"Protagonist", "Dave"}}}, Cast: []CastMember{{Name: "Steve"}, {Name: "Protagonist", Refs: []string{"the new guy"}}, {Name: "Dave"}}}
	got, _ = FoldFirstPersonCast(placeholder, roster, firstPersonChapter())
	if len(got.Cast) != 2 || got.Cast[0].Name != "Steve" || len(got.Cast[0].Refs) != 1 || got.Scenes[0].Present[0] != "Steve" {
		t.Fatalf("placeholder fold = %+v", got)
	}
	// Pronouns that contradict the narrator's rule the candidate out.
	if _, alias := FoldFirstPersonCast(ChapterSummary{Cast: []CastMember{{Name: "Marie", Pronouns: PronounsShe}}}, roster, firstPersonChapter()); alias != "" {
		t.Fatalf("folded %q despite she/her vs he/him", alias)
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
	got := mergeRefs(nil, []string{"his boots", "the old sailor", "Her scarf", "he", "The old sailor", "Rosie said", "dude", "man"})
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

func TestScenePresent(t *testing.T) {
	r := Roster{
		Names:       []string{"Steve", "Dave", "Kev", "Marie", "Boss"},
		Prominent:   []string{"Kev", "Dave", "Marie", "Boss", "Steve"},
		FirstPerson: "Steve",
		Scenes: []Scene{
			{StartIdx: 5, Present: []string{"Steve", "Kev"}},
			{StartIdx: 20, Present: []string{"Steve", "Dave", "Marie"}},
		},
	}
	lines := func(idx ...int) []ParagraphInput {
		var out []ParagraphInput
		for _, i := range idx {
			out = append(out, ParagraphInput{Idx: i, Text: "“Go.”", IsQuote: true})
		}
		return out
	}
	// Boss is in no scene (a phone voice), so always listed; Steve narrates.
	if got := r.scenePresent(lines(21, 30)); !reflect.DeepEqual(got, []string{"Dave", "Marie", "Boss", "Steve"}) {
		t.Fatalf("second scene = %v", got)
	}
	if got := r.scenePresent(lines(0, 8)); !reflect.DeepEqual(got, []string{"Kev", "Boss", "Steve"}) {
		t.Fatalf("first scene (and before it) = %v", got)
	}
	if got := r.scenePresent(lines(18, 22)); !reflect.DeepEqual(got, r.Prominent) {
		t.Fatalf("spanning both = %v", got)
	}
	// Named in the text: listed even outside their scene.
	if got := relevantNames(r, []ParagraphInput{{Idx: 25, Text: "Kev groaned on the floor."}}); !slices.Contains(got, "Kev") {
		t.Fatalf("relevantNames = %v, want Kev (mentioned)", got)
	}
	if got := (Roster{Prominent: []string{"A"}}).scenePresent(lines(1)); !reflect.DeepEqual(got, []string{"A"}) {
		t.Fatalf("no scenes = %v", got)
	}
}

func TestSceneStarting(t *testing.T) {
	r := Roster{Scenes: []Scene{{StartIdx: 0, Location: "the flat", Present: []string{"Steve", "Kev"}}, {StartIdx: 206, Present: []string{"Steve", "Dave"}}}}
	if sc := r.sceneStarting(-1, 190); sc == nil || sc.StartIdx != 0 {
		t.Fatalf("batch start = %+v, want the scene it falls in", sc)
	}
	if sc := r.sceneStarting(190, 200); sc != nil {
		t.Fatalf("mid-scene = %+v", sc)
	}
	if sc := r.sceneStarting(205, 207); sc == nil || sc.StartIdx != 206 {
		t.Fatalf("crossing 206 = %+v", sc)
	}
	if got := sceneMarker(&r.Scenes[0]); got != "the flat; present: Steve, Kev" {
		t.Fatalf("sceneMarker = %q", got)
	}
	if (Roster{}).sceneStarting(-1, 3) != nil {
		t.Fatal("no scenes")
	}
}

func TestCarryOver(t *testing.T) {
	if carryOver(ChapterSummary{}) != "" {
		t.Fatal("first window gets no carry-over")
	}
	got := carryOver(ChapterSummary{Summary: "Steve fights wolves.", Scenes: []Scene{{Location: "a taverna", Present: []string{"Steve", "Ingrid"}}}})
	for _, want := range []string{"so far: Steve fights wolves.", "the scene in progress: a taverna", "present when it broke off: Steve, Ingrid"} {
		if !strings.Contains(got, want) {
			t.Fatalf("carryOver = %q, missing %q", got, want)
		}
	}
}

func TestLeadIn(t *testing.T) {
	if leadIn(nil) != "" {
		t.Fatal("first window gets no lead-in")
	}
	var prev []ParagraphInput
	for i := 0; i < 12; i++ {
		prev = append(prev, ParagraphInput{Idx: i, Text: fmt.Sprintf("p%d", i)})
	}
	prev[4].Inline = true // the cut would land mid-paragraph: back up to 3
	got := leadIn(prev)
	if !strings.Contains(got, "> p3 p4") || strings.Contains(got, "p2") || !strings.Contains(got, "> p11") {
		t.Fatalf("leadIn = %q", got)
	}
}

func TestAddTaggedToScenes(t *testing.T) {
	paras := []ParagraphInput{
		{Idx: 0, Text: "“Go,”", IsQuote: true}, {Idx: 1, Text: "Dave said.", Inline: true},
		{Idx: 2, Text: "“Where?”", IsQuote: true}, {Idx: 3, Text: "Ingrid asked.", Inline: true},
	}
	sum := ChapterSummary{Scenes: []Scene{{StartIdx: 0, Present: []string{"Steve", "Dave"}}, {StartIdx: 2, Present: []string{"Steve", "Barmaid"}}}}
	addTaggedToScenes(&sum, paras, NewSpeakerNameResolver([]string{"Steve", "Dave", "Ingrid"}, nil))
	if !reflect.DeepEqual(sum.Scenes[0].Present, []string{"Steve", "Dave"}) || !reflect.DeepEqual(sum.Scenes[1].Present, []string{"Steve", "Barmaid", "Ingrid"}) {
		t.Fatalf("scenes = %+v", sum.Scenes)
	}
}

func TestResolveCastRefsIgnoresRoleNameRefs(t *testing.T) {
	cast := []CastMember{{Name: "Werewolf", Role: true}, {Name: "Johnny", Refs: []string{"the werewolf", "the pack leader"}}}
	got := ResolveCastRefs(map[int]string{1: "Werewolf", 2: "the pack leader"}, cast)
	if got[1] != "Werewolf" || got[2] != "Johnny" {
		t.Fatalf("ResolveCastRefs = %v", got)
	}
}
