package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Roster review: finding what is wrong with a roster that many chapters of
// attribution built up - one person under two names, a label for a kind of
// person that took several people's names as its aliases, an alias that is
// somebody else's name, a name that names nobody. Rules find what they can
// outright (RosterRuleFixes); the rest are pairs of entries that may be one
// person (RosterPairs), which only the text can settle: the passages
// naming them are found in code (IdentityIndex.PairPassages) and the model
// only answers same, different or unclear, with the words that show it
// (Client.JudgeSamePerson) - identity retrieval's shape. Nothing here
// changes a roster; httpapi.ReviewRoster stores each result as a proposal
// for the reader.

// RosterEntry is one character of a roster under review.
type RosterEntry struct {
	ID       string
	Name     string
	Aliases  []string
	Pronouns string
	IsRole   bool
	// Lines is how many lines of dialogue are attributed to them.
	Lines int
}

// RosterFixKind is a change to a roster a review can propose.
type RosterFixKind string

const (
	// RosterMerge folds Subject into Target: one person under two names.
	RosterMerge RosterFixKind = "merge"
	// RosterRemoveAlias drops Detail from Subject's aliases.
	RosterRemoveAlias RosterFixKind = "remove_alias"
	// RosterSplitAlias drops Detail from Subject's aliases and makes it a
	// character of its own, Subject's lines looked over again for the ones
	// that are theirs: someone else folded into Subject under that name.
	RosterSplitAlias RosterFixKind = "split_alias"
	// RosterMarkRole makes Subject a role label - a kind of person.
	RosterMarkRole RosterFixKind = "mark_role"
	// RosterInvalidate marks Subject as no one's name, so attribution
	// stops using it.
	RosterInvalidate RosterFixKind = "invalidate"
)

// RosterFix is one proposed change.
type RosterFix struct {
	Kind    RosterFixKind
	Subject RosterEntry
	// Target is the entry Subject is merged into (RosterMerge), or the
	// owner of the name an alias collides with (RosterRemoveAlias, when
	// there is one).
	Target RosterEntry
	// Detail is the alias to remove.
	Detail string
	Reason string
	// Evidence is the passage that shows it, when one does.
	Evidence []string
}

// RosterPair is two entries that may be one person. A is the one that
// would be kept (more lines; the longer name on a tie).
type RosterPair struct {
	A, B RosterEntry
	// Alias is the alias of one that is the other's name, "" when the pair
	// only shares a word.
	Alias string
	// AliasHolder is whichever of A and B has Alias.
	AliasHolder RosterEntry
	// titles are the world's own titles the pair was made with
	// (RosterPairs), which its names' distinctive words leave out.
	titles WorldWords
}

// Entries is the pair as it should be searched for and shown: A and B, the
// alias in question taken off whichever has it - with it, every passage
// naming the one would count as naming the other.
func (p RosterPair) Entries() (a, b RosterEntry) {
	strip := func(e RosterEntry) RosterEntry {
		if p.Alias == "" || e.ID != p.AliasHolder.ID {
			return e
		}
		kept := make([]string, 0, len(e.Aliases))
		for _, al := range e.Aliases {
			if !strings.EqualFold(al, p.Alias) {
				kept = append(kept, al)
			}
		}
		e.Aliases = kept
		return e
	}
	return strip(p.A), strip(p.B)
}

// SharesName reports whether the pair was made by its names - one's
// distinctive words all among the other's ("Zheng", "Doctor Zheng") -
// rather than by an alias.
func (p RosterPair) SharesName() bool {
	wa, wb := rosterNameWords(p.A.Name, p.titles), rosterNameWords(p.B.Name, p.titles)
	return p.Alias == "" && len(wa) > 0 && len(wb) > 0 && (subsetOf(wa, wb) || subsetOf(wb, wa))
}

