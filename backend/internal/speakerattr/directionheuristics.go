package speakerattr

import (
	"regexp"

	"github.com/rhino1998/lectable/backend/internal/deliverytags"
)

// heuristicSentencePattern is one regex-detectable, purely typographic
// sentence-level cue - the direction-pass analog of heuristicSfxPatterns
// (sfxheuristics.go), living here instead since shouting/surprise are
// sentence-level tags (validSentenceTags), not inline ones
// (validInlineTags) - a line's own punctuation, not any particular word
// spelling, is what regex recognizes deterministically here.
type heuristicSentencePattern struct {
	re  *regexp.Regexp
	tag string
}

var heuristicSentencePatterns = []heuristicSentencePattern{
	// Doubled (or more) exclamation marks - "!!", "!!!" - are an
	// unambiguous, purely typographic shouting cue.
	{regexp.MustCompile(`!!+`), "<|style:shouting|>"},
	// A "?!"/"!?" combo, in either order and doubled or not, is the
	// standard typographic marker for a startled, incredulous
	// exclamation-question - read as surprise/disbelief, not volume, so
	// this gets <|emotion:surprise|> rather than shouting.
	{regexp.MustCompile(`\?!+|!+\?`), "<|emotion:surprise|>"},
	// An ALL-CAPS word (2+ letters, contractions like "DON'T" allowed)
	// immediately followed by "!" - "STOP!", "NO!", "GET OUT!" - is the
	// standard prose convention for shouted dialogue. Requiring the
	// trailing "!" (rather than firing on any all-caps word alone) is
	// what keeps this from misfiring on an ordinary acronym/initialism
	// ("NASA", "OK", "DNA") sitting in otherwise normal prose - those
	// essentially never appear immediately followed by "!". Only ever
	// actually applies to quoted dialogue in practice: restrictNonQuoteTags
	// already strips any <|style:*|> insertion (this tag included) from a
	// non-quote paragraph regardless of source, the same restriction the
	// two rules above already rely on rather than checking IsQuote here.
	{regexp.MustCompile(`\b[A-Z]{2,}(?:'[A-Z]+)?\b!`), "<|style:shouting|>"},
}

// addHeuristicSentenceTags checks orig against every heuristicSentencePatterns
// rule and returns existing plus one new insertion per rule that matches
// anywhere in orig, always anchored at offset 0 (the line's own start) -
// matching directionSystemPrompt's own "most lines need at most one tag,
// placed at the very start of the line" default, and its own newer
// same-position stacking rule (see its doc comment) for when more than one
// applies: both rules can genuinely fire on the same line at once (e.g.
// "No!!?" contains both "!!" and "!?"), correctly stacking
// <|style:shouting|> and <|emotion:surprise|> together rather than picking
// just one - a line can be both shouted and startled. Insertion order here
// (heuristicSentencePatterns is checked Style-then-Emotion, and any of the
// LLM's own insertions in existing always come first since heuristic tags
// are appended after them) doesn't enforce directionSystemPrompt's own
// Emotion-then-Style-then-Pacing stacking convention in every possible
// combination - a real but low-stakes gap given how narrow the two rules
// here are (only shout/surprise, never all three categories at once).
// Skips a rule entirely if existing already contains that exact tag
// (either from the LLM's own judgment or a previous call), so the same
// line is never told to shout/be-surprised twice.
func addHeuristicSentenceTags(orig string, existing []deliverytags.Insertion) []deliverytags.Insertion {
	has := make(map[string]bool, len(existing))
	for _, ins := range existing {
		has[ins.Tag] = true
	}
	out := existing
	for _, p := range heuristicSentencePatterns {
		if has[p.tag] {
			continue
		}
		if p.re.MatchString(orig) {
			out = append(out, deliverytags.Insertion{Offset: 0, Tag: p.tag})
		}
	}
	return out
}
