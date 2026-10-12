package speakerattr

import (
	"slices"
	"strings"
	"testing"
)

// The Wandering Inn's roster as it stood: "Prognugator" a character
// holding other characters as aliases, beside "Prognugator Tersk".
func prognugators() []RosterEntry {
	xrn := entry("xrn", "Prognugator Xrn", 90)
	xrn.Pronouns = "she/her"
	return []RosterEntry{
		entry("p", "Prognugator", 120, "Tersk", "Wrymvr", "Xrn"),
		entry("tersk", "Prognugator Tersk", 60), entry("dekass", "Prognugator Dekass", 40), xrn,
		entry("tersk2", "Tersk", 12), entry("klb", "Klbkch", 900),
	}
}

func TestWorldTitlesInRosterRules(t *testing.T) {
	entries := prognugators()
	titles := NewTitles([]string{"Prognugator"})

	// Without the title the bare word shares a name with every holder.
	if got := strings.Join(pairNames(RosterPairs(entries, nil)), "; "); !strings.Contains(got, "Prognugator <- Prognugator Dekass") {
		t.Errorf("pairs without the title: %q", got)
	}
	if got, want := strings.Join(pairNames(RosterPairs(entries, titles)), "; "), "Prognugator Tersk <- Tersk"; got != want {
		t.Errorf("pairs with the title: %q, want %q", got, want)
	}
	if !IsBareTitle("Prognugator", titles) || IsBareTitle("Prognugator", nil) || IsBareTitle("Prognugator Tersk", titles) {
		t.Error("IsBareTitle: only the bare word, and only once it is a title")
	}

	var got []string
	for _, f := range RosterRuleFixes(entries, nil, titles) {
		got = append(got, string(f.Kind)+" "+f.Subject.Name+" ["+f.Detail+"] "+f.Target.Name)
	}
	// "Tersk" is two entries' distinctive word, but one is named just
	// that; "Xrn" is "Prognugator Xrn"; no entry is Wrymvr.
	want := []string{"remove_alias Prognugator [Tersk] Tersk", "remove_alias Prognugator [Xrn] Prognugator Xrn"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("fixes:\n got %q\nwant %q", got, want)
	}
	if fixes := RosterRuleFixes(entries, nil, nil); len(fixes) != 0 {
		t.Errorf("fixes without the title: %v", fixes)
	}
}

func TestResolverWithTitles(t *testing.T) {
	names := []string{"Prognugator Dekass", "Klbkch"}
	if got, ok := NewSpeakerNameResolver(names, nil).Resolve("Prognugator"); !ok || got != "Prognugator Dekass" {
		t.Fatalf("without titles: %q, %v", got, ok)
	}
	r := NewSpeakerNameResolver(names, nil).WithTitles(NewTitles([]string{"prognugator"}))
	if got, ok := r.Resolve("Prognugator"); ok {
		t.Errorf("a title alone resolved to %q", got)
	}
	for _, written := range []string{"Dekass", "Prognugator Dekass"} {
		if got, ok := r.Resolve(written); !ok || got != "Prognugator Dekass" {
			t.Errorf("Resolve(%q) = %q, %v", written, got, ok)
		}
	}
}

