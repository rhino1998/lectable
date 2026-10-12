package speakerattr

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/rhino1998/lectable/backend/internal/namekey"
)

// WorldWords is the words of one story's own world that are never a
// person's name, each with what it is (store.WordClass): a title
// ("Prognugator", "Ser"), a role - a job or class ("Baker",
// "Guardsman") -, a race ("Drake", "Gnoll"), a nationality
// ("Pallassian") or a place ("Pallass") - what the reader recorded beside the fixed titles every
// book shares (titleWords, rosterRankWords). Without them such a word
// reads as part of a name: "Prognugator" was a character of The Wandering
// Inn holding Tersk, Wrymvr and Xrn as aliases, beside "Prognugator Tersk"
// and "Prognugator Xrn". nil is no words of the world's own. An entry can
// be several words ("Wall Lord", "Drowned Folk"): it is then matched as
// that phrase, and its words alone mean nothing ("Wall" is not a title).
type WorldWords map[string]string

// The classes a WorldWords word can have - store.WordClass's values.
const (
	WordTitle       = "title"
	WordRole        = "role"
	WordRace        = "race"
	WordNationality = "nationality"
	WordPlace       = "place"
)

// NewWorldWords makes WorldWords of classes: word (any case) -> class.
func NewWorldWords(classes map[string]string) WorldWords {
	if len(classes) == 0 {
		return nil
	}
	t := WorldWords{}
	for w, class := range classes {
		if w = strings.Join(strings.Fields(strings.ToLower(w)), " "); w != "" && class != "" {
			t[w] = class
		}
	}
	return t
}

// NewTitles makes WorldWords of words that are all titles.
func NewTitles(words []string) WorldWords {
	classes := map[string]string{}
	for _, w := range words {
		classes[w] = WordTitle
	}
	return NewWorldWords(classes)
}

// Has reports whether w names no one on its own: one of the fixed titles
// (IsTitleWord) or one of this world's words.
func (t WorldWords) Has(w string) bool {
	w = strings.Join(strings.Fields(strings.ToLower(w)), " ")
	return titleWords[w] || t[w] != ""
}

// rank reports whether w, lower-cased, is a word two different people can
// share - Has, or one of rosterRankWords.
func (t WorldWords) rank(w string) bool {
	return titleWords[w] || rosterRankWords[w] || t[w] != "" || t.people(w)
}

// people reports whether w, lower-cased, says what sort of person someone
// is - a role, a race or a nationality (a plain plural included) - rather
// than who they are or what office they hold.
func (t WorldWords) people(w string) bool {
	class := t[w]
	if class == "" {
		class = t[strings.TrimSuffix(w, "s")]
	}
	return class == WordRole || class == WordRace || class == WordNationality
}

// withoutPhrases returns lower - a name's words, lower-cased and joined by
// single spaces - with every entry of several words taken out (a plain
// plural too: "wall lords"), the longest first, and whether any was. keep,
// when not nil, says which classes' entries to take.
func (t WorldWords) withoutPhrases(lower string, keep func(class string) bool) (string, bool) {
	var phrases []string
	for w, class := range t {
		if strings.Contains(w, " ") && (keep == nil || keep(class)) {
			phrases = append(phrases, w)
		}
	}
	if len(phrases) == 0 {
		return lower, false
	}
	sort.Slice(phrases, func(i, j int) bool {
		if len(phrases[i]) != len(phrases[j]) {
			return len(phrases[i]) > len(phrases[j])
		}
		return phrases[i] < phrases[j]
	})
	padded, found := " "+lower+" ", false
	for _, p := range phrases {
		for _, form := range []string{" " + p + " ", " " + p + "s "} {
			if strings.Contains(padded, form) {
				padded, found = strings.ReplaceAll(padded, form, " "), true
			}
		}
	}
	return strings.TrimSpace(padded), found
}

