package speakerattr

import (
	"fmt"
	"strings"
)

// askedLine reports whether attribution asks about p: an unsettled line
// of dialogue.
func askedLine(p ParagraphInput) bool {
	return p.IsQuote && p.Known == "" && !p.ContextOnly
}

// knownLabel is a settled line's speaker as a prompt shows it: the name,
// with a role's number.
func knownLabel(p ParagraphInput) string {
	if p.KnownIndex > 0 {
		return fmt.Sprintf("%s %d", p.Known, p.KnownIndex)
	}
	return p.Known
}

// prevNextNote is the "prev/next" note under the asked line lines[i]
// (AttributeOptions.PrevNext): the nearest settled dialogue line on each
// side within the batch, "" when there is neither.
func prevNextNote(lines []ParagraphInput, i int) string {
	var notes []string
	for j := i - 1; j >= 0; j-- {
		if p := lines[j]; p.IsQuote && p.Known != "" {
			notes = append(notes, fmt.Sprintf("prev: %s (%d)", knownLabel(p), p.Idx))
			break
		}
	}
	for j := i + 1; j < len(lines); j++ {
		if p := lines[j]; p.IsQuote && p.Known != "" {
			notes = append(notes, fmt.Sprintf("next: %s (%d)", knownLabel(p), p.Idx))
			break
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return "    " + strings.Join(notes, " · ") + "\n"
}
