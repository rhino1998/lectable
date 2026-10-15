package speakerattr

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// descriptorLead matches the opening of a speaker answer that describes
// someone instead of naming them: "One of the thugs", "another Browncoat",
// "the older man", "a weedy man".
var descriptorLead = regexp.MustCompile(`^(?i:(?:one|another|some|any)\s+of\s+(?:the|his|her|their|those|these)\s+|the\s+|a\s+|an\s+|one\s+|another\s+)`)

// descriptorCut ends a description's head noun phrase: "Guard who grabbed
// Fritz", "Woman with pink hair net", "the third of the women sitting
// there".
var descriptorCut = regexp.MustCompile(`(?i)\s+(?:with|who|whom|that|from|in|at|on|by|near|behind|beyond|through|outside|inside|across|over|under|beside|called|named|he|she|they|i|we|you|it|\w{3,}ing)\b.*$`)

// uniqueTitles are head nouns that name one specific person in a scene -
// "The King", "the old Count" - rather than a kind of person.
var uniqueTitles = map[string]bool{
	"king": true, "queen": true, "prince": true, "princess": true, "duke": true, "duchess": true,
	"count": true, "countess": true, "lord": true, "lady": true, "captain": true, "chief": true,
	"emperor": true, "empress": true, "baron": true, "baroness": true,
}

// roleGroupWords are head nouns naming several people - "the older of
// the pair" is one of them, but which is unknowable.
var roleGroupWords = map[string]bool{"pair": true, "two": true, "three": true, "team": true, "group": true, "crowd": true, "twins": true, "both": true}

// notHeadNouns are words a description can end on that name no kind of
// person - "the other", "the first", "one argued".
var notHeadNouns = map[string]bool{
	"other": true, "first": true, "second": true, "third": true, "last": true, "one": true,
	"another": true, "same": true, "former": true, "latter": true, "rest": true, "voice": true,
	"older": true, "younger": true, "elder": true,
}

// RoleLabel turns a descriptive speaker answer into a role label - its
// head noun, singular and capitalized: "One of the thugs" -> "Thug", "the
// older man" -> "Man", "Guard who grabbed Fritz" -> "Guard", "Woman with
// pink hair net" -> "Woman". ok is false when answer isn't a description
// (a proper name, or "The Nightshark" - a capitalized epithet after "the"
// reads as a name), or when nothing usable is left ("the other", "one
// argued"). isTitle reports a head noun that is a title (King, Count,
// Captain) - usually one specific person, so not a role.
func RoleLabel(answer string) (label string, isTitle, ok bool) {
	s := strings.TrimSpace(answer)
	lead := descriptorLead.FindString(s)
	rest := s[len(lead):]
	isThe := strings.EqualFold(strings.TrimSpace(lead), "the")
	if isThe && (allCapitalized(rest) && !uniqueTitles[strings.ToLower(rest)] || strings.Contains(rest, "'s ") || strings.Contains(rest, "’s ")) {
		// "The Duchess Blackbrine", "The Rat Cleaver", "The Duke's son":
		// a name or one specific person, not a kind.
		return "", false, false
	}
	// A speech verb ends the phrase: "The man inquired loudly".
	words := strings.Fields(rest)
	for i, w := range words {
		if slices.Contains(speechVerbs, strings.ToLower(strings.Trim(w, ".,"))) {
			words = words[:i]
			break
		}
	}
	rest = strings.TrimSpace(descriptorCut.ReplaceAllString(strings.Join(words, " "), ""))
	words = strings.Fields(rest)
	if len(words) == 0 {
		return "", false, false
	}
	if lead == "" && len(words) == len(strings.Fields(s)) && !DescriptorWithoutArticle(s) {
		// No article and nothing cut: a proper name ("Jeb", "Quin
		// Cold") or already a role label ("Guard").
		return "", false, false
	}
	// Brackets mark a class in some books ("One of the [Knights]"); the
	// label is the plain word.
	head := strings.ToLower(strings.Trim(words[len(words)-1], `.,'’"“”!?[]`))
	if head == "" || notHeadNouns[head] || roleGroupWords[head] || slices.Contains(speechVerbs, head) || strings.HasSuffix(head, "ed") {
		return "", false, false
	}
	head = singular(head)
	return capitalizeFirst(head), uniqueTitles[head], true
}

