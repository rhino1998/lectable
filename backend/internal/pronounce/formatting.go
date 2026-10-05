package pronounce

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// formattingGlyphs are the layout marks stripped wherever they appear:
// list bullets, footnote daggers and asterisms. The box-drawing, shape,
// symbol and dingbat blocks are covered by isFormattingGlyph's ranges.
const formattingGlyphs = "•‣⁃∙‧◦†‡⁂⁎⁕"

// looseFormattingGlyphs are marks that are formatting only when they stand
// alone between spaces ("* * *", "Name | Level", "=====") - glued to a
// word they're part of it ("C#", "l·l", "snake_case").
const looseFormattingGlyphs = "*_|=#~·⋅"

func isFormattingGlyph(r rune) bool {
	switch {
	case r >= 0x2500 && r <= 0x25FF: // box drawing, block elements, geometric shapes
		return true
	case r >= 0x2600 && r <= 0x26FF: // miscellaneous symbols
		return true
	case r >= 0x2700 && r <= 0x27BF: // dingbats
		return true
	}
	return strings.ContainsRune(formattingGlyphs, r)
}

// FormattingSubstitutions finds the formatting characters in text a TTS
// model would otherwise try to say - a list bullet comes out as a stray
// syllable - and returns a Substitution removing each run of them, for
// store.Paragraph.ResolveGenerationText. A run between two words leaves a
// comma behind ("HP 10 • MP 5" -> "HP 10, MP 5") so the items it separated
// don't run together; one at either end of the text, or after other
// punctuation, just goes. Returns nil when nothing but formatting would be
// left: an all-glyph paragraph is left for its caller to deal with rather
// than turned into empty text.
func FormattingSubstitutions(text string) []Substitution {
	var out []Substitution
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !isFormattingGlyph(r) && !strings.ContainsRune(looseFormattingGlyphs, r) {
			i += size
			continue
		}
		start, end := i, formattingRunEnd(text, i)
		i = end
		if !isFormattingRun(text, start, end) {
			continue
		}
		// Take the surrounding whitespace with the run, and any further
		// runs it leads straight into ("• • •").
		for {
			ws := end + len(text[end:]) - len(strings.TrimLeftFunc(text[end:], unicode.IsSpace))
			next := formattingRunEnd(text, ws)
			if next == ws || !isFormattingRun(text, ws, next) {
				end = ws
				break
			}
			end = next
		}
		i = end
		start = len(strings.TrimRightFunc(text[:start], unicode.IsSpace))
		if start == 0 && end == len(text) {
			return nil
		}
		out = append(out, Substitution{Offset: start, Length: end - start, Replacement: formattingJoin(text, start, end)})
	}
	return out
}

// formattingRunEnd returns the end of the unbroken run of formatting
// characters (of either kind) starting at byte offset i - i itself if
// there isn't one.
func formattingRunEnd(text string, i int) int {
	for i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !isFormattingGlyph(r) && !strings.ContainsRune(looseFormattingGlyphs, r) {
			break
		}
		i += size
	}
	return i
}

// isFormattingRun reports whether the run text[start:end] should be
// stripped: always if it holds a formattingGlyphs character, otherwise only
// standing alone between spaces - and not a lone "=", "#" or "~", which is
// read aloud ("2 = 2").
func isFormattingRun(text string, start, end int) bool {
	run := text[start:end]
	if strings.IndexFunc(run, isFormattingGlyph) >= 0 {
		return true
	}
	if before, _ := utf8.DecodeLastRuneInString(text[:start]); start > 0 && !unicode.IsSpace(before) {
		return false
	}
	if after, _ := utf8.DecodeRuneInString(text[end:]); end < len(text) && !unicode.IsSpace(after) {
		return false
	}
	return len(run) > 1 || !strings.Contains("=#~", run)
}

// formattingJoin is what replaces the stripped span text[start:end]
// (whitespace included): nothing at either end of the text, a comma
// between two words, and otherwise the space it took away, if any.
func formattingJoin(text string, start, end int) string {
	if start == 0 || end == len(text) {
		return ""
	}
	before, _ := utf8.DecodeLastRuneInString(text[:start])
	after, _ := utf8.DecodeRuneInString(text[end:])
	spaced := strings.IndexFunc(text[start:end], unicode.IsSpace) >= 0
	switch {
	case isWordRune(before) && isWordRune(after):
		return ", "
	case spaced:
		return " "
	}
	return ""
}
