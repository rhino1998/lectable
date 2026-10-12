package speakerattr

import (
	"maps"
	"regexp"
	"strconv"
	"strings"
)

// MaxRoleIndex is the highest speaker number kept on a role line. A role's
// numbers are voice slots reused from one conversation to the next, not
// identities, so a handful covers any scene; a larger number written as a
// figure takes one of those slots again ("Adventurer 7" is slot 1). Left
// alone it became a role of its own: The Wandering Inn had "Adventurer 7"
// and "Adventurer 8" on its roster beside "Adventurer" numbered 1 to 6.
const MaxRoleIndex = 6

var (
	roleMarkerRe    = regexp.MustCompile(`(?i)\s*\[role\]`)
	roleNumberRe    = regexp.MustCompile(`(?i)^(.*?\pL)\s*(?:#|no\.?\s*|number\s+)?(\d{1,2})$`)
	roleNumberWords = map[string]int{"two": 2, "three": 3, "four": 4, "five": 5, "six": 6}
	roleOrdinals    = map[string]int{
		"first": 1, "second": 2, "third": 3, "fourth": 4, "fifth": 5, "sixth": 6,
		"1st": 1, "2nd": 2, "3rd": 3, "4th": 4, "5th": 5, "6th": 6,
	}
)

// SplitRoleIndex takes the speaker number off a numbered role answer -
// "Guard 2", "Guard #2", "Guard Two", "Second Guard", "the second guard" -
// returning the answer without it ("Guard", "the guard") and the number.
// A "[role]" marker anywhere in answer is kept. A figure above
// MaxRoleIndex wraps round into 1..MaxRoleIndex. index is 0, and label is
// answer unchanged, when there is no number or it is 0.
//
// It only parses; a caller decides whether the answer is a role at all
// ("Unit 7" may be someone's name). A trailing "One" is left alone - "the
// Old One" is a name far more often than a count - see splitRoleOne.
func SplitRoleIndex(answer string) (label string, index int) {
	marker := roleMarkerRe.FindString(answer)
	s := strings.TrimSpace(roleMarkerRe.ReplaceAllString(answer, ""))
	rest, n := "", 0
	if m := roleNumberRe.FindStringSubmatch(s); m != nil {
		rest = m[1]
		n, _ = strconv.Atoi(m[2])
	} else if words := strings.Fields(s); len(words) >= 2 {
		last := strings.ToLower(words[len(words)-1])
		first, lead := strings.ToLower(words[0]), 0
		if (first == "the" || first == "a") && len(words) >= 3 {
			first, lead = strings.ToLower(words[1]), 1
		}
		switch {
		case roleNumberWords[last] > 0:
			rest, n = strings.Join(words[:len(words)-1], " "), roleNumberWords[last]
		case roleOrdinals[first] > 0:
			rest, n = strings.Join(append(append([]string(nil), words[:lead]...), words[lead+1:]...), " "), roleOrdinals[first]
		}
	}
	rest = strings.TrimSpace(rest)
	if n > MaxRoleIndex {
		n = (n-1)%MaxRoleIndex + 1
	}
	if n < 1 || rest == "" {
		return answer, 0
	}
	if lower := strings.ToLower(rest); lower == "the" || lower == "a" {
		return answer, 0
	}
	return rest + marker, n
}

// roleNumber takes the speaker number off one raw attribution answer
// (SplitRoleIndex) when the answer is a role: marked "[role]" by the model,
// a label already known as a role, or a description ("the second guard",
// returned as its role label, "Guard [role]"). Any other numbered answer
// is returned unchanged with 0 - "Unit 7" may be someone's name.
func roleNumber(answer string, marked bool, knownRoles map[string]bool, resolver *SpeakerNameResolver) (string, int) {
	label, n := SplitRoleIndex(answer)
	if n == 0 {
		one, ok := splitRoleOne(answer, maps.Keys(knownRoles))
		if !ok {
			return answer, 0
		}
		label, n = one, 1
	}
	bare := strings.TrimSpace(roleMarkerRe.ReplaceAllString(label, ""))
	if marked || bare != label || knownRoles[resolver.Canonical(bare)] {
		return label, n
	}
	// Folded to its label here, not left for resolveIdentities: by then
	// canonicalizeSpeakerNames has merged a plain "Guard" into the longer
	// "the guard".
	if kind, isTitle, ok := RoleLabel(bare); ok && !isTitle {
		return kind + " [role]", n
	}
	return answer, 0
}

// splitRoleOne is SplitRoleIndex for a label ending in the word "One"
// ("Thug One"), which only counts as number 1 next to a sibling: names
// must hold the same label with a higher number ("Thug Two", "Thug 2").
// Alone, "the Old One" or "Chosen One" is just what someone is called.
func splitRoleOne(answer string, names func(yield func(string) bool)) (label string, ok bool) {
	marker := roleMarkerRe.FindString(answer)
	words := strings.Fields(strings.TrimSpace(roleMarkerRe.ReplaceAllString(answer, "")))
	if len(words) < 2 || !strings.EqualFold(words[len(words)-1], "one") {
		return answer, false
	}
	bare := strings.Join(words[:len(words)-1], " ")
	for name := range names {
		if sibling, n := SplitRoleIndex(name); n > 1 && strings.EqualFold(strings.TrimSpace(roleMarkerRe.ReplaceAllString(sibling, "")), bare) {
			return bare + marker, true
		}
	}
	return answer, false
}

// shareParagraphNumbers gives the quotes of one source paragraph that went
// to the same role one speaker number: a quote, its tag and the rest of
// the speech are one person ("Not before me," the guard with the lantern
// growled, "He just cost me a gold triad"), but the model often numbers
// only one of the two. Left alone when the paragraph's quotes carry two
// different numbers for that role.
func shareParagraphNumbers(paragraphs []ParagraphInput, speakers map[int]string, indexes map[int]int) {
	for start := 0; start < len(paragraphs); {
		end := start + 1
		for end < len(paragraphs) && paragraphs[end].Inline {
			end++
		}
		numbers := map[string]int{} // role -> its one number here, -1 for several
		for _, p := range paragraphs[start:end] {
			n := indexes[p.Idx]
			if !p.IsQuote || n == 0 {
				continue
			}
			sp := speakers[p.Idx]
			if seen, ok := numbers[sp]; ok && seen != n {
				n = -1
			}
			numbers[sp] = n
		}
		for _, p := range paragraphs[start:end] {
			if n := numbers[speakers[p.Idx]]; p.IsQuote && n > 0 && p.Known == "" {
				indexes[p.Idx] = n
			}
		}
		start = end
	}
}