// IsPeopleDescription reports whether name is made only of roles, races
// and nationalities, with at most words saying which of several ("Drake",
// "Pallassian Gnoll", "Drake Trader", "One of the Gnolls"): a kind of
// person rather than a person. A title among its words doesn't count:
// "Gnoll Chieftain" may be the one the scene has.
func (t WorldWords) IsPeopleDescription(name string) bool {
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' && r != '’' && r != '-' })
	rest, found := t.withoutPhrases(strings.Join(words, " "), func(class string) bool {
		return class == WordRole || class == WordRace || class == WordNationality
	})
	for _, w := range strings.Fields(rest) {
		switch {
		case t.people(w):
			found = true
		case notTitleLeads[w] || nameConnectors[w] || len([]rune(w)) < 3:
		default:
			return false
		}
	}
	return found
}

// TitleCandidate is a word that may be a title of the story's world: it
// leads the names of several roster entries.
type TitleCandidate struct {
	// Word as the names write it: "Prognugator".
	Word string
	// Holders are the entries it leads, most lines first: "Prognugator
	// Tersk", "Prognugator Dekass".
	Holders []string
}

const (
	// MinTitleHolders is how many different people a word must lead the
	// names of to be a candidate: one "Prognugator Tersk" could be a first
	// name and a surname.
	MinTitleHolders = 2
	// MinTitleUses is how many paragraphs must write a candidate after an
	// article ("the Prognugator", TitleUses) for it to be proposed: a
	// family name written first ("Zhao Wei", "Zhao Na") leads several
	// names too, but nobody is "the Zhao". Five, since a first name two
	// people share turns up this way now and then ("Joe dated a Margaret
	// in high school", three times in Expeditionary Force).
	MinTitleUses = 5
)

// notTitleLeads are words that lead names without being a title: which of
// several ("First Gardener", "Senior Guardsman Yill"), or what sort
// ("Female Gnoll").
var notTitleLeads = map[string]bool{
	"first": true, "second": true, "third": true, "senior": true, "junior": true, "female": true, "male": true,
	"other": true, "another": true, "new": true, "former": true, "dead": true, "one": true, "two": true, "three": true,
}

// TitleCandidates returns the words that lead the names of at least
// MinTitleHolders entries who aren't roles, numbered or possessive -
// "Prognugator" of "Prognugator Tersk" and "Prognugator Dekass" - and
// aren't titles already (the fixed ones, or known), the word with the most
// holders first. A name whose second word is a title is left out: of "Wall
// Lord Ilvriss" the title is "Wall Lord", and "Wall" alone is a wall. So
// is one whose remainder is a role on the roster ("Female Goblin" beside
// the role "Goblin"): a description, not a title and a name. Whether the
// text uses a candidate as a title is TitleUses' to say, and whether what
// follows it is a name NamedHolders'.
func TitleCandidates(entries []RosterEntry, known WorldWords) []TitleCandidate {
	sorted := append([]RosterEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Lines > sorted[j].Lines })
	roles := map[string]bool{}
	for _, e := range entries {
		if e.IsRole {
			roles[strings.ToLower(strings.TrimSpace(e.Name))] = true
		}
	}
	byWord := map[string]*TitleCandidate{}
	rests := map[string]map[string]bool{}
	var order []string
	for _, e := range sorted {
		if e.IsRole || namedByAnother(e.Name) || strings.ContainsAny(e.Name, "[]") {
			continue
		}
		words := strings.Fields(e.Name)
		if len(words) < 2 {
			continue
		}
		word, lower := words[0], strings.ToLower(words[0])
		next := strings.ToLower(strings.Trim(words[1], ".,"))
		if len([]rune(word)) < 3 || !startsUpper(word) || strings.IndexFunc(word, func(r rune) bool { return !unicode.IsLetter(r) }) >= 0 {
			continue
		}
		if known.rank(lower) || notTitleLeads[lower] || nameConnectors[lower] || known.rank(next) || nameConnectors[next] || !startsUpper(words[1]) {
			continue
		}
		// One person under two spellings ("Ve'delina", "Ve’delina") is
		// one holder.
		rest := namekey.Fold(strings.Join(words[1:], " "))
		if roles[rest] {
			continue
		}
		if rests[lower] == nil {
			rests[lower] = map[string]bool{}
			byWord[lower] = &TitleCandidate{Word: word}
			order = append(order, lower)
		}
		if !rests[lower][rest] {
			rests[lower][rest] = true
			byWord[lower].Holders = append(byWord[lower].Holders, e.Name)
		}
	}
	var out []TitleCandidate
	for _, w := range order {
		if len(byWord[w].Holders) >= MinTitleHolders {
			out = append(out, *byWord[w])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Holders) > len(out[j].Holders) })
	return out
}

