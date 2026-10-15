package speakerattr

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestFindIdentityEvidence(t *testing.T) {
	book := []string{
		"Fritz walked to the gate with Bert.",
		"Fritz ate breakfast.",
		"Fritz slept.",
		"Fritz trained with Bert.",
		"“Halt!” The storm guard sergeant, Louisa, asked with some surprise. Fritz blinked.",
		"Fritz answered her.",
		"Later the storm guard sergeant waved them through.",
		"Louisa smiled at Fritz.",
	}
	names := map[string][]string{"Fritz": nil, "Bert": nil, "Sergeant Louisa": {"Louisa"}}
	ev := FindIdentityEvidence(book, "storm guard sergeant", names, nil)
	if len(ev.Candidates) == 0 || ev.Candidates[0] != "Sergeant Louisa" {
		t.Fatalf("candidates = %v, want Sergeant Louisa first despite Fritz being everywhere", ev.Candidates)
	}
	if len(ev.Snippets) == 0 || !strings.Contains(ev.Snippets[0], "Louisa, asked") {
		t.Fatalf("snippets = %q", ev.Snippets)
	}
	if ev := FindIdentityEvidence(book, "the ferrywoman", names, nil); len(ev.Candidates) != 0 {
		t.Fatalf("no hits should find nothing, got %v", ev)
	}
}

// httpapi shares one index per book across concurrent summary tasks; the
// frequency cache once crashed the server with "concurrent map writes".
func TestIdentityIndexConcurrent(t *testing.T) {
	book := []string{"Fritz met the storm guard sergeant, Louisa.", "Bert waved.", "Louisa nodded."}
	ix := NewIdentityIndex(book, map[string][]string{"Fritz": nil, "Bert": nil, "Sergeant Louisa": {"Louisa"}})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if ev := ix.FindIdentityEvidence("storm guard sergeant", nil); len(ev.Candidates) == 0 {
				t.Error("no candidates")
			}
		})
	}
	wg.Wait()
}

type identityLLM struct{ reply string }

func (l identityLLM) LLMGenerate(context.Context, string, string, float32, int) (string, error) {
	return l.reply, nil
}

func TestMatchIdentityNeedsVerbatimEvidence(t *testing.T) {
	ev := IdentityEvidence{Candidates: []string{"Sergeant Louisa", "Fritz"}, Snippets: []string{"The storm guard sergeant, Louisa, asked with some surprise."}}
	for reply, want := range map[string]string{
		`{"same_as": "Sergeant Louisa", "evidence": "The storm guard sergeant, Louisa, asked"}`: "Sergeant Louisa",
		`{"same_as": "Sergeant Louisa", "evidence": "Louisa is the sergeant"}`:                  "", // not in any passage
		`{"same_as": "Bert", "evidence": "The storm guard sergeant, Louisa, asked"}`:            "", // not a candidate
		`{"same_as": "", "evidence": ""}`:                                                       "",
	} {
		got, err := NewClient(Config{}, identityLLM{reply}).MatchIdentity(context.Background(), "B", "storm guard sergeant", ev, nil)
		if err != nil || got != want {
			t.Errorf("reply %s: got %q, %v; want %q", reply, got, err, want)
		}
	}
}

// scriptedIdentityLLM answers each identity question by the phrase asked.
type scriptedIdentityLLM map[string]string

func (l scriptedIdentityLLM) LLMGenerate(_ context.Context, _, user string, _ float32, _ int) (string, error) {
	for phrase, reply := range l {
		if strings.Contains(user, "Phrase: \""+phrase+"\"") {
			return reply, nil
		}
	}
	return `{"same_as": "", "evidence": ""}`, nil
}

func TestResolveCastIdentities(t *testing.T) {
	book := []string{
		"The storm guard sergeant, Louisa, asked with some surprise.",
		"“Tobias!” That was the name of the man in grey, Toby, Bert's cousin.",
		"Bert laughed.",
	}
	roster := Roster{Names: []string{"Sergeant Louisa", "Toby", "Bert"}, Aliases: map[string][]string{"Sergeant Louisa": {"Louisa"}}, Pronouns: map[string]string{"Toby": PronounsHe}}
	ix := NewIdentityIndex(book, map[string][]string{"Sergeant Louisa": {"Louisa"}, "Toby": nil, "Bert": nil})
	llm := scriptedIdentityLLM{
		"storm guard sergeant": `{"same_as": "Sergeant Louisa", "evidence": "The storm guard sergeant, Louisa, asked"}`,
		"Tobias":               `{"same_as": "Toby", "evidence": "the man in grey, Toby"}`,
	}
	sum := ChapterSummary{
		Scenes: []Scene{{StartIdx: 0, Present: []string{"Tobias", "Guard"}}},
		Cast: []CastMember{
			{Name: "Tobias"},
			{Name: "Guard", Role: true, Refs: []string{"storm guard sergeant", "the gate guard"}},
			{Name: "Bert"},
		},
	}
	got, aliases, err := NewClient(Config{}, llm).ResolveCastIdentities(context.Background(), "B", sum, ix, roster)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 1 || aliases[0] != (CastAlias{Character: "Toby", Alias: "Tobias"}) {
		t.Fatalf("aliases = %v", aliases)
	}
	byName := map[string]CastMember{}
	for _, m := range got.Cast {
		byName[m.Name] = m
	}
	if _, ok := byName["Tobias"]; ok {
		t.Fatalf("Tobias should have become Toby: %+v", got.Cast)
	}
	if g := byName["Guard"]; len(g.Refs) != 1 || g.Refs[0] != "the gate guard" {
		t.Fatalf("Guard refs = %v, want the sergeant moved out", g.Refs)
	}
	if l := byName["Sergeant Louisa"]; len(l.Refs) != 1 || l.Refs[0] != "storm guard sergeant" {
		t.Fatalf("Louisa = %+v", l)
	}
	if got.Scenes[0].Present[0] != "Toby" {
		t.Fatalf("scene present = %v", got.Scenes[0].Present)
	}
}

func TestMentionsAcrossCurlyQuotes(t *testing.T) {
	for text, want := range map[string]bool{
		"“tobias!” he said": true,
		"‘tobias’":          true,
		"tobiasson arrived": false,
		"zétobias":          false,
		"said tobias.":      true,
	} {
		if got := mentions([]string{"tobias"}, text); got != want {
			t.Errorf("mentions(%q) = %v, want %v", text, got, want)
		}
	}
}
