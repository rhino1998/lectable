package speakerattr

import (
	"regexp"
	"strings"
)

// CharacterEvidence is the material CharacterizeVoice reads, each part gathered by its own search across the character's
// series.
type CharacterEvidence struct {
	// Quotes are their own lines, spread over their whole run.
	Quotes []QuoteContext
	// Introductions are the first narration paragraphs naming them.
	Introductions []string
	// Traits are narration sentences stating something of them (see
	// TraitStatements).
	Traits []string
	// VoiceClues and Origins are VoiceClues' and OriginClues' sentences.
	VoiceClues []string
	Origins    []string
	// StatedAges are their own lines giving their age (see StatedAges).
	StatedAges []string
	// WorldNotes are recorded facts about the story's world that this
	// material brings up, as "key: fact" lines (see MatchWorldNotes).
	WorldNotes []string
	// Idioms counts the dialect-marking words in every line they speak,
	// not only Quotes: of Jax's 3786 lines 99 say "mate" and 7 "aye", and
	// a sample that happened to hold an "aye" cast him Scottish.
	Idioms IdiomCounts
}

// idiomLines renders e.Idioms for a prompt, "" when their lines hold
// fewer than minIdiomWords such words - too few to say anything.
func (e CharacterEvidence) idiomLines() string {
	total := 0
	for _, n := range e.Idioms {
		total += n
	}
	if total < minIdiomWords {
		return ""
	}
	return e.Idioms.String()
}

// minIdiomWords is how many dialect-marking words a character's lines
// need before their counts are shown.
const minIdiomWords = 3

// traitSuffixes are what follows a name in a sentence stating a trait of
// them rather than something they did: "Bert was a", "Tallie, a",
// "Sid's voice". Looser ones ("was the", "wasn't", "had a") mostly found
// plot: "Fritz was the last one in", "Fritz had a mind to refuse".
var traitSuffixes = []string{
	" was a ", " was an ", " was only ", " was barely ", " was always ", " had always ",
	" was old", " was young", " was tall", " was short", " was big", " was small", " was huge", " was thin", " was older", " was younger", " was taller", " was shorter",
	" looked like ", " looked every ", " looked no older", " looked about ", " looked to be ",
	", a ", ", an ",
	"'s voice", "’s voice", "'s accent", "’s accent", "'s age", "’s age", "'s build", "’s build", "'s frame", "’s frame",
}

// TraitPhrases returns the phrases to search a book's narration for
// (store.NarrationMatching) to find names' trait statements: each name
// (a character's name and aliases, as clueNames keeps them) followed by
// each of traitSuffixes.
func TraitPhrases(names []string) []string {
	var out []string
	for _, n := range traitNames(names) {
		for _, suffix := range traitSuffixes {
			out = append(out, n+suffix)
		}
	}
	return out
}

// traitNames is names without the ones too short or too generic to
// search for.
func traitNames(names []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if len([]rune(n)) < 3 || seen[strings.ToLower(n)] || originTitleWords[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}
	return out
}

// TraitStatements returns up to limit sentences, evenly spread over
// paragraphs (what the search for TraitPhrases found, in reading order),
// in which one of names as a whole word is followed by one of
// traitSuffixes - the search itself matches substrings in any case, so
// "Bert was a" also finds "Albert was a".
func TraitStatements(paragraphs, names []string, limit int) []string {
	names = traitNames(names)
	if len(names) == 0 {
		return nil
	}
	quoted := func(items []string) string {
		out := make([]string, len(items))
		for i, it := range items {
			out[i] = regexp.QuoteMeta(it)
		}
		return strings.Join(out, "|")
	}
	re := regexp.MustCompile(`\b(?:` + quoted(names) + `)(?:` + quoted(traitSuffixes) + `)`)
	var all []string
	dup := map[string]bool{}
	for _, p := range paragraphs {
		m := re.FindStringIndex(p)
		if m == nil {
			continue
		}
		if s := sentencesAround(p, m[0], m[1]); !dup[s] {
			dup[s] = true
			all = append(all, s)
		}
	}
	if len(all) <= limit {
		return all
	}
	out := make([]string, limit)
	for i := range out {
		out[i] = all[i*len(all)/limit]
	}
	return out
}

// maxIntroductionChars caps one introduction paragraph in the prompt.
const maxIntroductionChars = 600