// WordCandidate is a word of the roster's names that may be a title, a
// race or a nationality of the story's world, for ClassifyWords to say.
type WordCandidate struct {
	// Word as the names write it: "Drake".
	Word string
	// Names are the entries using it, most lines first: "Drake Tesy",
	// "Pallassian Drake", "One of the Drakes".
	Names []string
}

// minWordNames is how many entries must use a word for it to be a
// WordCandidate.
const minWordNames = 2

// WordCandidates returns the capitalized words that at least minWordNames
// entries' names use, anywhere in the name and roles included ("Drake" of
// "Drake Tesy", "Pallassian Drake" and the role "Drake"; a plain plural
// counts as its singular), the phrases of two or three words that lead
// several names ("Wall Lord"), name a people ("Drowned Folk") or follow
// "of" ("Blighted Lands"), and a single word that is all that follows
// "of" in even one name ("Healer of Tenbault"), that aren't titles already (the fixed
// ones, or known) - the word most names use first. Wider than
// TitleCandidates on purpose: it takes in family names and jobs too, and
// leaves telling them apart to TitleUses and then ClassifyWords.
func WordCandidates(entries []RosterEntry, known WorldWords) []WordCandidate {
	sorted := append([]RosterEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Lines > sorted[j].Lines })
	type use struct {
		word  string
		names []string
	}
	uses := map[string]*use{}
	// after are the words that are all that follows "of"/"from" in some
	// name - a place, most likely ("Healer of Tenbault"), for which one
	// name is enough.
	after := map[string]bool{}
	var order []string
	for _, e := range sorted {
		seen := map[string]bool{}
		for _, w := range strings.FieldsFunc(e.Name, func(r rune) bool { return !unicode.IsLetter(r) && r != '-' }) {
			lower := strings.ToLower(w)
			if len([]rune(w)) < 3 || !startsUpper(w) || known.rank(lower) || known.rank(strings.TrimSuffix(lower, "s")) || notTitleLeads[lower] || nameConnectors[lower] || lower == "role" || seen[lower] {
				continue
			}
			seen[lower] = true
			if uses[lower] == nil {
				uses[lower] = &use{word: w}
				order = append(order, lower)
			}
			uses[lower].names = append(uses[lower].names, e.Name)
			if placed(e.Name, w) {
				after[lower] = true
			}
		}
	}
	// A phrase of two or three capitalized words is a candidate when it
	//   - leads several names, each going on differently: "Wall Lord" of
	//     "Wall Lord Ilvriss" and "Wall Lord Brilm" ("Lady Magnolia" leads
	//     only "Lady Magnolia Reinhart");
	//   - is used by several entries, one of them a role or a description
	//     - a people: "Drowned Folk" of "Drowned Folk Captain" and "One of
	//     the Drowned Folk" (a person's two names turn up in two entries
	//     too, but both are names);
	//   - or is all that follows "of"/"from" in a name - a place: "Blighted
	//     Lands" of "King of the Blighted Lands".
	type phrase struct {
		use
		rests     map[string]bool
		leading   []string
		described bool
		after     bool
	}
	phrases := map[string]*phrase{}
	var phraseOrder []string
	capital := func(w string) bool {
		return startsUpper(w) && strings.IndexFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && r != '-' && r != '\'' && r != '’' }) < 0
	}
	for _, e := range sorted {
		if namedByAnother(e.Name) || strings.ContainsAny(e.Name, "[]") {
			continue
		}
		words := strings.Fields(e.Name)
		described := e.IsRole || !startsUpper(e.Name) || descriptorLead.MatchString(e.Name)
		for i := range words {
			for n := 2; n <= 3 && i+n <= len(words); n++ {
				gram, ranks, ok := words[i:i+n], 0, true
				for _, w := range gram {
					lower := strings.ToLower(w)
					if !capital(w) || notTitleLeads[lower] {
						ok = false
					}
					if known.rank(lower) {
						ranks++
					}
				}
				lower := strings.ToLower(strings.Join(gram, " "))
				// Not one made of titles already ("Lord Commander").
				if !ok || ranks == n || known[lower] != "" {
					continue
				}
				if phrases[lower] == nil {
					phrases[lower] = &phrase{use: use{word: strings.Join(gram, " ")}, rests: map[string]bool{}}
					phraseOrder = append(phraseOrder, lower)
				}
				p := phrases[lower]
				if !slices.Contains(p.names, e.Name) {
					p.names = append(p.names, e.Name)
				}
				p.described = p.described || described
				// Leading a proper name: not one going on in lower case
				// ("Queen of the Antinium").
				if i == 0 && n < len(words) && !e.IsRole && startsUpper(words[n]) {
					if rest := strings.ToLower(strings.Join(words[n:], " ")); !p.rests[rest] {
						p.rests[rest] = true
						p.leading = append(p.leading, e.Name)
					}
				}
				// The whole of what follows "of (the)" or "from".
				if i+n == len(words) && i > 0 {
					prev := strings.ToLower(words[i-1])
					if prev == "the" && i > 1 {
						prev = strings.ToLower(words[i-2])
					}
					p.after = p.after || prev == "of" || prev == "from"
				}
			}
		}
	}
	var out []WordCandidate
	for _, lower := range phraseOrder {
		p := phrases[lower]
		switch {
		case len(p.rests) >= minWordNames && len(p.names) == len(p.leading):
			p.names = p.leading
		case len(p.rests) >= minWordNames, p.after, p.described && len(p.names) >= minWordNames:
		default:
			continue
		}
		out = append(out, WordCandidate{Word: p.word, Names: p.names})
		// Its words are no candidates of their own for the names it
		// accounts for: "Wall" is only ever part of "Wall Lord".
		for _, w := range strings.Fields(lower) {
			if u := uses[w]; u != nil {
				u.names = slices.DeleteFunc(u.names, func(n string) bool {
					return strings.Contains(" "+strings.ToLower(strings.Join(strings.Fields(n), " "))+" ", " "+lower+" ")
				})
			}
		}
	}
	// "Gnolls" of "One of the Gnolls" is a use of "Gnoll".
	for _, lower := range order {
		if singular := strings.TrimSuffix(lower, "s"); singular != lower && uses[singular] != nil {
			uses[singular].names = append(uses[singular].names, uses[lower].names...)
			delete(uses, lower)
		}
	}
	for _, lower := range order {
		if u := uses[lower]; u != nil && (len(u.names) >= minWordNames || after[lower] && len(u.names) > 0) {
			out = append(out, WordCandidate{Word: u.word, Names: u.names})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Names) > len(out[j].Names) })
	return out
}