func TestTitleCandidates(t *testing.T) {
	role := entry("r", "Drake Guard", 5)
	role.IsRole = true
	entries := append(prognugators(),
		entry("s1", "Ser Dalimont", 80), entry("s2", "Ser Raim", 20), entry("s3", "Ser Raim", 2),
		entry("c1", "Captain Vash", 30), entry("c2", "Captain Geram", 30), // a title already
		entry("w1", "Wall Lord Ilvriss", 300), entry("w2", "Wall Lady Navine", 40), // the title is two words
		entry("l1", "Zhao Wei", 50), entry("l2", "Zhao Na", 50), // a family name: TitleUses' to rule out
		entry("a1", "Archer B23", 3), entry("a2", "Archer B25", 3), // numbered
		entry("m1", "Erin Solstice", 9000),                             // nobody else is an Erin
		entry("f1", "First Gardener", 9), entry("f2", "First Mind", 9), // which of several, not a title
		entry("t1", "Tolveilouka Ve'delina Mer", 9), entry("t2", "Tolveilouka Ve’delina Mer", 2), // one person, two spellings
		entry("k1", "Drake Trader", 4), entry("k2", "Drake Tactician", 4), entry("k3", "Drake Tesy", 4),
		role, entry("d2", "Drake [role]", 1),
	)
	var got []string
	for _, c := range TitleCandidates(entries, nil) {
		got = append(got, c.Word+": "+strings.Join(c.Holders, ", "))
	}
	want := []string{
		"Prognugator: Prognugator Xrn, Prognugator Tersk, Prognugator Dekass",
		"Drake: Drake Trader, Drake Tactician, Drake Tesy",
		"Ser: Ser Dalimont, Ser Raim",
		"Zhao: Zhao Wei, Zhao Na",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("candidates:\n got %q\nwant %q", got, want)
	}
	if left := TitleCandidates(entries, NewTitles([]string{"Prognugator", "ser", "zhao", "drake"})); len(left) != 0 {
		t.Errorf("known titles proposed again: %v", left)
	}

	uses := NewTitleUses([]string{"Prognugator", "Zhao"})
	for i, text := range []string{
		"Zhao Wei bowed. Zhao Na did not.",
		"Klbkch waited. “The Prognugator will see you now,” the Worker said. It left.",
		"Two Prognugators stood at the gate, and a third inside.",
		"Every Hive had its Prognugator.",
		"He was no Prognugator.",
		"“It was true that Prognugator Tersk had left.”", // "that" is no article
	} {
		uses.Add(string(rune('a'+i)), text)
	}
	if u := uses.Of("zhao"); u.Paragraphs != 0 {
		t.Errorf("Zhao used as a title in %d paragraphs", u.Paragraphs)
	}
	u := uses.Of("Prognugator")
	if u.Paragraphs != 3 || u.EvidenceAt != "b" || u.Evidence != "“The Prognugator will see you now,” the Worker said." {
		t.Errorf("Prognugator: %d paragraphs, evidence %q at %q", u.Paragraphs, u.Evidence, u.EvidenceAt)
	}
	// A kind leads names too, but ordinary words follow it.
	drake := TitleCandidate{Word: "Drake", Holders: []string{"Drake Trader", "Drake Tactician", "Drake Tesy"}}
	named := NewWordCaseCounts(drake.HolderWords())
	for _, text := range []string{"The trader and the tactician met Tesy.", "A trader. Trader!", "Our tactician knows Tesy."} {
		named.Add(text)
	}
	if got := strings.Join(drake.NamedHolders(named), ", "); got != "Drake Tesy" {
		t.Errorf("NamedHolders = %q", got)
	}
	if got := strings.Join(NamesUsing("prognugator", []string{"Prognugator", "Prognugator Tersk", "Klbkch"}), ", "); got != "Prognugator Tersk" {
		t.Errorf("NamesUsing = %q", got)
	}
}

func TestPeopleWordsInRosterRules(t *testing.T) {
	words := NewWorldWords(map[string]string{"Drake": WordRace, "gnoll": WordRace, "pallassian": WordNationality, "trader": WordRole, "chieftain": WordTitle, "pallass": WordPlace})
	for name, want := range map[string]bool{
		"Drake": true, "Pallassian Gnoll": true, "Drake Trader": true, "One of the Gnolls": true, "Female Gnoll": true,
		"Drake Tesy": false, "Gnoll Chieftain": false, "Chieftain": false, "Pallass": false, "Tesy": false,
	} {
		if got := words.IsPeopleDescription(name); got != want {
			t.Errorf("IsPeopleDescription(%q) = %v", name, got)
		}
	}
	entries := []RosterEntry{
		entry("d", "Drake", 400, "Relc"), entry("relc", "Relc", 900),
		entry("pg", "Pallassian Gnoll", 12), entry("dt", "Drake Tesy", 30), entry("t", "Tesy", 4),
		entry("gc", "Gnoll Chieftain", 20),
	}
	var got []string
	for _, f := range RosterRuleFixes(entries, nil, words) {
		got = append(got, string(f.Kind)+" "+f.Subject.Name+" ["+f.Detail+"]")
	}
	want := []string{"mark_role Drake []", "remove_alias Drake [Relc]", "mark_role Pallassian Gnoll []"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("fixes:\n got %q\nwant %q", got, want)
	}
	if got, want := strings.Join(pairNames(RosterPairs(entries, words)), "; "), "Drake Tesy <- Tesy"; got != want {
		t.Errorf("pairs: %q, want %q", got, want)
	}

	var cands []string
	for _, c := range WordCandidates(append(entries, entry("o", "One of the Drakes", 3), entry("lp", "Lord of Pallass", 5), entry("qp", "Queen of Pallass", 5)), NewTitles([]string{"chieftain"})) {
		cands = append(cands, c.Word)
	}
	if got := strings.Join(cands, ", "); got != "Drake, Tesy, Gnoll, Pallass" {
		t.Errorf("WordCandidates = %q", got)
	}
}

