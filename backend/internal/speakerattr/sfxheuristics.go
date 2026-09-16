package speakerattr

import (
	"regexp"

	"github.com/rhino1998/lectable/backend/internal/deliverytags"
)

// heuristicSfxPattern is one regex-detectable, deterministically spelled
// onomatopoeia (or, for the ellipsis rule, punctuation) this package tags
// without ever asking the LLM about it - unlike TagSfx's own model-driven
// sfx judgment (which still exists for onomatopoeia with no single fixed
// spelling: crying, screaming, burping, sniffling, etc., each written many
// different ways in real prose), these have one small, closed set of
// real-world spellings a plain regex recognizes deterministically and for
// free. sfxSystemPrompt doesn't even list these as options any more - see
// its own doc comment - so there's no risk of the LLM burning tokens
// re-deriving what regex already gets right every time.
type heuristicSfxPattern struct {
	re  *regexp.Regexp
	tag string
}

var heuristicSfxPatterns = []heuristicSfxPattern{
	// Laughter: repeated ha/he/ho syllables, optionally led by an evil-
	// laugh "mwa"/"bwa" prefix and/or closed by a trailing lone "h" -
	// covers "Ha", "Hah", "Haha", "Hahaha", "Mwahaha", "Bwahahaha", "Heh",
	// "Hehe", "Hehehe", "Hoh", "Hoho", "Hohoho", case-insensitive.
	//
	// "ha" is allowed bare (a single "ha" with no repeat or trailing "h"
	// still counts - it's already a standalone laugh interjection), but
	// bare "he"/"ho" deliberately are NOT: "he" is one of the most common
	// words in English (the pronoun) and "ho" shows up in names/places/
	// other exclamations, so either matching bare would tag ordinary prose
	// constantly. "he"/"ho" only count once they're either repeated
	// (hehe, hoho) or closed with a trailing "h" (heh, hoh) - a spelling
	// that never collides with the plain word.
	//
	// Deliberately only the directly-concatenated form - "Ha, ha, ha!" or
	// "Ha ha" already matches as separate words each on their own, which
	// is fine (each gets its own tag); no special-casing for a joining
	// space/comma/hyphen between repeats.
	{regexp.MustCompile(`(?i)\b(?:(?:mwa|bwa)?ha(?:ha)*h?|(?:he){2,}h?|heh|(?:ho){2,}h?|hoh)\b`), "<|sfx:laughter|>"},
	// Humming/hesitation: "Hmm", "Hmmmm", "Hrmmm", ... - one or more h's,
	// an optional "r", then one or more m's.
	{regexp.MustCompile(`(?i)\bh+r?m+\b`), "<|sfx:humming|>"},
	// Sneeze: "Achoo", "Atchoo", elongated ("Achooo") - one or more "a"s,
	// an optional "t", then "cho" plus one or more "o"s.
	{regexp.MustCompile(`(?i)\ba+t?choo+\b`), "<|sfx:sneeze|>"},
	// Throat-clear/cough: "Ahem", elongated ("Aheem", "Ahemmm").
	{regexp.MustCompile(`(?i)\bahe+m+\b`), "<|sfx:cough|>"},
	// Sigh: "Sigh", elongated ("Sighhh"). \b after h+ keeps this from
	// matching "sight"/"sighed"/"sighing" - none of those have a word
	// boundary right after their own "h".
	{regexp.MustCompile(`(?i)\bsigh+\b`), "<|sfx:sigh|>"},
	// Trailing-off ellipsis: "...", "…", or spaced-out ". . ." (any run of
	// 3+ periods, each optionally separated by a single space/tab) - a
	// pause belongs immediately before it, the same place content-pause
	// judgment would put one for "an ellipsis" per sfxSystemPrompt's own
	// rule, just guaranteed instead of left to the model's own recall.
	// Always the lighter <|prosody:pause|>, never long_pause - a regex
	// match carries no judgment about how long the beat should be, so it
	// defaults conservative the same way the breath-pause heuristic does.
	{regexp.MustCompile(`…|\.(?:[ \t]?\.){2,}`), "<|prosody:pause|>"},
}

// addHeuristicSfxTags scans orig (a line's own real text, as sent to/
// diffed against the LLM - see TagSfx's own call site) for every
// heuristicSfxPatterns match and returns existing plus one new insertion
// per match, skipping any match whose start offset coincides with an
// insertion already in existing (most often the LLM sfx pass itself
// already tagging the same word with something, sfx:laughter included) so
// the same word is never tagged twice. Matches can never land at
// len(orig) (offset == length of the matched word's own remaining text is
// impossible - the tag always sits before real word text that follows
// it), so this needs no trailing-tag filtering of its own - see
// parseTaggedLines's own doc comment for why that filtering exists at all.
func addHeuristicSfxTags(orig string, existing []deliverytags.Insertion) []deliverytags.Insertion {
	taken := make(map[int]bool, len(existing))
	for _, ins := range existing {
		taken[ins.Offset] = true
	}
	out := existing
	for _, p := range heuristicSfxPatterns {
		for _, loc := range p.re.FindAllStringIndex(orig, -1) {
			if taken[loc[0]] {
				continue
			}
			taken[loc[0]] = true
			out = append(out, deliverytags.Insertion{Offset: loc[0], Tag: p.tag})
		}
	}
	return out
}
