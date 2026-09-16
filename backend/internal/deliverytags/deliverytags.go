// Package deliverytags implements the shared validation/composition logic
// behind Higgs Audio v3 TTS's own inline delivery-tag markup
// (<|category:value|> - see PROMPTING.md embedded in
// Higgs-Audio-v3-TTS-4B-GGUF's own tokenizer.json, where every one of these
// is a dedicated "special": true added_tokens entry, so audio.cpp's plain
// tokenizer.encode(text, true) tokenizes one as a single atomic
// conditioning token rather than literal text).
//
// A tag may only ever be *inserted* into a paragraph's own plain text,
// never change it - a paragraph's real words are what forced alignment and
// the on-screen reading text are keyed to, and both must stay identical to
// what the ttsworker actually speaks minus tag markup, or word-level
// timing and the reader's own highlight would drift out of sync with the
// audio. ExtractInsertions enforces that invariant on an LLM's own output
// (never trust it un-verified - see internal/speakerattr's callers), and
// Merge composes multiple independently-produced annotated variants of the
// same plain text (e.g. one LLM pass tagging emotion/style, a separate one
// tagging sound effects) back into a single final string to actually send
// for generation. A standalone leaf package (no dependencies of its own)
// so it can sit underneath both internal/speakerattr (which validates an
// LLM's output against it) and internal/store (which resolves a
// paragraph's final generation text with it) without either importing the
// other.
package deliverytags

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// tagPattern matches one <|category:value|> tag, anchored to the start of
// whatever string it's tested against (see ExtractInsertions, which always
// tests a suffix of the annotated string) - category/value are always
// lowercase-with-underscores in every tag this app's own prompts ever ask
// for or accept (see internal/speakerattr's validSentenceTags/
// validInlineTags), matching Higgs's own real vocabulary exactly.
var tagPattern = regexp.MustCompile(`^<\|[a-z_]+:[a-z_]+\|>`)

// Insertion is one delivery tag found in an annotated variant of some
// plain text, anchored to the byte offset into plain it was inserted
// immediately before.
type Insertion struct {
	Offset int
	Tag    string
}

// ExtractInsertions diffs annotated against plain (their common ancestor -
// always a paragraph's own store.Paragraph.Text) and returns every
// delivery tag annotated inserts, each anchored to its own offset into
// plain, in the order they appear. Returns an error if annotated diverges
// from plain in any other way (a word added, removed, reordered, or
// changed) - callers (internal/speakerattr's own tagging passes) must
// treat that as untrusted LLM output and drop it entirely, never persist
// or apply it.
//
// Byte-indexed throughout, not rune-indexed - safe even though book text
// is full of multi-byte UTF-8 (curly quotes, em-dashes, accents): every
// byte of a multi-byte rune is either part of a tag match (always pure
// ASCII) or compared/copied verbatim one byte at a time against plain, so
// a multi-byte rune's own bytes are never split across a tag boundary or
// otherwise reinterpreted - by the time all of one rune's bytes are
// consumed the same way on both sides, both offsets land back on a valid
// rune boundary regardless.
func ExtractInsertions(plain, annotated string) ([]Insertion, error) {
	var out []Insertion
	pi, ai := 0, 0
	for ai < len(annotated) {
		if loc := tagPattern.FindStringIndex(annotated[ai:]); loc != nil {
			out = append(out, Insertion{Offset: pi, Tag: annotated[ai+loc[0] : ai+loc[1]]})
			ai += loc[1]
			continue
		}
		if pi >= len(plain) || annotated[ai] != plain[pi] {
			return nil, fmt.Errorf("annotated text diverges from paragraph text at byte %d", ai)
		}
		ai++
		pi++
	}
	if pi != len(plain) {
		return nil, fmt.Errorf("annotated text is missing %d trailing byte(s) of the paragraph's own text", len(plain)-pi)
	}
	return out, nil
}

// Merge rebuilds plain with every insertion from every given set spliced
// back in, sorted by Offset - ties (two insertions, from different sets,
// landing at the exact same point - e.g. a sentence-level emotion tag and
// an inline sfx tag both anchored to a line's very first word) are broken
// by which set argument came first, so a caller passing sentence-level
// insertions before positional ones gets them written in that order,
// matching Higgs's own documented stacking convention (an emotion tag
// immediately followed by an sfx tag at the same position - see
// PROMPTING.md's "stacking tags" example).
func Merge(plain string, sets ...[]Insertion) string {
	type ranked struct {
		Insertion
		setIdx int
	}
	var all []ranked
	for si, set := range sets {
		for _, ins := range set {
			all = append(all, ranked{ins, si})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Offset != all[j].Offset {
			return all[i].Offset < all[j].Offset
		}
		return all[i].setIdx < all[j].setIdx
	})
	var b strings.Builder
	pos := 0
	for _, ins := range all {
		b.WriteString(plain[pos:ins.Offset])
		b.WriteString(ins.Tag)
		pos = ins.Offset
	}
	b.WriteString(plain[pos:])
	return b.String()
}
