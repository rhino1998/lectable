package speakerattr

import (
	"regexp"
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
// clues": the description tagger only marks appearance/personality
// narration, so "Ingrid, a Danish archeologist" and "Lars whispered in
// Danish" never reached characterization, which cast them as a British
// woman and a German man.
func OriginClues(paragraphs, names []string, limit int) []string {
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
		if words := strings.Fields(n); len(words) > 1 {
			addName(words[0])
		}
	}
	if len(nameRes) == 0 {
		return nil
	}
	type clue struct {
		text string
		dist int
	}
	var clues []clue
	for _, p := range paragraphs {
		origins := originWord.FindAllStringIndex(p, -1)
		best, bestSpan := originClueDistance+1, [2]int{}
		for _, re := range nameRes {
			for _, nm := range re.FindAllStringIndex(p, -1) {
				for _, o := range origins {
					d := max(o[0]-nm[1], nm[0]-o[1], 0)
					if d < best {
						best, bestSpan = d, [2]int{min(nm[0], o[0]), max(nm[1], o[1])}
					}
				}
			}
		}
		if best <= originClueDistance {
			clues = append(clues, clue{sentencesAround(p, bestSpan[0], bestSpan[1]), best})
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
