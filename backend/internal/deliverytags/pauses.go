package deliverytags

import (
	"regexp"
	"unicode"
)

// PauseTag is the one Higgs delivery tag this app still inserts - see
// PauseInsertions. Every other Higgs tag (emotion/style/prosody speed/sfx)
// was dropped for pulling the voice away from its reference clip;
// emotion is now carried by the reference clip itself instead (see
// internal/emotions).
const PauseTag = "<|prosody:pause|>"

// pausePattern matches the punctuation a pause belongs immediately before:
// a trailing-off ellipsis ("…", "...", or spaced ". . .", any run of 3+
// periods), or an em dash - a mid-sentence break or interruption.
var pausePattern = regexp.MustCompile(`…|\.(?:[ \t]?\.){2,}|—`)

// PauseInsertions returns a <|prosody:pause|> insertion before every
// ellipsis and em dash in text that still has something speakable after
// it - a deterministic, character-based replacement for the LLM pass that
// used to place pauses, applied at generation time for Higgs only (see
// store.Paragraph.ResolveGenerationText). A pause with nothing left to
// speak after it (a line ending "..." or "—", possibly followed by closing
// quotes) is skipped: Higgs renders a tag with nothing to color as a stray
// artifact. Consecutive matches with nothing speakable between them
// ("—..." ) collapse into one pause.
func PauseInsertions(text string) []Insertion {
	var out []Insertion
	lastEnd := -1
	for _, m := range pausePattern.FindAllStringIndex(text, -1) {
		if !hasSpeakable(text[m[1]:]) {
			break
		}
		if lastEnd >= 0 && !hasSpeakable(text[lastEnd:m[0]]) {
			lastEnd = m[1]
			continue
		}
		out = append(out, Insertion{Offset: m[0], Tag: PauseTag})
		lastEnd = m[1]
	}
	return out
}

// hasSpeakable reports whether s contains a letter or digit.
func hasSpeakable(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