func TestMultiwordTitles(t *testing.T) {
	entries := []RosterEntry{
		entry("i1", "Wall Lord Ilvriss", 300), entry("i2", "Ilvriss", 40), entry("b", "Wall Lord Brilm", 20),
		entry("n", "Wall Lady Navine", 10), entry("w", "Wall Lord", 15, "Brilm"),
		entry("m1", "Lady Magnolia Reinhart", 500), entry("m2", "Lady Magnolia", 5), // a name after a title
		entry("d", "Drowned Folk Captain", 8), entry("q", "Queen of the Antinium", 60),
	}
	var cands []string
	for _, c := range WordCandidates(entries, nil) {
		cands = append(cands, c.Word+": "+strings.Join(c.Names, ", "))
	}
	// "Wall" is only ever part of "Wall Lord" but for Navine: one name is
	// no candidate - unless it is all that follows "of" (Antinium).
	if got, want := strings.Join(cands, "\n"), "Wall Lord: Wall Lord Ilvriss, Wall Lord Brilm, Wall Lord\nMagnolia: Lady Magnolia Reinhart, Lady Magnolia\nIlvriss: Wall Lord Ilvriss, Ilvriss\nAntinium: Queen of the Antinium"; got != want {
		t.Errorf("candidates:\n got %q\nwant %q", got, want)
	}

	role := entry("o", "One of the Drowned Folk", 3)
	role.IsRole = true
	cands = nil
	for _, c := range WordCandidates(append(entries, role, entry("k", "King of the Blighted Lands", 9), entry("h", "Healer of Tenbault", 4), entry("m3", "Magnolia Reinhart", 2)), nil) {
		cands = append(cands, c.Word)
	}
	// A people (two entries, one a role) and places after "of"; "Magnolia
	// Reinhart" is in two entries too, but both are names.
	for _, want := range []string{"Wall Lord", "Drowned Folk", "Blighted Lands", "Tenbault"} {
		if !slices.Contains(cands, want) {
			t.Errorf("%q is no candidate: %v", want, cands)
		}
	}
	for _, not := range []string{"Magnolia Reinhart", "Lady Magnolia", "Lands", "Blighted", "Folk"} {
		if slices.Contains(cands, not) {
			t.Errorf("%q is a candidate: %v", not, cands)
		}
	}

	words := NewWorldWords(map[string]string{"Wall  Lord": WordTitle, "drowned folk": WordRace})
	if !words.Has("wall lord") || words.Has("Wall") {
		t.Error("Has: the phrase, not its words")
	}
	if !IsBareTitle("Wall Lord", words) || IsBareTitle("Wall Lord Ilvriss", words) || !IsBareTitle("Wall Lords", words) {
		t.Error("IsBareTitle with a title of two words")
	}
	if got, want := strings.Join(pairNames(RosterPairs(entries, words)), "; "), "Wall Lord Ilvriss <- Ilvriss; Lady Magnolia Reinhart <- Lady Magnolia"; got != want {
		t.Errorf("pairs: %q, want %q", got, want)
	}
	var fixes []string
	for _, f := range RosterRuleFixes(entries, nil, words) {
		fixes = append(fixes, string(f.Kind)+" "+f.Subject.Name+" ["+f.Detail+"] "+f.Target.Name)
	}
	if got, want := strings.Join(fixes, "\n"), "remove_alias Wall Lord [Brilm] Wall Lord Brilm"; got != want {
		t.Errorf("fixes: %q, want %q", got, want)
	}
	if !words.IsPeopleDescription("One of the Drowned Folk") || words.IsPeopleDescription("Drowned Folk Captain") || words.IsPeopleDescription("Wall Lord") {
		t.Error("IsPeopleDescription with a race of two words")
	}
	if got := strings.Join(NamesUsing("wall lord", []string{"Wall Lord", "Wall Lord Brilm", "Wall Lady Navine", "Two Wall Lords"}), ", "); got != "Wall Lord Brilm, Two Wall Lords" {
		t.Errorf("NamesUsing = %q", got)
	}
	r := NewSpeakerNameResolver([]string{"Wall Lord Brilm", "Erin"}, nil).WithTitles(words)
	if got, ok := r.Resolve("Wall Lord"); ok {
		t.Errorf("a title of two words resolved to %q", got)
	}
	if got, ok := r.Resolve("Brilm"); !ok || got != "Wall Lord Brilm" {
		t.Errorf("Resolve(Brilm) = %q, %v", got, ok)
	}
	uses := NewTitleUses([]string{"Wall Lord"})
	uses.Add("c", "He bowed to the Wall  Lords of Salazsar.")
	if u := uses.Of("wall lord"); u.Paragraphs != 1 {
		t.Errorf("title uses of a phrase: %d", u.Paragraphs)
	}
}

func TestNumberedRoleMergedIntoItsRole(t *testing.T) {
	role := func(id, name string) RosterEntry {
		e := entry(id, name, 5)
		e.IsRole = true
		return e
	}
	var got []string
	for _, f := range RosterRuleFixes([]RosterEntry{role("a", "Adventurer"), role("a7", "Adventurer 7"), role("g3", "Gnome 3"), entry("u", "Unit 7", 9), entry("un", "Unit", 9)}, nil, nil) {
		got = append(got, string(f.Kind)+" "+f.Subject.Name+" -> "+f.Target.Name)
	}
	// "Gnome 3" has no plain "Gnome" to join, and "Unit 7" is a name.
	if strings.Join(got, "; ") != "merge Adventurer 7 -> Adventurer" {
		t.Errorf("fixes = %q", got)
	}
}