// UnusableDescription reports whether a speaker answer describes someone
// but names neither a person nor a kind RoleLabel can use - "the other",
// "one argued", "the older of the pair" - so it can only be Unknown. A
// possessive ("the king's man") is one specific person, kept.
func UnusableDescription(answer string) bool {
	if _, _, ok := RoleLabel(answer); ok {
		return false
	}
	rest := strings.TrimSpace(descriptorLead.ReplaceAllString(answer, ""))
	return descriptorLead.MatchString(answer) && !startsUpper(rest) && !strings.Contains(rest, "'s ") && !strings.Contains(rest, "’s ")
}

// nameConnectors are lower-case words a proper name can contain ("Lady of
// the Lake", "Ludwig van Beethoven") - see DescriptorWithoutArticle.
var nameConnectors = map[string]bool{
	"of": true, "the": true, "de": true, "del": true, "da": true, "van": true, "von": true,
	"der": true, "den": true, "al": true, "bin": true, "le": true, "la": true, "du": true, "y": true,
}

// DescriptorWithoutArticle reports whether an answer with no article still
// reads as a description rather than a name: all lower case ("woman",
// "doctor", "bearded man"), or a capitalized first word followed by a
// lower-case one ("Older man"). A number anywhere ("Neighbor 1", "Thug 2")
// keeps it a deliberate label for one of several people in a scene, and a
// possessive ("Ingrid's friend") one specific person.
func DescriptorWithoutArticle(s string) bool {
	words := strings.Fields(strings.TrimSpace(s))
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		for _, r := range w {
			if unicode.IsDigit(r) {
				return false
			}
		}
		// "Ingrid's friend", "the Duke's son": one specific person, named
		// by who they belong to.
		if strings.HasSuffix(w, "'s") || strings.HasSuffix(w, "’s") {
			return false
		}
	}
	if !startsUpper(words[0]) {
		return true
	}
	for _, w := range words[1:] {
		if !startsUpper(w) && !nameConnectors[strings.ToLower(w)] {
			return true
		}
	}
	return false
}

// allCapitalized reports whether every word of s starts upper case - a
// name, not a description.
func allCapitalized(s string) bool {
	words := strings.Fields(s)
	for _, w := range words {
		if !startsUpper(w) {
			return false
		}
	}
	return len(words) > 0
}

func singular(w string) string {
	switch {
	case strings.HasSuffix(w, "women"):
		return strings.TrimSuffix(w, "women") + "woman"
	case strings.HasSuffix(w, "men"):
		return strings.TrimSuffix(w, "men") + "man"
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		return strings.TrimSuffix(w, "ies") + "y"
	case strings.HasSuffix(w, "ss"), strings.HasSuffix(w, "us"):
		return w
	// "elves", "dwarves", "wolves" - but "slaves", "knaves" just lose the s.
	case strings.HasSuffix(w, "lves"), strings.HasSuffix(w, "rves"):
		return strings.TrimSuffix(w, "ves") + "f"
	// "witches", "bushes", "boxes", "masses".
	case strings.HasSuffix(w, "ches"), strings.HasSuffix(w, "shes"), strings.HasSuffix(w, "xes"), strings.HasSuffix(w, "sses"):
		return strings.TrimSuffix(w, "es")
	case strings.HasSuffix(w, "s") && len(w) > 3:
		return strings.TrimSuffix(w, "s")
	}
	return w
}

