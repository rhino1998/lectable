package speakerattr

import (
	"context"
	"strings"
	"testing"
)

func entry(id, name string, lines int, aliases ...string) RosterEntry {
	return RosterEntry{ID: id, Name: name, Lines: lines, Aliases: aliases}
}

func pairNames(pairs []RosterPair) []string {
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.A.Name + " <- " + p.B.Name
	}
	return out
}

func TestRosterPairs(t *testing.T) {
	he := entry("darakin", "Darakin", 700, "Daralen")
	he.Pronouns = "he/him"
	she := entry("daralen", "Daralen", 200)
	she.Pronouns = "she/her"
	role := entry("guard", "Guard", 50)
	role.IsRole = true
	got := pairNames(RosterPairs([]RosterEntry{
		entry("r1", "Centurion Primus Restun", 680), entry("r2", "Primus Restun", 496),
		entry("s1", "Baron Sanguis", 154), entry("s2", "Sanguis", 10),
		entry("t1", "Cletus Thane", 26), entry("t2", "Mara Thane", 30), // relatives: each has a word the other lacks
		entry("d1", "Daana", 40), entry("d2", "Daana's mother", 22), // someone else than Daana
		entry("g0", "Gnome", 250), entry("g1", "Gnome 1", 6),
		entry("c1", "Centurion Hennen", 31), entry("c2", "Centurion Brina", 10), // share only a rank
		entry("b", "Baron", 36, "Joshua"), entry("j", "Joshua", 12), // a bare title holds no one's name
		entry("f", "Flux", 475, "Intaglio"), entry("i", "Intaglio", 20),
		he, she, role, entry("gc", "Guard Captain", 5),
	}))
	want := []string{"Centurion Primus Restun <- Primus Restun", "Flux <- Intaglio", "Baron Sanguis <- Sanguis"}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("pairs:\n got %v\nwant %v", got, want)
	}
}

func TestRosterRuleFixes(t *testing.T) {
	kinds := NewWordCaseCounts([]string{"Gnome", "Jack"})
	for _, text := range []string{"The gnome bowed.", "A gnome, then another gnome.", "Gnome!", "Jack ran.", "Jack sat.", "a jack of all trades"} {
		kinds.Add(text)
	}
	he := entry("flux", "Flux", 400, "Intaglio")
	he.Pronouns = "he/him"
	she := entry("intaglio", "Intaglio", 20)
	she.Pronouns = "she/her"
	fixes := RosterRuleFixes([]RosterEntry{
		entry("gnome", "Gnome", 250, "Baant", "Cleq"),
		entry("jack", "Jack", 27000, "Jax", "Silas"),
		entry("helm", "helmsman", 19),
		entry("junk", "Someone else", 8),
		entry("markup", "Elder [role]", 3), entry("elder", "Elder", 30),
		entry("baron", "Baron", 36, "Joshua"), entry("joshua", "Joshua", 12),
		he, she,
	}, kinds)
	var got []string
	for _, f := range fixes {
		line := string(f.Kind) + " " + f.Subject.Name
		if f.Detail != "" {
			line += " [" + f.Detail + "]"
		}
		if f.Kind == RosterMerge {
			line += " -> " + f.Target.Name
		}
		got = append(got, line)
	}
	want := []string{
		"mark_role Gnome", "remove_alias Gnome [Baant]", "remove_alias Gnome [Cleq]",
		"mark_role helmsman",
		"invalidate Someone else",
		"merge Elder [role] -> Elder",
		"remove_alias Baron [Joshua]",
		"remove_alias Flux [Intaglio]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("fixes:\n got %q\nwant %q", got, want)
	}
}

func TestPairPassagesAndUnwrittenName(t *testing.T) {
	ix := NewIdentityIndex([]string{
		"“Centurion Primus Restun, thank you,” the legate said.",
		"Restun murmured approvingly.",
		"Baron Sanguis—no, just Sanguis now—hung his head.",
		"Sanguis wept.",
	}, nil)
	long, short := entry("r1", "Centurion Primus Restun", 680), entry("r2", "Primus Restun", 496, "Restun")
	found := ix.PairPassages(long, short)
	if found.NameA != 1 || found.NameB != 0 {
		t.Errorf("name hits = %d/%d, want 1/0: \"Primus Restun\" is only ever part of the longer name", found.NameA, found.NameB)
	}
	fix, ok := RosterPair{A: long, B: short}.UnwrittenName(found)
	if !ok || fix.Subject.ID != "r2" || fix.Target.ID != "r1" {
		t.Errorf("UnwrittenName = %+v, %v; want Primus Restun merged into the longer name", fix, ok)
	}

	baron, plain := entry("s1", "Baron Sanguis", 154), entry("s2", "Sanguis", 10)
	found = ix.PairPassages(baron, plain)
	if len(found.Both) != 1 || len(found.OnlyB) != 1 || found.NameA != 1 || found.NameB != 2 {
		t.Errorf("Sanguis passages = %+v", found)
	}
	if _, ok := (RosterPair{A: baron, B: plain}).UnwrittenName(found); ok {
		t.Error("both names are written: the text has to be read")
	}
}

type verdictLLM struct{ reply string }

func (l verdictLLM) LLMGenerate(context.Context, string, string, float32, int) (string, error) {
	return l.reply, nil
}

func TestJudgeSamePersonNeedsEvidenceNamingBoth(t *testing.T) {
	a, b := entry("d", "Darakin", 777), entry("o", "Othair", 90)
	found := PairPassages{
		Both:  []string{"“Welcome, Lord Darakin,” Othair said, bowing to the god."},
		OnlyB: []string{"“I am Othair, my lord, and I’d like to offer myself.”"},
	}
	for _, tc := range []struct {
		reply string
		want  SamePersonVerdict
	}{
		{`{"evidence": "I am Othair, my lord", "verdict": "same"}`, VerdictUnclear},                   // names only one of them
		{`{"evidence": "words that are nowhere in the passages", "verdict": "same"}`, VerdictUnclear}, // made up
		{`{"evidence": "Othair said, bowing to the god", "verdict": "different"}`, VerdictDifferent},
		{`{"evidence": "“Welcome, Lord Darakin,” Othair said", "verdict": "same"}`, VerdictSame},
		{`{"evidence": "", "verdict": "unclear"}`, VerdictUnclear},
	} {
		c := NewClient(Config{}, verdictLLM{tc.reply})
		got, _, err := c.JudgeSamePerson(context.Background(), "Book", a, b, found)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q (%v), want %q", tc.reply, got, err, tc.want)
		}
	}
}
