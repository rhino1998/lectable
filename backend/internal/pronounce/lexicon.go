package pronounce

import (
	"regexp"
	"sort"
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
}

// LexiconSubstitutions finds every whole-word match of lex's terms in text
// (a term's first/last character must not continue into a neighboring
// letter or digit), as Substitutions in text's byte coordinates, sorted by
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
			if !wordEdge(text, start, end) {
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
			out = append(out, Substitution{Offset: start, Length: end - start, Replacement: e.Replacement})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out
}

// wordEdge reports whether text[start:end] isn't glued to a neighboring
// word character on either side.
func wordEdge(text string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		first, _ := utf8.DecodeRuneInString(text[start:end])
		if isWordRune(r) && isWordRune(first) {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		last, _ := utf8.DecodeLastRuneInString(text[start:end])
		if isWordRune(r) && isWordRune(last) {
			return false
		}
	}
	return true
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
