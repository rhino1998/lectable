package pronounce

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// LexiconEntry is one user-authored pronunciation rule: every whole-word
// occurrence of Term is spoken as Replacement ("Nyx" -> "Nicks"). Unlike
// the LLM pass's substitutions (see the package doc comment), these are
// typed in by the listener - a deterministic override for names and
// coinages the model keeps getting wrong, applied at generation time the
// same way (never to on-screen text or alignment).
type LexiconEntry struct {
	Term          string `json:"term"`
	Replacement   string `json:"replacement"`
	CaseSensitive bool   `json:"caseSensitive"`
	// Prefix also matches words that merely *start* with Term, respelling
	// just that part and keeping the rest ("gaol" -> "jail" also turns
	// "gaoler" into "jailer", "gaols" into "jails").
	Prefix bool `json:"prefix"`
}

// LexiconSubstitutions finds every whole-word match of lex's terms in text
// (a term's first/last character must not continue into a neighboring
// letter or digit - only its first, for a Prefix entry), as Substitutions in text's byte coordinates, sorted by
// offset and never overlapping: longer terms claim their spans first, so
// "Mary Anne" wins over "Mary" wherever both would match.
func LexiconSubstitutions(text string, lex []LexiconEntry) []Substitution {
	if len(lex) == 0 || text == "" {
		return nil
	}
	entries := append([]LexiconEntry(nil), lex...)
	sort.SliceStable(entries, func(i, j int) bool { return len(entries[i].Term) > len(entries[j].Term) })
	claimed := make([]bool, len(text))
	var out []Substitution
	for _, e := range entries {
		if e.Term == "" {
			continue
		}
		pattern := regexp.QuoteMeta(e.Term)
		if !e.CaseSensitive {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			continue
		}
	matches:
		for _, m := range re.FindAllStringIndex(text, -1) {
			start, end := m[0], m[1]
			if !wordEdge(text, start, end, e.Prefix) {
				continue
			}
			for i := start; i < end; i++ {
				if claimed[i] {
					continue matches
				}
			}
			for i := start; i < end; i++ {
				claimed[i] = true
			}
			replacement := e.Replacement
			if !e.CaseSensitive {
				replacement = matchCase(text[start:end], replacement)
			}
			out = append(out, Substitution{Offset: start, Length: end - start, Replacement: replacement})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out
}

// wordEdge reports whether text[start:end] isn't glued to a neighboring
// word character on either side (only the leading side when prefix).
func wordEdge(text string, start, end int, prefix bool) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		first, _ := utf8.DecodeRuneInString(text[start:end])
		if isWordRune(r) && isWordRune(first) {
			return false
		}
	}
	if end < len(text) && !prefix {
		r, _ := utf8.DecodeRuneInString(text[end:])
		last, _ := utf8.DecodeLastRuneInString(text[start:end])
		if isWordRune(r) && isWordRune(last) {
			return false
		}
	}
	return true
}

// matchCase carries a case-insensitive match's capitalization onto its
// replacement: an all-caps original ("GAOL") gets an all-caps replacement,
// a capitalized one ("Gaol") a capitalized replacement; anything else
// keeps the replacement exactly as the rule spells it.
func matchCase(original, replacement string) string {
	if strings.ToUpper(original) == original && strings.ToLower(original) != original && utf8.RuneCountInString(original) > 1 {
		return strings.ToUpper(replacement)
	}
	first, _ := utf8.DecodeRuneInString(original)
	rFirst, size := utf8.DecodeRuneInString(replacement)
	if unicode.IsUpper(first) && unicode.IsLower(rFirst) {
		return string(unicode.ToUpper(rFirst)) + replacement[size:]
	}
	return replacement
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// MergeLexicon combines a paragraph's lexicon matches with its LLM-resolved
// pronunciation substitutions: the listener's own rule wins wherever the
// two overlap (it was typed in precisely because the model gets that word
// wrong), and the result stays sorted and non-overlapping for Compose/Apply.
func MergeLexicon(lexicon, pron []Substitution) []Substitution {
	if len(lexicon) == 0 {
		return pron
	}
	out := append([]Substitution(nil), lexicon...)
	for _, p := range pron {
		pEnd := p.Offset + p.Length
		overlaps := false
		for _, l := range lexicon {
			if p.Offset < l.Offset+l.Length && l.Offset < pEnd {
				overlaps = true
				break
			}
		}
		if !overlaps {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out
}

// LexiconMatchedWords is every word in text that rule matches, as written -
// for a Prefix rule the whole word ("gaolers", not just "gaol"), so a
// listener can see exactly what a derived-word rule will respell.
func LexiconMatchedWords(text string, rule LexiconEntry) []string {
	var out []string
	for _, sub := range LexiconSubstitutions(text, []LexiconEntry{rule}) {
		end := sub.Offset + sub.Length
		for end < len(text) {
			r, size := utf8.DecodeRuneInString(text[end:])
			if !isWordRune(r) {
				break
			}
			end += size
		}
		out = append(out, text[sub.Offset:end])
	}
	return out
}
