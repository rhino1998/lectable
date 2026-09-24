package speakerattr

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// numberTokenRE finds a maximal run of digits plus the separators a written
// number can contain (thousands commas, a decimal point, a fraction slash,
// a time colon) - numberCandidates then decides whether the whole run is a
// shape it knows how to read, and skips it otherwise (a time "3:45", a
// section number "1.2.3", a full date "3/4/2024"). Always ends on a digit,
// so sentence punctuation ("5," / "5.") is never swallowed into the token.
var numberTokenRE = regexp.MustCompile(`[0-9](?:[0-9,./:]*[0-9])?`)

var (
	plainIntRE   = regexp.MustCompile(`^[0-9]+$`)
	groupedIntRE = regexp.MustCompile(`^[0-9]{1,3}(?:,[0-9]{3})+$`)
	decimalRE    = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	fractionRE   = regexp.MustCompile(`^([0-9]+)/([0-9]+)$`)
)

// datePrepositions are the words that, directly before a bare "M/D" pair,
// make a calendar-date reading plausible enough to ask the LLM about - see
// findNumberTerms.
var datePrepositions = map[string]bool{
	"on": true, "by": true, "since": true, "until": true, "till": true,
	"before": true, "after": true, "from": true, "dated": true, "due": true,
}

// lastWordBefore returns the lowercased word immediately before byte offset
// i in text, skipping whitespace - "" if there isn't one.
func lastWordBefore(text string, i int) string {
	before := strings.TrimRightFunc(text[:i], unicode.IsSpace)
	if j := strings.LastIndexFunc(before, func(r rune) bool { return !unicode.IsLetter(r) }); j >= 0 {
		_, size := utf8.DecodeRuneInString(before[j:])
		before = before[j+size:]
	}
	return strings.ToLower(before)
}

// dottedLabelNeighbour reports whether text[start:end] is glued by a "."
// to a letter or digit on either side.
func dottedLabelNeighbour(text string, start, end int) bool {
	alnum := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	if strings.HasPrefix(text[end:], ".") {
		if r, _ := utf8.DecodeRuneInString(text[end+1:]); end+1 < len(text) && alnum(r) {
			return true
		}
	}
	if strings.HasSuffix(text[:start], ".") && start >= 2 {
		if r, _ := utf8.DecodeLastRuneInString(text[:start-1]); alnum(r) {
			return true
		}
	}
	return false
}

// yearCues are words that, directly before a four-digit number followed by
// punctuation rather than another word, mark it as a year ("In 2001, the
// project..."). A following word ("since 1500 gold coins") keeps it a
// quantity - see yearReading.
var yearCues = map[string]bool{
	"in": true, "since": true, "until": true, "till": true, "circa": true, "year": true,
	"spring": true, "summer": true, "autumn": true, "fall": true, "winter": true,
}

var monthDayCommaRE = regexp.MustCompile(`(?:January|February|March|April|May|June|July|August|September|October|November|December) [0-9]{1,2}, *$`)

// yearReading spells text[start:end] the way a year is said ("nineteen
// ninety eight", "two thousand and one", "twenty twenty five") when it's a
// plain four-digit number from 1100 to 2099 in a year context - right after
// "Month D," or after a yearCues word with no word following it - and
// returns "" otherwise, leaving it to be read as a cardinal.
func yearReading(text string, start, end int) string {
	tok := text[start:end]
	if len(tok) != 4 || !plainIntRE.MatchString(tok) {
		return ""
	}
	n, _ := strconv.Atoi(tok)
	if n < 1100 || n > 2099 {
		return ""
	}
	after := strings.TrimLeft(text[end:], " ")
	r, _ := utf8.DecodeRuneInString(after)
	followedByWord := after != "" && unicode.IsLetter(r)
	if !monthDayCommaRE.MatchString(text[:start]) && (followedByWord || !yearCues[lastWordBefore(text, start)]) {
		return ""
	}
	hi, lo := int64(n/100), int64(n%100)
	switch {
	case n >= 2000 && n < 2010:
		return numberWords(int64(n)) // two thousand and one
	case lo == 0:
		return numberWords(hi) + " hundred" // nineteen hundred
	case lo < 10:
		return numberWords(hi) + " oh " + numberWords(lo) // nineteen oh five
	}
	return numberWords(hi) + " " + numberWords(lo)
}

// counterLabelMaxWords bounds how short a paragraph must be for a trailing
// "M/N" to count as a stat-block label line ("Technique 1/3").
const counterLabelMaxWords = 8

