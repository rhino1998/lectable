package epub

import (
	"strings"
	"unicode"
)

// sceneBreakGlyphs is the closed set of characters a prose scene/section
// break is conventionally spelled out with when an epub doesn't use a real
// <hr/> element - a handful of repeated symbols, e.g. "* * *", "***",
// "-=-=-=-", or a single "⁂" asterism (the dedicated Unicode character for
// exactly this purpose). Deliberately contains no letter or digit, so
// isSceneBreakText can never misidentify even a very short real line of
// narration ("No.", "Wait.") as a break.
var sceneBreakGlyphs = map[rune]bool{
	'*': true, '#': true, '~': true, '-': true, '_': true, '=': true, '+': true,
	'•': true, '·': true, '∙': true, '○': true, '◦': true, '§': true,
	'‡': true, '†': true, '❦': true, '❧': true, '⁂': true, '⁕': true,
	'♦': true, '♠': true, '❈': true, '✦': true, '✧': true, '⋅': true,
}

// maxSceneBreakGlyphs bounds how many marker characters a candidate line
// can contain before isSceneBreakText gives up and treats it as ordinary
// text instead - real breaks are always a short, repeated run ("* * *",
// "-=-=-=-="), so this stays generous while still refusing to swallow an
// implausibly long line.
const maxSceneBreakGlyphs = 12

// isSceneBreakText reports whether text (already whitespace-normalized by
// collectText, and possibly still carrying boldStart/boldEnd/italicStart/
// italicEnd sentinels - ignored here, since a scene-break line is never
// meaningfully bold/italic) consists of nothing but sceneBreakGlyphs. See
// that var's own doc comment for why this can only ever be a false
// negative (missing a break formatted with an unlisted glyph), never a
// false positive against real narration.
func isSceneBreakText(text string) bool {
	n := 0
	for _, r := range text {
		switch {
		case r == boldStart || r == boldEnd || r == italicStart || r == italicEnd:
			continue
		case unicode.IsSpace(r):
			continue
		case sceneBreakGlyphs[r]:
			n++
		default:
			return false
		}
	}
	return n > 0 && n <= maxSceneBreakGlyphs
}

func normalizeWhitespace(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// quoteOpeners/quoteClosers are the double-quote characters this app
// recognizes as bounding spoken dialogue - straight ASCII " (which serves
// as both opener and closer), curly “ ” (which don't), guillemets « »
// (French/Russian/Spanish/Italian/Greek-style quotes, also directional),
// and angle brackets < > - directional the same way, and only reachable
// here as literal runes via a source epub's own &lt;/&gt; entities (a raw
// `<` in XHTML text would otherwise open a tag, so golang.org/x/net/html
// only ever hands collectText a literal `<`/`>` rune after decoding an
// entity), making them unambiguous to treat as quote marks rather than
// markup. Single quotes are deliberately excluded: they're far more often
// an apostrophe (contraction, possessive) than nested dialogue, and
// getting that wrong would wrongly fragment ordinary narration.
const (
	quoteOpeners = "\"“«<"
	quoteClosers = "\"”»>"
)

// quoteSegment is one piece of splitQuoteSegments' output - its text plus
// whether it's an actual quoted-dialogue span (IsQuote) as opposed to a
// narration/description segment. See Block.IsQuote for why this
// distinction is kept all the way through to attribution.
type quoteSegment struct {
	Text    string
	IsQuote bool
}

// splitQuoteSegments splits text into alternating narration and quoted-
// dialogue segments on quote-mark boundaries (quoteOpeners/quoteClosers -
// straight ", curly “ ”, guillemets « », and angle brackets < >), each
// segment including its own quote marks, so a paragraph like `”The bridge
// is out,” Sam said.` becomes [{`”The bridge is out,”`, true}, {`Sam
// said.`, false}] - two separately attributable/voiceable units (see
// internal/speakerattr, internal/narration) that the reader still sees as
// one paragraph (see Block.Inline). A straight " toggles in/out of a quote
// (it's both opener and closer); every other pair only opens on its own
// left-hand rune and only closes on its own right-hand rune. Unbalanced or
// quote-free text comes back as a single unsplit, non-quote segment -
// splitting only happens where a quote clearly opens and closes within the
// same paragraph, never on a guess. Every span that opens and closes
// cleanly becomes an IsQuote segment, including scare quotes and mentions
// (`the "downward" stairway`) - telling those apart from real dialogue is
// left entirely to internal/speakerattr.Client.ScareQuoteChapter's LLM
// pass, which flags them (store.Paragraph.ScareQuote) so generation merges
// them back into the surrounding narration's own TTS call (see
// jobs.Manager.scareQuoteMergeGroup) rather than voicing them separately.
func splitQuoteSegments(text string) []quoteSegment {
	type span struct{ start, end int } // rune offsets, end exclusive, quote marks included

	runes := []rune(text)
	var spans []span
	inQuote := false
	quoteStart := 0
	for i, ch := range runes {
		switch {
		case !inQuote && strings.ContainsRune(quoteOpeners, ch):
			inQuote = true
			quoteStart = i
		case inQuote && strings.ContainsRune(quoteClosers, ch):
			spans = append(spans, span{quoteStart, i + 1})
			inQuote = false
		}
	}
	if len(spans) == 0 {
		return []quoteSegment{{Text: text}}
	}

	var segments []quoteSegment
	pos := 0
	for _, sp := range spans {
		if sp.start > pos {
			if s := strings.TrimSpace(string(runes[pos:sp.start])); s != "" {
				segments = append(segments, quoteSegment{Text: s})
			}
		}
		segments = append(segments, quoteSegment{Text: string(runes[sp.start:sp.end]), IsQuote: true})
		pos = sp.end
	}
	if pos < len(runes) {
		if s := strings.TrimSpace(string(runes[pos:])); s != "" {
			segments = append(segments, quoteSegment{Text: s})
		}
	}
	// segments always has at least one element here (each span is
	// unconditionally appended above), so no "did we actually split"
	// fallback is needed - unlike the len(spans)==0 case above, there's
	// nothing to discard: a paragraph that's entirely one quote with no
	// surrounding narration (e.g. a standalone `"I don't know what to
	// do."` paragraph) correctly comes back as a single IsQuote=true
	// segment, not the plain-text fallback. An earlier version of this
	// function returned []quoteSegment{{Text: text}} (silently dropping
	// IsQuote) whenever len(segments) <= 1, which - since a whole-paragraph
	// quote always produces exactly one segment - meant every standalone
	// dialogue paragraph with no narration tag lost its IsQuote flag
	// entirely, forcing it to be treated as narration by every downstream
	// IsQuote-gated behavior (attribution's own prompt hints, and
	// httpapi.attributeChapter's deterministic is_quote filter, which
	// would then force its speaker back to "Narrator" regardless of what
	// the model correctly identified it as).
	return segments
}
