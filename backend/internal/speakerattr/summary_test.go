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

// emptySummaryLLM answers every call with an empty summary.
type emptySummaryLLM struct{}

func (emptySummaryLLM) LLMGenerate(context.Context, string, string, float32, int) (string, error) {
	return `{"summary": "", "scenes": [], "cast": []}`, nil
}

// A chapter with nothing to summarize (Black Ops' one-line "Converted
// Ebook" page) gets empty notes when the model says as much; a chapter
// with real text still fails on an empty answer.
func TestSummarizeChapterNothingToSay(t *testing.T) {
	c := NewClient(Config{}, emptySummaryLLM{})
	sum, err := c.SummarizeChapter(context.Background(), "Black Ops", "Converted Ebook", nil, Roster{}, []ParagraphInput{{Idx: 0, Text: "Converted Ebook"}})
	if err != nil || sum.Summary != "" || len(sum.Scenes) != 0 || len(sum.Cast) != 0 {
		t.Fatalf("one-line chapter = %+v, %v", sum, err)
	}
	long := []ParagraphInput{{Idx: 0, Text: strings.Repeat("Bert and Sid argue. ", 20)}}
	if _, err := c.SummarizeChapter(context.Background(), "Black Ops", "One", nil, Roster{}, long); err == nil {
		t.Fatal("an empty summary of a real chapter should fail")
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

// A first-person chapter summarized as someone else's (Reclaimer ch. 14's
// Dan, named once in a memory) is asked again, and the retry is kept only
// when it has the narrator.
func TestSummarizeChapterReasksWhenNarratorMissing(t *testing.T) {
	paras := []ParagraphInput{
		{Idx: 0, Text: "I crept down the corridor, rifle raised."},
		{Idx: 1, Text: "I remembered the device Dan had given me back at the police station."},
		{Idx: 2, Text: "I wired it into the panel and waited."},
		{Idx: 3, Text: "The door slid open, and I went through."},
	}
	roster := Roster{Names: []string{"Steve", "Dan"}, FirstPerson: "Steve"}
	standIn := `{"summary": "Dan breaks into the security station.", "scenes": [{"start": 0, "location": "corridor", "time": "", "present": ["Dan"]}], "cast": [{"name": "Dan", "role": false, "pronoun": "he", "refs": []}]}`
	fixed := `{"summary": "Steve breaks into the security station.", "scenes": [{"start": 0, "location": "corridor", "time": "", "present": ["Steve"]}], "cast": [{"name": "Steve", "role": false, "pronoun": "he", "refs": []}]}`

	llm := &replyLLM{replies: []string{standIn, fixed}}
	got, err := NewClient(Config{}, llm).SummarizeChapter(context.Background(), "B", "C", nil, roster, paras)
	if err != nil {
		t.Fatal(err)
	}
	if len(llm.users) != 2 {
		t.Fatalf("%d calls, want the summary and one re-ask", len(llm.users))
	}
	if note := llm.users[1]; !strings.Contains(note, "left Steve out") || !strings.Contains(note, "made Dan, whom these paragraphs name in only 1 of them") {
		t.Fatalf("re-ask doesn't explain the miss:\n%s", note)
	}
	if got.Summary != "Steve breaks into the security station." || len(got.Cast) != 1 || got.Cast[0].Name != "Steve" || !reflect.DeepEqual(got.Scenes[0].Present, []string{"Steve"}) {
		t.Fatalf("got %+v, want the retry's notes", got)
	}

	// A retry that still leaves the narrator out is dropped.
	llm = &replyLLM{replies: []string{standIn, standIn}}
	got, _ = NewClient(Config{}, llm).SummarizeChapter(context.Background(), "B", "C", nil, roster, paras)
	if len(llm.users) != 2 || got.Summary != "Dan breaks into the security station." {
		t.Fatalf("calls %d, summary %q: want the first answer kept after a failed re-ask", len(llm.users), got.Summary)
	}

	// Narrator present (or a placeholder for them): no re-ask.
	for _, name := range []string{"Steve", "Protagonist"} {
		llm = &replyLLM{replies: []string{strings.ReplaceAll(fixed, `"name": "Steve"`, `"name": "`+name+`"`)}}
		if _, err := NewClient(Config{}, llm).SummarizeChapter(context.Background(), "B", "C", nil, roster, paras); err != nil || len(llm.users) != 1 {
			t.Fatalf("cast %s: %d calls (err %v), want 1", name, len(llm.users), err)
		}
	}

	// Third-person narration (a prologue elsewhere in time): no re-ask.
	third := []ParagraphInput{
		{Idx: 0, Text: "Dan crept down the corridor."},
		{Idx: 1, Text: "He wired the device into the panel."},
		{Idx: 2, Text: "The door slid open."},
	}
	llm = &replyLLM{replies: []string{standIn}}
	if _, err := NewClient(Config{}, llm).SummarizeChapter(context.Background(), "B", "C", nil, roster, third); err != nil || len(llm.users) != 1 {
		t.Fatalf("third person: %d calls (err %v), want 1", len(llm.users), err)
	}
}

// A chapter's notes pronouns win over the roster's for a named character
// (a book can change one mid-way), under the roster's own spelling; a role's
// only fill a gap.
func TestWithCastChapterPronouns(t *testing.T) {
	r := Roster{
		Names:    []string{"Sid", "Francine Amanda Deena", "Guard"},
		Aliases:  map[string][]string{"Francine Amanda Deena": {"Amanda"}},
		Pronouns: map[string]string{"Sid": PronounsShe, "Francine Amanda Deena": PronounsShe, "Guard": PronounsHe},
	}
	got := r.WithCast([]CastMember{
		{Name: "Sid", Pronouns: PronounsHe},
		{Name: "Amanda", Pronouns: PronounsShe},
		{Name: "Guard", Role: true, Pronouns: PronounsShe},
		{Name: "Bosun", Role: true, Pronouns: PronounsHe},
	}).Pronouns
	want := map[string]string{"Sid": PronounsHe, "Francine Amanda Deena": PronounsShe, "Guard": PronounsHe, "Bosun": PronounsHe}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pronouns = %v, want %v", got, want)
	}
	if r.Pronouns["Sid"] != PronounsShe {
		t.Fatal("WithCast changed the roster it was called on")
	}
}

func TestWithCastDropsRefsNamingSomeoneElse(t *testing.T) {
	r := Roster{
		Names:   []string{"Bob", "Seneschal", "Oracle"},
		Aliases: map[string][]string{"Oracle": {"Hellenica"}},
	}.WithCast([]CastMember{
		{Name: "Bob", Refs: []string{"Seneschal", "bone minion"}},
		{Name: "Oracle", Refs: []string{"Hellenica"}},
	})
	if got := r.Cast[0].Refs; !reflect.DeepEqual(got, []string{"bone minion"}) {
		t.Fatalf("Bob's refs = %v", got)
	}
	if got := r.Cast[1].Refs; !reflect.DeepEqual(got, []string{"Hellenica"}) {
		t.Fatalf("Oracle's refs = %v", got)
	}
	if d := r.Descriptions["Bob"]; strings.Contains(d, "Seneschal") {
		t.Fatalf("Bob's description = %q", d)
	}
	got := ResolveCastRefs(map[int]string{1: "Seneschal", 2: "the bone minion"}, r.Cast)
	if got[1] != "Seneschal" || got[2] != "Bob" {
		t.Fatalf("ResolveCastRefs = %v", got)
	}
}
