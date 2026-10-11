package speakerattr

import (
	"regexp"
	"slices"
	"sort"
	"strings"
)

// originWord matches a nationality, language, or accent word - what an
// origin clue must contain. Demonyms double as language names ("Danish").
var originWord = regexp.MustCompile(`\b(?:` + strings.Join([]string{
	"English", "British", "Briton", "Scottish", "Scots?", "Irish", "Welsh", "Cockney", "Geordie", "Scouse", "Yorkshire", "Londoner",
	"American", "Texan", "Canadian", "Australian", "Aussie", "Kiwi", "New Zealander",
	"Danish", "Danes?", "Swedish", "Swedes?", "Norwegian", "Finnish", "Finns?", "Icelandic", "Dutch", "Flemish", "Belgian",
	"German", "Austrian", "Swiss", "French", "Parisian", "Italian", "Spanish", "Spaniard", "Portuguese", "Catalan",
	"Greek", "Russian", "Polish", "Czech", "Slovak(?:ian)?", "Ukrainian", "Hungarian", "Romanian", "Bulgarian", "Serbian", "Croatian", "Albanian",
	"Turkish", "Arab(?:ic)?", "Egyptian", "Persian", "Iranian", "Israeli", "Hebrew", "Lebanese", "Moroccan",
	"Indian", "Pakistani", "Bengali", "Chinese", "Cantonese", "Mandarin", "Japanese", "Korean", "Vietnamese", "Thai", "Filipino", "Indonesian",
	"Mexican", "Brazilian", "Argentin(?:e|ian)", "Colombian", "Cuban", "Jamaican", "Puerto Rican", "Latin American",
	"African", "Nigerian", "Kenyan", "Ethiopian", "Somali", "Ghanaian",
	"accents?", "accented", "brogue", "drawl", "lilt", "dialect",
}, "|") + `)\b`)

// OriginParagraphs keeps the paragraphs (a whole book) that contain an
// origin word - the small set OriginClues searches, cached per book by
// its caller.
func OriginParagraphs(texts []string) []string {
	var out []string
	for _, t := range texts {
		if originWord.MatchString(t) {
			out = append(out, t)
		}
	}
	return out
}

// originClueDistance is how close (in characters) a name and an origin
// word must sit to count as a clue: "Ingrid, a Danish archeologist",
// "Danish muttering coming from Ingrid".
const originClueDistance = 120

// originTitleWords never stand for a character on their own ("Lord
// Ballast" is never just "Lord").
var originTitleWords = map[string]bool{"mr": true, "mrs": true, "ms": true, "miss": true, "lord": true, "lady": true, "sir": true, "dame": true, "master": true, "mistress": true, "captain": true, "lieutenant": true, "sergeant": true, "constable": true, "count": true, "duke": true, "duchess": true, "king": true, "queen": true, "prince": true, "princess": true, "doctor": true, "dr": true, "professor": true, "the": true, "old": true, "young": true}

// OriginClues returns up to limit sentences from paragraphs (see
// OriginParagraphs) where one of names - a character's name and aliases,
// plus a multi-word name's first word ("Lars" for "Lars Machmüller") -
// sits within originClueDistance characters of a nationality, language or
// accent word, closest first. CharacterizeVoice shows them as "Origin
// clues": a sample of their lines rarely holds them, so "Ingrid, a Danish
// archeologist" and "Lars whispered in Danish" never reached
// characterization, which cast them as a British woman and a German man.
// shared is as in clueNames.
func OriginClues(paragraphs, names, shared []string, limit int) []string {
	nameRes := clueNames(names, shared)
	if len(nameRes) == 0 {
		return nil
	}
	type clue struct {
		text string
		dist int
	}
	var clues []clue
	for _, p := range paragraphs {
		if dist, span := nearestToName(p, nameRes, originWord.FindAllStringIndex(p, -1), false); dist <= originClueDistance {
			clues = append(clues, clue{sentencesAround(p, span[0], span[1]), dist})
		}
	}
	sort.SliceStable(clues, func(i, j int) bool { return clues[i].dist < clues[j].dist })
	var out []string
	dup := map[string]bool{}
	for _, c := range clues {
		if len(out) == limit {
			break
		}
		if !dup[c.text] {
			dup[c.text] = true
			out = append(out, c.text)
		}
	}
	return out
}