// Shorter returns the entries of a SharesName pair whose name could be
// short for the other's: the one whose words are all among the other's,
// or both when they have the same words ("Dr. Zheng", "Doctor Zheng").
func (p RosterPair) Shorter() []RosterEntry {
	wa, wb := rosterNameWords(p.A.Name, p.titles), rosterNameWords(p.B.Name, p.titles)
	var out []RosterEntry
	if subsetOf(wa, wb) {
		out = append(out, p.A)
	}
	if subsetOf(wb, wa) {
		out = append(out, p.B)
	}
	return out
}

// rosterRankWords are ranks and offices beyond titleWords that two
// different people can share ("Centurion Hennen", "Centurion Brina"), so
// they don't make a pair on their own.
var rosterRankWords = map[string]bool{
	"centurion": true, "primus": true, "legionnaire": true, "optio": true, "tribune": true, "prefect": true,
	"baron": true, "baroness": true, "earl": true, "emperor": true, "empress": true, "reeve": true,
	"madame": true, "madam": true, "professor": true, "priest": true, "priestess": true, "matriarch": true,
	"patriarch": true, "lieutenant": true, "major": true, "colonel": true, "private": true, "corporal": true,
	"admiral": true, "officer": true, "uncle": true, "aunt": true, "grandmother": true, "grandfather": true,
	"saint": true, "high": true, "grand": true, "great": true, "little": true, "big": true, "arch": true,
	"of": true, "and": true, "von": true, "van": true, "de": true, "man": true, "woman": true, "leader": true,
}

// rosterNameWords is name's distinctive words, lower-cased: no titles
// (titles' own beside the fixed ones, a title of several words - "Wall
// Lord" - taken out whole), ranks, numbers or words under three letters.
func rosterNameWords(name string, titles WorldWords) []string {
	var out []string
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’'
	})
	if rest, found := titles.withoutPhrases(strings.Join(words, " "), nil); found {
		words = strings.Fields(rest)
	}
	for _, w := range words {
		w = strings.TrimSuffix(strings.TrimSuffix(w, "'s"), "’s")
		if len([]rune(w)) < 3 || titles.rank(w) || strings.IndexFunc(w, unicode.IsDigit) >= 0 {
			continue
		}
		out = append(out, w)
	}
	return out
}

// namedByAnother reports whether name is a possessive ("Daana's mother" -
// someone else than Daana) or carries a number ("Gnome 1", one of several
// in a scene): sharing a word with another entry says nothing about either.
func namedByAnother(name string) bool {
	if strings.IndexFunc(name, unicode.IsDigit) >= 0 {
		return true
	}
	for _, w := range strings.Fields(name) {
		if strings.HasSuffix(w, "'s") || strings.HasSuffix(w, "’s") {
			return true
		}
	}
	return false
}

// IsBareTitle reports whether name is only titles and ranks ("Baron",
// "Centurion Primus", or one of titles) - something several people can be
// called, so never one person's first name.
func IsBareTitle(name string, titles WorldWords) bool { return len(rosterNameWords(name, titles)) == 0 }

