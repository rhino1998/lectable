package speakerattr

import (
	"regexp"
	"slices"
	"strings"
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
	if lead == "" && len(words) == len(strings.Fields(s)) {
		// No article and nothing cut: a proper name ("Jeb", "Quin
		// Cold") or already a role label ("Guard").
		return "", false, false
	}
	head := strings.ToLower(strings.Trim(words[len(words)-1], `.,'’"“”!?`))
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
	out := make(map[int]string, len(speakers))
	for idx, sp := range speakers {
		out[idx] = sp
		if isSentinel(sp) || roles[sp] {
			continue
		}
		if _, ok := resolver.Resolve(sp); ok {
			continue
		}
		label, isTitle, ok := RoleLabel(sp)
		if !ok {
			if UnusableDescription(sp) {
				out[idx] = "Unknown"
			}
			continue
		}
		if name, ok := resolver.Resolve(label); ok {
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