// clueNames compiles a whole-word matcher for each of names - a
// character's name and aliases, plus a multi-word name's first word -
// skipping ones under 3 letters and bare title words. A first word in
// shared (lower case: what the rest of the roster is called, see
// SharedNames) is someone else's name too and is left out: "Thomas, an
// eight-year-old boy" cast Thomas Oh, the lead's father, as a child.
func clueNames(names, shared []string) []*regexp.Regexp {
	var nameRes []*regexp.Regexp
	seen := map[string]bool{}
	addName := func(n string) {
		n = strings.TrimSpace(n)
		if len([]rune(n)) < 3 || seen[strings.ToLower(n)] || originTitleWords[strings.ToLower(n)] {
			return
		}
		seen[strings.ToLower(n)] = true
		nameRes = append(nameRes, regexp.MustCompile(`\b`+regexp.QuoteMeta(n)+`\b`))
	}
	for _, n := range names {
		addName(n)
		if words := strings.Fields(n); len(words) > 1 && !slices.Contains(shared, strings.ToLower(words[0])) {
			addName(words[0])
		}
	}
	return nameRes
}

// SharedNames returns, in lower case, every name in others (the names and
// aliases of the rest of a character's roster) and each one's first word:
// the first names a character can't be told by (clueNames).
func SharedNames(others []string) []string {
	var out []string
	for _, n := range others {
		words := strings.Fields(strings.ToLower(n))
		if len(words) == 0 {
			continue
		}
		out = append(out, strings.Join(words, " "))
		if len(words) > 1 {
			out = append(out, words[0])
		}
	}
	return out
}

// nearestToName returns the smallest distance in characters between a
// match of nameRes in p and one of hits (match index pairs), and the span
// covering both; the distance is originClueDistance+1 when there is no
// pair. With nameFirst only a hit after the name counts. A pair with a
// sentence end between the two never counts: "West stood in the middle,
// grinning widely. The huge Nigerian..." is two people, and cast Captain
// West, a Scot, with a Nigerian accent and the other man's size.
func nearestToName(p string, nameRes []*regexp.Regexp, hits [][]int, nameFirst bool) (dist int, span [2]int) {
	dist = originClueDistance + 1
	if len(hits) == 0 {
		return dist, span
	}
	for _, re := range nameRes {
		for _, nm := range re.FindAllStringIndex(p, -1) {
			for _, h := range hits {
				if nameFirst && h[0] < nm[1] {
					continue
				}
				if lo, hi := min(nm[1], h[1]), max(nm[0], h[0]); lo < hi && sentenceSplit.MatchString(p[lo:hi]) {
					continue
				}
				if d := max(h[0]-nm[1], nm[0]-h[1], 0); d < dist {
					dist, span = d, [2]int{min(nm[0], h[0]), max(nm[1], h[1])}
				}
			}
		}
	}
	return dist, span
}

// Excerpt returns the sentences of text around the first occurrence of
// term (in any case), or its start when term isn't found, at most maxChars
// long - what a lookup shows of a matching paragraph.
func Excerpt(text, term string, maxChars int) string {
	start, end := 0, 0
	if i := strings.Index(strings.ToLower(text), strings.ToLower(term)); i >= 0 {
		start, end = i, i+len(term)
	}
	out := sentencesAround(text, start, min(end, len(text)))
	if len(out) > maxChars {
		out = out[:maxChars] + "..."
	}
	return out
}

// sentencesAround widens [start, end) of text to the sentences containing
// it, capped at 400 characters.
func sentencesAround(text string, start, end int) string {
	from := 0
	for _, m := range sentenceSplit.FindAllStringIndex(text[:start], -1) {
		from = m[1]
	}
	to := len(text)
	if m := sentenceSplit.FindStringIndex(text[end:]); m != nil {
		to = end + m[0] + 1
	}
	out := oneLine(text[from:min(to, len(text))])
	if len(out) > 400 {
		out = out[:400] + "..."
	}
	return out
}
