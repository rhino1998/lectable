package speakerattr

import (
	"regexp"
	"strings"
)

// WorldNote is one recorded fact about a story's world - store.WorldFact's
// own shape, kept as a plain value here rather than importing store (see
// ParagraphInput).
type WorldNote struct {
	Key     string
	Aliases []string
	Fact    string
}

// maxWorldNotes caps how many notes one prompt is shown.
const maxWorldNotes = 8

// MatchWorldNotes returns, as "key: fact" lines for a prompt's "World
// notes", the notes whose key or an alias appears as a whole word in any
// of texts - in any case, a plain plural included ("kobold" finds
// "Kobolds") - in notes' own order, at most maxWorldNotes. A fact about
// kobolds reaches the casting of a character the material calls one, and
// nobody else's.
func MatchWorldNotes(notes []WorldNote, texts ...string) []string {
	if len(notes) == 0 {
		return nil
	}
	material := strings.Join(texts, "\n")
	var out []string
	for _, n := range notes {
		var words []string
		for _, w := range append([]string{n.Key}, n.Aliases...) {
			if w = strings.TrimSpace(w); w != "" {
				words = append(words, regexp.QuoteMeta(w))
			}
		}
		if len(words) == 0 || strings.TrimSpace(n.Fact) == "" {
			continue
		}
		re := regexp.MustCompile(`(?i)\b(?:` + strings.Join(words, "|") + `)(?:s|es)?\b`)
		if !re.MatchString(material) {
			continue
		}
		out = append(out, n.Key+": "+strings.TrimSpace(n.Fact))
		if len(out) == maxWorldNotes {
			break
		}
	}
	return out
}