// placed reports whether word is the last word of name and all that
// follows "of (the)" or "from" in it: "Tenbault" of "Healer of Tenbault".
func placed(name, word string) bool {
	words := strings.Fields(strings.ToLower(name))
	n := len(words)
	if n < 3 || strings.Trim(words[n-1], ".,") != strings.ToLower(word) {
		return false
	}
	prev := words[n-2]
	if prev == "the" {
		prev = words[n-3]
	}
	return prev == "of" || prev == "from"
}

// HolderWords is the word after the candidate in each of its holders'
// names ("Tersk" of "Prognugator Tersk"): what NamedHolders needs counted
// in the text (WordCaseCounts).
func (c TitleCandidate) HolderWords() []string {
	out := make([]string, 0, len(c.Holders))
	for _, h := range c.Holders {
		if words := strings.Fields(h); len(words) > 1 {
			out = append(out, strings.Trim(words[1], ".,"))
		}
	}
	return out
}

// NamedHolders is the holders whose name goes on with a name - a word the
// text doesn't mostly write in lower case (counts over HolderWords). A
// kind leads names too, but what follows it is an ordinary word: "Drake
// Trader", "Gnoll Bartender", "First Gardener". A candidate left with
// fewer than MinTitleHolders is no title.
func (c TitleCandidate) NamedHolders(counts WordCaseCounts) []string {
	var out []string
	for _, h := range c.Holders {
		if words := strings.Fields(h); len(words) > 1 && !counts.MostlyLower(strings.Trim(words[1], ".,")) {
			out = append(out, h)
		}
	}
	return out
}