// subsetOf reports whether every word of a is in b.
func subsetOf(a, b []string) bool {
	for _, w := range a {
		found := false
		for _, x := range b {
			if w == x {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// keepFirst orders a pair: the entry with more lines first, the longer
// name on a tie.
func keepFirst(a, b RosterEntry) (RosterEntry, RosterEntry) {
	if b.Lines > a.Lines || b.Lines == a.Lines && len(b.Name) > len(a.Name) {
		return b, a
	}
	return a, b
}

// pronounsDiffer reports whether both entries have pronouns and they
// aren't the same ones.
func pronounsDiffer(a, b RosterEntry) bool {
	return a.Pronouns != "" && b.Pronouns != "" && a.Pronouns != b.Pronouns
}

// maxSharedWordEntries is how many entries may share a word for it to
// still pair them up - a word half the cast carries ("Gnome 1".."Gnome 9",
// a clan name) says nothing.
const maxSharedWordEntries = 4

// RosterPairs returns the pairs of entries that may be one person, the
// pairs with the most lines at stake first:
//   - one has an alias that is the other's name (or they share an alias);
//   - neither is a role and one's distinctive words (rosterNameWords) are
//     all among the other's: "Primus Restun" and "Centurion Primus
//     Restun", "Sanguis" and "Baron Sanguis". "Cletus Thane" and "Mara
//     Thane" each have a word the other lacks, and aren't paired; nor is a
//     possessive or numbered name ("Daana's mother", "Gnome 1").
//
// Entries whose pronouns differ are never paired, nor is a bare title
// ("Baron") with the character whose name it has as an alias
// (RosterRuleFixes proposes removing the alias in both cases). titles are
// the world's own, dropped from a name like the fixed ones: with
// "Prognugator" among them, "Prognugator Tersk" and "Tersk" are a pair and
// "Prognugator Tersk" and "Prognugator Dekass" aren't.
func RosterPairs(entries []RosterEntry, titles WorldWords) []RosterPair {
	type key [2]string
	seen := map[key]bool{}
	var out []RosterPair
	add := func(a, b RosterEntry, alias string, holder RosterEntry) {
		if a.ID == b.ID || pronounsDiffer(a, b) {
			return
		}
		k := key{a.ID, b.ID}
		if k[0] > k[1] {
			k[0], k[1] = k[1], k[0]
		}
		if seen[k] {
			return
		}
		seen[k] = true
		keep, other := keepFirst(a, b)
		out = append(out, RosterPair{A: keep, B: other, Alias: alias, AliasHolder: holder, titles: titles})
	}

	byName := map[string]RosterEntry{}
	for _, e := range entries {
		byName[strings.ToLower(e.Name)] = e
	}
	aliasHolders := map[string][]RosterEntry{}
	for _, e := range entries {
		for _, a := range e.Aliases {
			la := strings.ToLower(strings.TrimSpace(a))
			if other, ok := byName[la]; ok && other.ID != e.ID && len(rosterNameWords(e.Name, titles)) > 0 {
				add(e, other, a, e)
			}
			aliasHolders[la] = append(aliasHolders[la], e)
		}
	}
	for _, holders := range aliasHolders {
		for i := 1; i < len(holders); i++ {
			add(holders[0], holders[i], "", RosterEntry{})
		}
	}

	words := map[string][]string{}
	byWord := map[string][]RosterEntry{}
	for _, e := range entries {
		if e.IsRole || namedByAnother(e.Name) {
			continue
		}
		ws := rosterNameWords(e.Name, titles)
		words[e.ID] = ws
		for _, w := range ws {
			byWord[w] = append(byWord[w], e)
		}
	}
	for _, group := range byWord {
		if len(group) < 2 || len(group) > maxSharedWordEntries {
			continue
		}
		for i := range group {
			for j := i + 1; j < len(group); j++ {
				a, b := group[i], group[j]
				if subsetOf(words[a.ID], words[b.ID]) || subsetOf(words[b.ID], words[a.ID]) {
					add(a, b, "", RosterEntry{})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].B.Lines != out[j].B.Lines {
			return out[i].B.Lines > out[j].B.Lines
		}
		if out[i].A.Lines != out[j].A.Lines {
			return out[i].A.Lines > out[j].A.Lines
		}
		return out[i].A.Name+"\x00"+out[i].B.Name < out[j].A.Name+"\x00"+out[j].B.Name
	})
	return out
}

// minBucketAliases is how many aliases a kind word needs before
// RosterRuleFixes calls it a bucket: one ("Seneschal", also called
// "Overseer") is usually one person's title.
const minBucketAliases = 2

// rosterNobody are answers that got through as names and name no one.
var rosterNobody = map[string]bool{
	"someone": true, "someone else": true, "somebody": true, "somebody else": true, "anyone": true, "everyone": true,
	"everybody": true, "another": true, "other": true, "others": true, "the other": true, "speaker": true, "voice": true,
}

// isKindCandidate reports whether e could be a kind word that swallowed
// several people: one word, not a role or a rank, with minBucketAliases
// aliases or more.
func isKindCandidate(e RosterEntry, titles WorldWords) bool {
	lower := strings.ToLower(strings.TrimSpace(e.Name))
	return !e.IsRole && len(strings.Fields(lower)) == 1 && len(e.Aliases) >= minBucketAliases && !titles.rank(lower)
}

// RosterKindCandidates is the names RosterRuleFixes needs counted in the
// text (WordCaseCounts) to tell a kind word from a name.
func RosterKindCandidates(entries []RosterEntry, titles WorldWords) []string {
	var out []string
	for _, e := range entries {
		if isKindCandidate(e, titles) {
			out = append(out, e.Name)
		}
	}
	return out
}

// WordCaseCounts is how often a book writes each of a few words in lower
// case and capitalized: "gnome" is mostly lower case, "Jack" - though the
// book also has a "jack" or two - isn't.
type WordCaseCounts map[string]*[2]int

// NewWordCaseCounts prepares counts for words (any case).
func NewWordCaseCounts(words []string) WordCaseCounts {
	c := WordCaseCounts{}
	for _, w := range words {
		c[strings.ToLower(strings.TrimSpace(w))] = &[2]int{}
	}
	return c
}

var caseWord = regexp.MustCompile(`\b\pL[\pL'’-]*`)

// Add counts text's uses of the words.
func (c WordCaseCounts) Add(text string) {
	if len(c) == 0 {
		return
	}
	for _, w := range caseWord.FindAllString(text, -1) {
		lower := strings.ToLower(w)
		n, ok := c[lower]
		if !ok {
			continue
		}
		if w == lower {
			n[0]++
		} else {
			n[1]++
		}
	}
}

// MostlyLower reports whether word was counted in lower case more often
// than capitalized.
func (c WordCaseCounts) MostlyLower(word string) bool {
	n := c[strings.ToLower(strings.TrimSpace(word))]
	return n != nil && n[0] > n[1]
}

// RosterRuleFixes returns the fixes rules alone can propose, with no
// reading of the text:
//   - a name that names no single person ("Someone else", "(he/him)",
//     "Fritz and Bert") is invalidated;
//   - a description that isn't marked as a role ("helmsman", "tall man")
//     is marked as one;
//   - a name carrying the attribution prompt's own markup ("Elder [role]")
//     is merged into the entry with the name itself, or invalidated when
//     there is none;
//   - a role named with a number ("Adventurer 7") beside the role itself
//     is merged into it;
//   - a single word the book mostly writes in lower case ("Gnome",
//     "Dwarf" - kinds, RosterKindCandidates counted over the text; nil
//     skips this), not a role or rank, holding several aliases, is a label
//     that swallowed the people it was once used for: each alias is
//     removed and it becomes a role;
//   - an alias that is the name of another entry is removed when the two
//     have different pronouns, or when the entry holding it is a bare title
//     ("Baron", also called "Joshua") - which also loses an alias that is
//     another entry's name without its titles ("Prognugator", also called
//     "Tersk", beside "Prognugator Tersk").
//
// titles are the world's own words (WorldWords): its titles are read like
// the fixed ones, and a name made only of its roles, races and
// nationalities ("Drake", "Pallassian Gnoll", "Drake Trader") is marked a role and loses its aliases.
func RosterRuleFixes(entries []RosterEntry, kinds WordCaseCounts, titles WorldWords) []RosterFix {
	var out []RosterFix
	byName := map[string]RosterEntry{}
	// byWords is the one entry with each set of distinctive words: who a
	// bare title's alias "Tersk" is, when no entry is named just that.
	byWords := map[string]RosterEntry{}
	for _, e := range entries {
		byName[strings.ToLower(e.Name)] = e
	}
	for _, e := range entries {
		if e.IsRole || namedByAnother(e.Name) {
			continue
		}
		if ws := strings.Join(rosterNameWords(e.Name, titles), " "); ws != "" {
			if _, taken := byWords[ws]; taken {
				byWords[ws] = RosterEntry{} // two entries: not one person's
			} else {
				byWords[ws] = e
			}
		}
	}
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		clean, _ := sanitizeSpeaker(name, e.IsRole)
		if clean == "Unknown" || clean == "Narrator" || strings.IndexFunc(clean, unicode.IsLetter) < 0 || rosterNobody[strings.ToLower(clean)] {
			out = append(out, RosterFix{Kind: RosterInvalidate, Subject: e, Reason: "names no single person"})
			continue
		}
		if strings.Contains(strings.ToLower(name), "[role]") {
			if other, ok := byName[strings.ToLower(clean)]; ok && other.ID != e.ID {
				out = append(out, RosterFix{Kind: RosterMerge, Subject: e, Target: other, Reason: "the same name, with the attribution prompt's markup left in"})
			} else {
				out = append(out, RosterFix{Kind: RosterInvalidate, Subject: e, Reason: "the attribution prompt's markup, not a name"})
			}
			continue
		}
		// A role that kept a number in its name ("Adventurer 7") beside the
		// role itself: the number is a voice slot of that role, not a role.
		if label, n := SplitRoleIndex(name); n > 0 && e.IsRole {
			if other, ok := byName[strings.ToLower(label)]; ok && other.ID != e.ID && other.IsRole {
				out = append(out, RosterFix{Kind: RosterMerge, Subject: e, Target: other, Reason: fmt.Sprintf("a numbered %s, not a role of its own", strings.ToLower(other.Name))})
				continue
			}
		}
		bucket := false
		if !e.IsRole {
			switch {
			case titles.IsPeopleDescription(name):
				// Any alias at all: a people has no other name.
				out = append(out, RosterFix{Kind: RosterMarkRole, Subject: e, Reason: "a role, race or nationality of this world, not a name"})
				bucket = len(e.Aliases) > 0
			case DescriptorWithoutArticle(name):
				out = append(out, RosterFix{Kind: RosterMarkRole, Subject: e, Reason: "a description, not a name"})
				bucket = len(e.Aliases) >= minBucketAliases
			case isKindCandidate(e, titles) && kinds.MostlyLower(name):
				out = append(out, RosterFix{Kind: RosterMarkRole, Subject: e, Reason: fmt.Sprintf("the book writes %q as an ordinary word, and it goes by %d other names", strings.ToLower(name), len(e.Aliases))})
				bucket = true
			}
		}
		bareTitle := len(rosterNameWords(name, titles)) == 0
		for _, a := range e.Aliases {
			other, collides := byName[strings.ToLower(strings.TrimSpace(a))]
			collides = collides && other.ID != e.ID
			if !collides && bareTitle {
				other, collides = byWords[strings.Join(rosterNameWords(a, titles), " ")]
				collides = collides && other.ID != "" && other.ID != e.ID
			}
			switch {
			case bucket:
				fix := RosterFix{Kind: RosterRemoveAlias, Subject: e, Detail: a, Reason: "a label for a kind of person holding one person's name"}
				if collides {
					fix.Target = other
				}
				out = append(out, fix)
			case collides && bareTitle:
				out = append(out, RosterFix{Kind: RosterRemoveAlias, Subject: e, Target: other, Detail: a,
					Reason: fmt.Sprintf("%s is another character, and %q is a title several people can hold", other.Name, name)})
			case collides && pronounsDiffer(e, other):
				out = append(out, RosterFix{Kind: RosterRemoveAlias, Subject: e, Target: other, Detail: a,
					Reason: fmt.Sprintf("%s is another character, with different pronouns (%s, not %s)", other.Name, other.Pronouns, e.Pronouns)})
			}
		}
	}
	return out
}

const (
	// pairPassagesEach is how many passages PairPassages returns of each
	// kind (naming both, only one, only the other).
	pairPassagesEach = 3
	// pairPassageChars caps one passage, centred on the name.
	pairPassageChars = 420
)

// PairPassages are passages of one book for a RosterPair: those naming
// both entries in one paragraph, and those naming only one.
type PairPassages struct {
	Both, OnlyA, OnlyB []string
	// NameA/NameB count the paragraphs writing each entry's own name (not
	// an alias) other than as part of the other's.
	NameA, NameB int
	// Alike says the book uses the two names the same way - the paragraphs
	// writing one read like the paragraphs writing the other, which
	// httpapi.ReviewRoster measures with their embeddings. JudgeSamePerson
	// then takes the model's "same" without a passage naming both: two
	// spellings of one name ("Dr. Zheng", "Doctor Zheng") never share a
	// paragraph.
	Alike bool
}

// Empty reports whether nothing names both, or nothing names each.
func (p PairPassages) Empty() bool {
	return len(p.Both) == 0 && (len(p.OnlyA) == 0 || len(p.OnlyB) == 0)
}

// All is every passage, those naming both first.
func (p PairPassages) All() []string {
	return append(append(append([]string(nil), p.Both...), p.OnlyA...), p.OnlyB...)
}

// Append adds q's passages to p's, each kind kept to pairPassagesEach.
func (p PairPassages) Append(q PairPassages) PairPassages {
	join := func(a, b []string) []string {
		a = append(a, b...)
		return a[:min(len(a), pairPassagesEach)]
	}
	return PairPassages{Both: join(p.Both, q.Both), OnlyA: join(p.OnlyA, q.OnlyA), OnlyB: join(p.OnlyB, q.OnlyB), NameA: p.NameA + q.NameA, NameB: p.NameB + q.NameB, Alike: p.Alike || q.Alike}
}

// rosterForms is an entry's name, then its aliases of three letters or
// more, lower-cased; nil for a blank name.
func rosterForms(e RosterEntry) []string {
	name := strings.ToLower(strings.TrimSpace(e.Name))
	if name == "" {
		return nil
	}
	out := []string{name}
	for _, f := range e.Aliases {
		if f = strings.ToLower(strings.TrimSpace(f)); len(f) >= 3 {
			out = append(out, f)
		}
	}
	return out
}

// withoutForms blanks every occurrence in text of a form of longer that
// contains one of shorter's - so "centurion primus restun" isn't also
// counted as a mention of "primus restun".
func withoutForms(text string, shorter, longer []string) string {
	for _, l := range longer {
		for _, s := range shorter {
			if l != s && strings.Contains(l, s) {
				text = strings.ReplaceAll(text, l, strings.Repeat("\x00", len(l)))
				break
			}
		}
	}
	return text
}

// spread picks up to n of hits, the first always, the rest evenly spaced:
// an introduction, then how the name is used later on.
func spread(hits []int, n int) []int {
	if len(hits) <= n {
		return hits
	}
	out := make([]int, 0, n)
	for i := range n {
		out = append(out, hits[i*len(hits)/n])
	}
	return out
}

// PairMentions is where a book names the two entries of a pair, as
// indexes into the paragraphs its IdentityIndex was made from.
type PairMentions struct {
	// Both name the two in one paragraph, OnlyA and OnlyB one of them, by
	// name or alias.
	Both, OnlyA, OnlyB []int
	// NameA/NameB are the paragraphs writing one's own name (not an alias)
	// and no form of the other's: how the book uses that name.
	NameA, NameB []int
	// WrittenA/WrittenB count the paragraphs writing each one's own name
	// at all, other than as part of the other's.
	WrittenA, WrittenB int
}

// PairMentions finds the book's paragraphs naming a and b (by name or
// alias, as whole words). A name that contains the other's ("Centurion
// Primus Restun", "Primus Restun") counts only as itself.
func (ix *IdentityIndex) PairMentions(a, b RosterEntry) PairMentions {
	aForms, bForms := rosterForms(a), rosterForms(b)
	if len(aForms) == 0 || len(bForms) == 0 {
		return PairMentions{}
	}
	var m PairMentions
	for i, t := range ix.lower {
		tA, tB := withoutForms(t, aForms, bForms), withoutForms(t, bForms, aForms)
		hasA, hasB := mentions(aForms, tA), mentions(bForms, tB)
		if hasA && mentions(aForms[:1], tA) {
			m.WrittenA++
			if !hasB {
				m.NameA = append(m.NameA, i)
			}
		}
		if hasB && mentions(bForms[:1], tB) {
			m.WrittenB++
			if !hasA {
				m.NameB = append(m.NameB, i)
			}
		}
		switch {
		case hasA && hasB:
			m.Both = append(m.Both, i)
		case hasA:
			m.OnlyA = append(m.OnlyA, i)
		case hasB:
			m.OnlyB = append(m.OnlyB, i)
		}
	}
	return m
}

// Passages picks what to show of m, a's and b's mentions in this book: up
// to pairPassagesEach paragraphs naming both, and as many naming only a
// and only b, each cut to pairPassageChars around the name.
func (ix *IdentityIndex) Passages(m PairMentions, a, b RosterEntry) PairPassages {
	aForms, bForms := rosterForms(a), rosterForms(b)
	cut := func(hits []int, forms []string) []string {
		var out []string
		for _, i := range spread(hits, pairPassagesEach) {
			text := ix.paragraphs[i]
			for _, f := range forms {
				if re := wordRegexp(f); re != nil && re.MatchString(text) {
					text = centred(text, re, pairPassageChars)
					break
				}
			}
			out = append(out, text)
		}
		return out
	}
	return PairPassages{Both: cut(m.Both, append(aForms, bForms...)), OnlyA: cut(m.OnlyA, aForms), OnlyB: cut(m.OnlyB, bForms), NameA: m.WrittenA, NameB: m.WrittenB}
}

// PairPassages is Passages of PairMentions: the book's passages for a and
// b.
func (ix *IdentityIndex) PairPassages(a, b RosterEntry) PairPassages {
	return ix.Passages(ix.PairMentions(a, b), a, b)
}

// UnwrittenName is the fix for a pair that shares its words when the book
// never writes one of the names on its own - "Primus Restun" only ever as
// part of "Centurion Primus Restun", or "Centurion Restun" not at all: a
// name attribution made up for someone the roster already has. ok is false
// when both names are written (the text has to be read), when p is an alias
// pair, and when neither is written unless the two differ only by rank
// ("Legionnaire Daralen", "Primus Daralen") - two names the text spells
// some other way say nothing about each other.
func (p RosterPair) UnwrittenName(found PairPassages) (RosterFix, bool) {
	wa, wb := rosterNameWords(p.A.Name, p.titles), rosterNameWords(p.B.Name, p.titles)
	switch {
	case p.Alias != "" || found.NameA > 0 && found.NameB > 0:
		return RosterFix{}, false
	case found.NameA == 0 && found.NameB == 0 && (!subsetOf(wa, wb) || !subsetOf(wb, wa)):
		return RosterFix{}, false
	case found.NameB == 0:
		return RosterFix{Kind: RosterMerge, Subject: p.B, Target: p.A, Reason: fmt.Sprintf("the book never writes %q on its own", p.B.Name)}, true
	default:
		return RosterFix{Kind: RosterMerge, Subject: p.A, Target: p.B, Reason: fmt.Sprintf("the book never writes %q on its own", p.A.Name)}, true
	}
}

// SamePersonVerdict is JudgeSamePerson's answer.
type SamePersonVerdict string

const (
	VerdictSame      SamePersonVerdict = "same"
	VerdictDifferent SamePersonVerdict = "different"
	VerdictUnclear   SamePersonVerdict = "unclear"
)

// samePersonSystemPrompt asks whether two roster entries are one person,
// from passages PairPassages found. The evidence comes before the verdict
// so the model reads before it answers.
const samePersonSystemPrompt = `You are checking a novel's cast list for duplicates. You will be given two entries from the list - each a name, with any other names it goes by, its pronouns and how many lines of dialogue it has - and passages from the book that mention them. Decide whether the two entries are one and the same person.

Answer "same" only when a passage makes it clear: it uses both names for one person, gives one name the other's title, rank or surname ("Centurion Primus Restun saluted. 'Sit,' Restun said"), says one is also called the other, or otherwise leaves no doubt. Answer "different" when a passage shows two people: they speak to or about each other, are told apart ("the Baron and his son, the Prince"), or are of different sexes, kinds or ranks at the same time. Sharing a surname, a title, a family or a scene is not enough for "same" - relatives share surnames, and many people hold one rank. If the passages don't settle it, answer "unclear".

Reply with a compact JSON object only, no other text:

{"evidence": "<the words from one passage that show it, copied exactly, or empty>", "verdict": "same" | "different" | "unclear"}`

type samePersonRaw struct {
	Evidence string `json:"evidence"`
	Verdict  string `json:"verdict"`
}

const samePersonMaxTokens = 384

// describeEntry is one entry as samePersonSystemPrompt's user turn lists
// it.
func describeEntry(e RosterEntry) string {
	var notes []string
	if len(e.Aliases) > 0 {
		notes = append(notes, "also called "+strings.Join(e.Aliases, ", "))
	}
	if e.Pronouns != "" {
		notes = append(notes, e.Pronouns)
	}
	if e.IsRole {
		notes = append(notes, "a label for a kind of person")
	}
	notes = append(notes, fmt.Sprintf("%d lines", e.Lines))
	return fmt.Sprintf("%q (%s)", e.Name, strings.Join(notes, "; "))
}

// JudgeSamePerson asks whether a and b are one person, given passages.
// VerdictSame and VerdictDifferent come back only with evidence found word
// for word in a passage (returned with it) - and VerdictSame only when
// that passage names both: shown "I am Othair, my lord" the model called
// Othair, a butler, the same as the god Darakin. When the book uses the
// two names alike (PairPassages.Alike) a passage naming either will do,
// and VerdictSame stands without one too, its evidence then empty: the
// model mostly answers such a pair with words from two passages run
// together, or its reasons. Anything else is VerdictUnclear.
func (c *Client) JudgeSamePerson(ctx context.Context, bookTitle string, a, b RosterEntry, passages PairPassages) (SamePersonVerdict, string, error) {
	all := passages.All()
	if len(all) == 0 {
		return VerdictUnclear, "", nil
	}
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\n\nEntry A: %s\nEntry B: %s\n\nPassages:\n", bookTitle, describeEntry(a), describeEntry(b))
	for i, s := range all {
		fmt.Fprintf(&user, "[%d] %s\n", i+1, oneLine(s))
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}
	raw, err := generateAndParse(ctx, c, samePersonSystemPrompt, user.String(), 0, samePersonMaxTokens, func(content string) (samePersonRaw, error) {
		value, found := firstJSONValue(content, '{')
		if !found {
			return samePersonRaw{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
		}
		var r samePersonRaw
		if err := json.Unmarshal([]byte(value), &r); err != nil {
			return samePersonRaw{}, fmt.Errorf("parse JSON object: %w", err)
		}
		return r, nil
	})
	if err != nil {
		return VerdictUnclear, "", err
	}
	verdict := SamePersonVerdict(strings.ToLower(strings.TrimSpace(raw.Verdict)))
	if verdict != VerdictSame && verdict != VerdictDifferent {
		return VerdictUnclear, "", nil
	}
	backed := false
	for _, s := range all {
		if !evidenceFound(raw.Evidence, []string{s}) {
			continue
		}
		lower := strings.ToLower(s)
		if verdict == VerdictDifferent || passages.Alike || mentions(rosterForms(a), lower) && mentions(rosterForms(b), lower) {
			backed = true
			break
		}
	}
	if !backed {
		if verdict == VerdictSame && passages.Alike {
			return VerdictSame, "", nil
		}
		return VerdictUnclear, "", nil
	}
	return verdict, strings.TrimSpace(raw.Evidence), nil
}