// resolveIdentities keeps attribution to picking someone, never
// inventing a character: an answer that is a known name or an existing
// role label stays; any other descriptive answer ("One of the thugs", "the
// older man", "Guard who grabbed Fritz") becomes a role label (RoleLabel -
// a title like King or Count stays one person rather than a role), and a
// description with no usable head noun ("the other", "one argued")
// Unknown. A proper name the model found in the text passes through as a
// new character. roles gains every role label it produced.
//
// Measured on 10 uncorrected Spire's Spite chapters (999 dialogue lines)
// with the descriptor-named characters stripped from the roster: raw
// attribution invented 43 descriptor-shaped characters on 137 lines; with
// this, 5 on 13, at no model cost. The book's live roster had 149 such
// characters out of 367. A separate LLM identification pass (a chapter
// cast list the attribution batches picked from) was tried first and
// added nothing on top - Qwen3-4B's cast lists invented references,
// leaked prompt examples and wrong pronouns, and cost a call per chapter.
func resolveIdentities(speakers map[int]string, resolver *SpeakerNameResolver, roles map[string]bool) map[int]string {
	// A word of a longer name may fold an answer into that character
	// ("Tomas" -> "Tomas Reyes"), but never into a different role label: a
	// role is a type, so "Woman" is not "Refugee Woman" (Arise's one
	// "Refugee Woman" was collecting every unnamed woman in the series).
	resolve := func(name string) (string, bool) {
		n, ok := resolver.Resolve(name)
		if ok && roles[n] && !strings.EqualFold(n, name) {
			return "", false
		}
		return n, ok
	}
	out := make(map[int]string, len(speakers))
	for idx, sp := range speakers {
		out[idx] = sp
		if isSentinel(sp) || roles[sp] {
			continue
		}
		if n, ok := resolve(sp); ok {
			out[idx] = n // the known character's own spelling
			continue
		}
		label, isTitle, ok := RoleLabel(sp)
		if !ok {
			if UnusableDescription(sp) {
				out[idx] = "Unknown"
			}
			continue
		}
		if name, ok := resolve(label); ok {
			out[idx] = name
			continue
		}
		out[idx] = label
		if !isTitle {
			roles[label] = true
		}
	}
	return out
}

// LowercaseWords is every word a book's text uses in lower case - what
// NamesAKind checks a role label against.
type LowercaseWords map[string]bool

var lowercaseWord = regexp.MustCompile(`\b[a-z][a-z'-]*\b`)

// NewLowercaseWords collects the lower-case words of paragraphs (a whole
// book).
func NewLowercaseWords(paragraphs []string) LowercaseWords {
	out := LowercaseWords{}
	for _, p := range paragraphs {
		for _, w := range lowercaseWord.FindAllString(p, -1) {
			out[strings.TrimSuffix(w, "'s")] = true
		}
	}
	return out
}

// NamesAKind reports whether a role label's head noun - its last word
// that isn't a number, possessive stripped - is a word the book itself
// uses in lower case: "Guard", "Refugee Woman", "Greek police officer",
// "Thug 1" all are ("the guard", "a woman"), while a name the model
// marked [role] isn't - "Henry", "Constable Hodges", "Count Wavereach",
// "The Nightshark" never appear lower case. Measured on this library's
// 118 role-flagged characters: every one it rejects is a named individual
// (Spire's Spite's Jerry, Lieutenant Mercer, the Duchess Blackbrine...),
// and every one it keeps is a kind of person. A label with no usable head
// noun counts as a kind, leaving the model's answer alone.
func (w LowercaseWords) NamesAKind(label string) bool {
	words := strings.Fields(label)
	for i := len(words) - 1; i >= 0; i-- {
		word := strings.TrimSuffix(strings.TrimSuffix(strings.Trim(words[i], `.,;:!?"'()`), "'s"), "’s")
		if word == "" || strings.IndexFunc(word, unicode.IsLetter) < 0 {
			continue
		}
		if len(word) > 1 && strings.ToUpper(word) == word {
			return true // an acronym ("Medical AI") is never lower case
		}
		return w[strings.ToLower(word)]
	}
	return true
}