// counterReading reads an "M/N" token as a count - "six of six", "one of
// three" - rather than a fraction when its context says it's a tally, not
// a portion: M >= N ("Capacity: 6/6" read as "six sixths" is nonsense), it
// follows a "Label:", or it ends a short label line ("Evolution 2/3",
// "Strain 0/3") - the stat-block shape LitRPG books are full of. Returns ""
// for anything else, leaving numberCandidate's fraction reading ("drank
// 3/4 of the bottle" -> "three quarters").
func counterReading(text string, start, end int) string {
	m := fractionRE.FindStringSubmatch(text[start:end])
	if m == nil {
		return ""
	}
	num, ok1 := parseSpellable(m[1])
	den, ok2 := parseSpellable(m[2])
	if !ok1 || !ok2 || den == 0 {
		return ""
	}
	before := strings.TrimRightFunc(text[:start], unicode.IsSpace)
	labelLine := strings.TrimSpace(text[end:]) == "" && len(strings.Fields(text)) <= counterLabelMaxWords
	if num < den && !strings.HasSuffix(before, ":") && !labelLine {
		return ""
	}
	return numberWords(num) + " of " + numberWords(den)
}

// maxSpelledNumber bounds what numberWords will spell out - anything bigger
// is left for the TTS front-end to read however it reads it.
const maxSpelledNumber = 999_999_999_999

// findNumberTerms finds every integer (plain or comma-grouped), decimal,
// fraction, and ordinal ("21st") in text, each with one fully spelled-out
// reading, so ResolvePronunciation resolves it directly without ever
// reaching the model - except a bare "M/D" pair that could also be a
// calendar date, which carries the date as a second candidate (see
// dateReading). Offsets are byte offsets into text, like every other
// pronunciationTerm. A token glued to a letter ("A4", "3D", "5km") or to a
// currency sign is skipped rather than guessed at - "$5" read as "dollar
// five" would be worse than leaving it to the TTS front-end.
func findNumberTerms(text string) []pronunciationTerm {
	var out []pronunciationTerm
	for _, loc := range numberTokenRE.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if r, _ := utf8.DecodeLastRuneInString(text[:start]); start > 0 && (unicode.IsLetter(r) || strings.ContainsRune("$£€¥", r)) {
			continue
		}
		tok := text[start:end]
		// A dot joining this token to more letters/digits makes it part of
		// a section or version label ("1.E.8", "v2.x") - not a quantity.
		if dottedLabelNeighbour(text, start, end) {
			continue
		}

		// An ordinal suffix is only accepted on a plain integer, and only
		// when it's the whole rest of the word ("21st", not "21stly").
		rest := text[end:]
		suffixLen := 0
		for _, suf := range []string{"st", "nd", "rd", "th", "ST", "ND", "RD", "TH"} {
			if strings.HasPrefix(rest, suf) {
				suffixLen = len(suf)
				break
			}
		}
		after := rest[suffixLen:]
		if r, _ := utf8.DecodeRuneInString(after); after != "" && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			continue
		}
		if suffixLen == 0 {
			if r, _ := utf8.DecodeRuneInString(rest); rest != "" && unicode.IsLetter(r) {
				continue
			}
		}

		var readings []string
		if suffixLen > 0 {
			if !plainIntRE.MatchString(tok) {
				continue
			}
			n, ok := parseSpellable(tok)
			if !ok {
				continue
			}
			readings = []string{ordinalWords(n)}
			end += suffixLen
		} else if y := yearReading(text, start, end); y != "" {
			readings = []string{y}
		} else if c := counterReading(text, start, end); c != "" {
			readings = []string{c}
		} else if r := numberCandidate(tok); r != "" {
			readings = []string{r}
			// A bare "M/D" pair that could also be a calendar date is the
			// one genuinely ambiguous number shape - but only offered to the
			// LLM as a date when a date-style preposition precedes it ("on
			// 3/4", "by 3/4"). Measured on real books, a bare "M/D" is
			// overwhelmingly a count or ratio ("Technique 2/3", "Capacity:
			// 6/6"), and the model still called a few of those dates when
			// asked about every one.
			if d := dateReading(tok); d != "" && datePrepositions[lastWordBefore(text, start)] {
				readings = append(readings, d)
			}
		}
		if len(readings) == 0 {
			continue
		}
		term := pronunciationTerm{
			Offset:     start,
			Length:     end - start,
			Text:       text[start:end],
			Candidates: readings,
		}
		if len(readings) > 1 {
			term.Senses = []string{
				"a count, ratio, score, or fraction (2/3 of the votes, Capacity: 3/6)",
				"a calendar date (born on 3/4)",
			}
		}
		out = append(out, term)
	}
	return out
}