// NamesUsing is the names among names that have word (or its plain
// plural) as one of their words beside others - "Prognugator" ->
// "Prognugator Tersk", "Gnoll" -> "One of the Gnolls" - in names' order.
func NamesUsing(word string, names []string) []string {
	word = strings.Join(strings.Fields(strings.ToLower(word)), " ")
	phrase := strings.Contains(word, " ")
	var out []string
	for _, n := range names {
		words := strings.FieldsFunc(strings.ToLower(n), func(r rune) bool { return !unicode.IsLetter(r) && r != '-' })
		if len(words) < 2 {
			continue
		}
		if phrase {
			if joined := " " + strings.Join(words, " ") + " "; joined != " "+word+" " && (strings.Contains(joined, " "+word+" ") || strings.Contains(joined, " "+word+"s ")) {
				out = append(out, n)
			}
			continue
		}
		for _, w := range words {
			if w == word || w == word+"s" {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// TitleUses counts, for a few candidate words, the paragraphs that write
// each the way a title, a people or a place is written and a person's
// name seldom is: after an article or the like ("the Prognugator", "a
// Prognugator", "his Prognugators") or a preposition of place ("in
// Pallass", "the walls of Liscor" - never "to", which is said to people). Not
// after "this" or "that": "it was true that Tekshia Shivertail was..."
// made a title of a first name two surnames share.
type TitleUses map[string]*TitleUse

// TitleUse is one word's count and the first sentence that used it so.
type TitleUse struct {
	re *regexp.Regexp
	// Paragraphs is how many paragraphs use the word as a title.
	Paragraphs int
	// Evidence is the first such sentence, and EvidenceAt what Add was
	// told of where its paragraph is.
	Evidence   string
	EvidenceAt string
	// Samples are up to maxTitleSamples such sentences, Evidence first,
	// then ones from later on (every tenth use, so they aren't all from
	// one scene).
	Samples []string
}

// maxTitleSamples is how many sentences TitleUse.Samples keeps.
const maxTitleSamples = 3

// NewTitleUses prepares counts for words (any case).
func NewTitleUses(words []string) TitleUses {
	u := TitleUses{}
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		u[strings.Join(strings.Fields(strings.ToLower(w)), " ")] = &TitleUse{
			re: regexp.MustCompile(`(?i)\b(?:the|a|an|another|each|every|no|our|my|your|his|her|its|their|new|former|in|from|of|at|near|across|throughout)\s+` + strings.Join(strings.Fields(regexp.QuoteMeta(w)), `\s+`) + `s?\b`),
		}
	}
	return u
}

// Add counts one paragraph's uses of the words; at says where it is (a
// chapter id).
func (u TitleUses) Add(at, text string) {
	for word, use := range u {
		// The regexp is the slow part on a long series; most paragraphs
		// don't have the word at all.
		// (For a phrase, its first word: the spacing may differ.)
		if first, _, _ := strings.Cut(word, " "); !containsFold(text, first) {
			continue
		}
		loc := use.re.FindStringIndex(text)
		if loc == nil {
			continue
		}
		if use.Paragraphs == 0 {
			use.Evidence, use.EvidenceAt = sentenceAround(text, loc[0], loc[1]), at
		}
		if use.Paragraphs%10 == 0 && len(use.Samples) < maxTitleSamples {
			use.Samples = append(use.Samples, sentenceAround(text, loc[0], loc[1]))
		}
		use.Paragraphs++
	}
}

// Of is word's count, nil when it wasn't one of the words.
func (u TitleUses) Of(word string) *TitleUse {
	return u[strings.Join(strings.Fields(strings.ToLower(word)), " ")]
}

// containsFold reports whether text contains lower, a lower-case word, in
// any case.
func containsFold(text, lower string) bool {
	return strings.Contains(strings.ToLower(text), lower)
}

// sentenceAround is the sentence of text holding [start, end), on one
// line.
func sentenceAround(text string, start, end int) string {
	isEnd := func(r rune) bool { return r == '.' || r == '!' || r == '?' || r == '\n' }
	from := strings.LastIndexFunc(text[:start], isEnd) + 1
	to := len(text)
	if i := strings.IndexFunc(text[end:], isEnd); i >= 0 {
		to = end + i + 1
	}
	return FactPassage(strings.TrimSpace(text[from:to]))
}
