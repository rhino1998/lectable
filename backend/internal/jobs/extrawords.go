package jobs

import (
	"regexp"
	"strings"
	"unicode"
)

// minExtraWords is how many consecutive transcript words with no
// counterpart in the input text (extraWordRun) make a generation count as
// having spoken extra words - a repeated phrase, a leaked reference line,
// or babble after the text ends. A run rather than a total, since ASR
// slips (a misheard name, "gonna" for "going to") surface as scattered
// substitutions and one-word insertions, while real additions are
// contiguous phrases.
const minExtraWords = 3

// extraWordsTagPattern matches one inline delivery tag (<|category:value|>,
// see internal/deliverytags) - never spoken, so dropped before comparing.
var extraWordsTagPattern = regexp.MustCompile(`<\|[^|]*\|>`)

// comparisonWords splits text into lowercase words for extraWordRun:
// delivery tags removed, curly apostrophes folded to straight ones, and
// every other non-letter/digit/apostrophe treated as a separator, so
// punctuation, hyphenation, and casing differences between the input
// text and ASR's own formatting never count as a mismatch.
func comparisonWords(text string) []string {
	text = extraWordsTagPattern.ReplaceAllString(text, " ")
	text = strings.NewReplacer("’", "'", "‘", "'").Replace(strings.ToLower(text))
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
	out := fields[:0]
	for _, f := range fields {
		if f = strings.Trim(f, "'"); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// extraWordRun returns the longest run of consecutive transcript words
// that a minimum word-level edit alignment against expected has to insert
// - words the audio contains that the input text doesn't. A substitution
// (a misheard or respelled word) costs the same as an insertion plus a
// deletion would together, so the alignment prefers matching words up one
// for one over treating them as extra.
func extraWordRun(expected, transcript string) int {
	exp, got := comparisonWords(expected), comparisonWords(transcript)
	n, m := len(exp), len(got)
	// cost[i][j]: edit distance between exp[i:] and got[j:].
	cost := make([][]int, n+1)
	for i := range cost {
		cost[i] = make([]int, m+1)
	}
	for i := n; i >= 0; i-- {
		for j := m; j >= 0; j-- {
			switch {
			case i == n:
				cost[i][j] = m - j
			case j == m:
				cost[i][j] = n - i
			default:
				sub := 1
				if exp[i] == got[j] {
					sub = 0
				}
				cost[i][j] = min(cost[i+1][j+1]+sub, cost[i+1][j]+1, cost[i][j+1]+1)
			}
		}
	}
	// Walk one optimal path forward, preferring match/substitute, then
	// delete, then insert - so an insertion is only taken when nothing
	// else is as cheap.
	longest, run := 0, 0
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && cost[i][j] == cost[i+1][j+1]+boolInt(exp[i] != got[j]):
			i, j, run = i+1, j+1, 0
		case i < n && cost[i][j] == cost[i+1][j]+1:
			i, run = i+1, 0
		default:
			j, run = j+1, run+1
			longest = max(longest, run)
		}
	}
	return longest
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
