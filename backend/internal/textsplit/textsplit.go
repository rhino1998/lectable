// Package textsplit breaks plain text into sentences - used by
// internal/jobs, reactively bisecting a paragraph's text as a last-resort
// retry when a forced-alignment call exceeds the TTS worker's own fixed
// audio-length ceiling (see jobs.alignWithSplit) or when generation itself
// runs out of the worker's own flat max_tokens budget for a slow-paced
// voice's text (see jobs.generateCloneSplit) - internal/epub has no
// paragraph-length cap of its own any more, so this package's only job now
// is that reactive recovery, not proactive import-time chunking. One
// implementation so both call sites can never quietly disagree about what
// counts as a sentence boundary.
package textsplit

import (
	"regexp"
	"strings"
)

// sentenceBoundaryRe matches the punctuation+optional-closing-quote+
// trailing-whitespace that ends a sentence. The closing-quote class covers
// every closing mark epub.quoteClosers recognizes (straight ", curly ”,
// guillemet », and angle bracket > - the latter reachable in real text
// only via a source epub's own &gt; entity, same as there) plus a closing
// paren and curly '. Not linguistically precise (doesn't special-case
// abbreviations like "Mr." or decimal numbers), but that's fine here: the
// only consequence of a false-positive split is one extra generation call
// (or, at import time, one extra stored paragraph row) at a slightly-odd
// boundary, which is a strictly better outcome than either generation
// failing outright or a paragraph staying too long to reliably generate at
// all.
var sentenceBoundaryRe = regexp.MustCompile(`[.!?]+['"”’»>)]*\s+`)

// SplitSentences splits text into sentences, trimmed of surrounding
// whitespace. Returns a single-element slice if no sentence boundary is
// found at all.
func SplitSentences(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var sentences []string
	start := 0
	for _, idx := range sentenceBoundaryRe.FindAllStringIndex(text, -1) {
		if s := strings.TrimSpace(text[start:idx[1]]); s != "" {
			sentences = append(sentences, s)
		}
		start = idx[1]
	}
	if s := strings.TrimSpace(text[start:]); s != "" {
		sentences = append(sentences, s)
	}
	return sentences
}
