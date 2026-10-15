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
	}, nil))
	want := []string{"Centurion Primus Restun <- Primus Restun", "Flux <- Intaglio", "Baron Sanguis <- Sanguis"}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("pairs:\n got %v\nwant %v", got, want)
	}
}

func TestOneOfKind(t *testing.T) {
	for in, want := range map[string]string{
		"One of the [Knights]":                      "Knight",
		"One of the Gnolls":                         "Gnoll",
		"One of the Summer Knights":                 "Summer Knight",
		"One of the [Witches]":                      "Witch",
		"One of the half-Elves":                     "Half-Elf",
		"One of the Rulers of Khelt":                "Ruler of Khelt",
		"One of the Jecrassian [Soldiers]":          "Jecrassian Soldier",
		"One of the other officers":                 "Officer",
		"One of the two Tallguard accompanying Noa": "Tallguard",
		"One of the Gladefolk":                      "Gladefolk",
		"one of the men":                            "Man",
	} {
		if got, _, ok := OneOfKind(in); !ok || got != want {
			t.Errorf("OneOfKind(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"One of the others", "One of the crowd", "One of the [Necromancers] 1", "One of Kanadith’s helpers", "One of his officers", "Gnoll", "The Crone"} {
		if got, _, ok := OneOfKind(in); ok {
			t.Errorf("OneOfKind(%q) = %q, want no kind", in, got)
		}
	}
	if _, c, _ := OneOfKind("One of the [Mages]"); strings.Join(c, "|") != "Mage|[Mage]|Mages|[Mages]" {
		t.Errorf("candidates for One of the [Mages] = %q", c)
	}
}

// "One of the X" goes into the role X when there is one - a role first,
// whichever spelling it has - and is otherwise renamed to it; a bracketed
// name loses its brackets the same way.
func TestRosterRuleFixesOneOfAndBrackets(t *testing.T) {
	role := func(id, name string) RosterEntry {
		e := entry(id, name, 10)
		e.IsRole = true
		return e
	}
	fixes := RosterRuleFixes([]RosterEntry{
		role("oknights", "One of the [Knights]"), role("knight", "Knight"),
		role("ognolls", "One of the Gnolls"), entry("gnolls", "Gnolls", 4), role("gnoll", "Gnoll"),
		entry("ofrae", "One of the Fraerlings", 56),
		role("obig", "One of the [Mages]"), role("bmage", "[Mage]"),
		entry("bsoldier", "[soldier]", 3), role("soldier", "Soldier"),
		role("bwitch", "[Witch]"),
		entry("balch", "[alchemists]", 2), role("alch", "Alchemist"),
		entry("others", "One of the others", 867),
	}, nil, nil)
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
		"merge One of the [Knights] -> Knight",
		"merge One of the Gnolls -> Gnoll",
		"rename_role One of the Fraerlings [Fraerling]",
		"merge One of the [Mages] -> [Mage]",
		"rename [Mage] [Mage]",
		"merge [soldier] -> Soldier",
		"rename [Witch] [Witch]",
		"merge [alchemists] -> Alchemist",
		"mark_role One of the others",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("fixes:\n got %q\nwant %q", got, want)
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
	}, kinds, nil)
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

func TestPairMentionsAndSharesName(t *testing.T) {
	ix := NewIdentityIndex([]string{
		"Doctor Zheng knelt in the stream.",
		"“Dr. Zheng, are you enjoying the tour?”",
		"Doctor Zheng waved at Dr. Zheng's reflection.",
		"Zee laughed.",
	}, nil)
	a, b := entry("a", "Doctor Zheng", 18), entry("b", "Dr. Zheng", 4, "Zee")
	m := ix.PairMentions(a, b)
	if len(m.Both) != 1 || len(m.OnlyA) != 1 || len(m.OnlyB) != 2 {
		t.Errorf("mentions = %+v", m)
	}
	// "Zee" is b's alias: that paragraph names b, but isn't how the book
	// uses the name "Dr. Zheng".
	if len(m.NameA) != 1 || m.NameA[0] != 0 || len(m.NameB) != 1 || m.NameB[0] != 1 || m.WrittenA != 2 || m.WrittenB != 2 {
		t.Errorf("name paragraphs = %v / %v, written %d / %d", m.NameA, m.NameB, m.WrittenA, m.WrittenB)
	}

	names := func(es []RosterEntry) string {
		var out []string
		for _, e := range es {
			out = append(out, e.Name)
		}
		return strings.Join(out, ", ")
	}
	spellings := RosterPair{A: a, B: b}
	if !spellings.SharesName() || names(spellings.Shorter()) != "Doctor Zheng, Dr. Zheng" {
		t.Errorf("two spellings: SharesName %v, Shorter %q", spellings.SharesName(), names(spellings.Shorter()))
	}
	full := RosterPair{A: entry("c", "Hans Chotek", 50), B: entry("d", "Chotek", 9)}
	if !full.SharesName() || names(full.Shorter()) != "Chotek" {
		t.Errorf("a surname: SharesName %v, Shorter %q", full.SharesName(), names(full.Shorter()))
	}
	alias := RosterPair{A: entry("e", "Darakin", 777, "Othair"), B: entry("f", "Othair", 90), Alias: "Othair"}
	if alias.SharesName() {
		t.Error("an alias pair doesn't share its names")
	}
}

func TestJudgeSamePersonNamesUsedAlike(t *testing.T) {
	a, b := entry("a", "Doctor Zheng", 18), entry("b", "Dr. Zheng", 4)
	found := PairPassages{
		OnlyA: []string{"One of the scientists, a Doctor Zheng, wandered onto the bridge."},
		OnlyB: []string{"Dr. Zheng, the biologist, was kneeling in the stream."},
	}
	reply := verdictLLM{`{"evidence": "Dr. Zheng, the biologist, was kneeling", "verdict": "same"}`}
	c := NewClient(Config{}, reply)
	if got, _, err := c.JudgeSamePerson(context.Background(), "Book", a, b, found); err != nil || got != VerdictUnclear {
		t.Errorf("no passage names both: got %q (%v), want unclear", got, err)
	}
	found.Alike = true
	if got, evidence, err := c.JudgeSamePerson(context.Background(), "Book", a, b, found); err != nil || got != VerdictSame || evidence == "" {
		t.Errorf("names used alike: got %q, %q (%v), want same with its evidence", got, evidence, err)
	}
	made := NewClient(Config{}, verdictLLM{`{"evidence": "words that are nowhere in the passages", "verdict": "same"}`})
	// The model's "same" still stands, with nothing quoted for it.
	if got, evidence, _ := made.JudgeSamePerson(context.Background(), "Book", a, b, found); got != VerdictSame || evidence != "" {
		t.Errorf("made-up evidence: got %q, %q; want same with no evidence", got, evidence)
	}
}