// numberCandidate spells out one numeric token, or returns "" for a shape it
// doesn't read (see numberTokenRE).
func numberCandidate(tok string) string {
	switch {
	case plainIntRE.MatchString(tok):
		// A leading zero ("007", "05") is an identifier or a clock-style
		// digit string, not a quantity - leave it alone.
		if len(tok) > 1 && tok[0] == '0' {
			return ""
		}
		if n, ok := parseSpellable(tok); ok {
			return numberWords(n)
		}
	case groupedIntRE.MatchString(tok):
		if n, ok := parseSpellable(strings.ReplaceAll(tok, ",", "")); ok {
			return numberWords(n)
		}
	case decimalRE.MatchString(tok):
		whole, frac, _ := strings.Cut(tok, ".")
		if len(whole) > 1 && whole[0] == '0' {
			return ""
		}
		n, ok := parseSpellable(whole)
		if !ok {
			return ""
		}
		digits := make([]string, len(frac))
		for i, d := range frac {
			digits[i] = onesWords[d-'0']
		}
		return numberWords(n) + " point " + strings.Join(digits, " ")
	case fractionRE.MatchString(tok):
		m := fractionRE.FindStringSubmatch(tok)
		num, ok1 := parseSpellable(m[1])
		den, ok2 := parseSpellable(m[2])
		if !ok1 || !ok2 || den == 0 {
			return ""
		}
		return fractionWords(num, den)
	}
	return ""
}

func parseSpellable(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > maxSpelledNumber {
		return 0, false
	}
	return n, true
}

var onesWords = [...]string{
	"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
	"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen",
}

var tensWords = [...]string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}

var scaleWords = [...]string{"", "thousand", "million", "billion"}

// numberWords spells n (0..maxSpelledNumber) out in British style - "and"
// before the last two digits ("seven thousand four hundred and fifty six",
// "one thousand and five") - space-separated with no hyphens, so
// ordinalWords can rewrite just the final word.
func numberWords(n int64) string {
	if n == 0 {
		return "zero"
	}
	var groups []int64 // least significant first
	for m := n; m > 0; m /= 1000 {
		groups = append(groups, m%1000)
	}
	var parts []string
	for i := len(groups) - 1; i >= 0; i-- {
		g := groups[i]
		if g == 0 {
			continue
		}
		var words []string
		if g >= 100 {
			words = append(words, onesWords[g/100], "hundred")
		}
		if r := g % 100; r > 0 {
			// "and" joins a sub-hundred remainder to its own hundreds, or -
			// in the lowest group only - to a larger number above it.
			if g >= 100 || (i == 0 && len(groups) > 1) {
				words = append(words, "and")
			}
			words = append(words, underHundredWords(r))
		}
		if i > 0 {
			words = append(words, scaleWords[i])
		}
		parts = append(parts, strings.Join(words, " "))
	}
	return strings.Join(parts, " ")
}

func underHundredWords(n int64) string {
	if n < 20 {
		return onesWords[n]
	}
	if n%10 == 0 {
		return tensWords[n/10]
	}
	return tensWords[n/10] + " " + onesWords[n%10]
}

var irregularOrdinals = map[string]string{
	"one": "first", "two": "second", "three": "third", "five": "fifth",
	"eight": "eighth", "nine": "ninth", "twelve": "twelfth",
}

// ordinalWords spells n as an ordinal ("twenty first", "one hundredth") by
// rewriting only numberWords' final word.
func ordinalWords(n int64) string {
	words := strings.Fields(numberWords(n))
	last := words[len(words)-1]
	switch {
	case irregularOrdinals[last] != "":
		last = irregularOrdinals[last]
	case strings.HasSuffix(last, "y"):
		last = strings.TrimSuffix(last, "y") + "ieth"
	default:
		last += "th"
	}
	words[len(words)-1] = last
	return strings.Join(words, " ")
}

// fractionWords reads num/den as a spoken fraction - "one third", "two
// thirds", "one half", "three quarters", "eight twelfths". A denominator of
// one has no ordinal form worth saying, so it reads as "N over one".
func fractionWords(num, den int64) string {
	if den == 1 {
		return numberWords(num) + " over one"
	}
	var unit string
	switch den {
	case 2:
		unit = "half"
		if num != 1 {
			unit = "halves"
		}
	case 4:
		unit = "quarter"
		if num != 1 {
			unit = "quarters"
		}
	default:
		unit = ordinalWords(den)
		if num != 1 {
			unit += "s"
		}
	}
	return numberWords(num) + " " + unit
}
