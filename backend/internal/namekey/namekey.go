// Package namekey decides when two spellings are the same character name:
// the same ignoring case and which apostrophe is used. A book's own text
// says "Ve’delina" (a typographic apostrophe) where the LLM, a reader's
// keyboard or another chapter's markup says "Ve'delina", and those were
// two roster rows with two voices. A leaf package, so the store and
// speakerattr compare names the same way without importing each other.
package namekey

import "strings"

// apostrophes maps every mark set as an apostrophe to the plain one:
// the curly quotes, the modifier letters, prime, and the accents typed
// in their place.
var apostrophes = strings.NewReplacer(
	"’", "'", "‘", "'", "‛", "'",
	"ʼ", "'", "ʻ", "'", "′", "'",
	"＇", "'", "`", "'", "´", "'",
)

// Fold is the form of name two spellings of one name share: lower-cased,
// with any apostrophe a plain one. For comparing and keying, never for
// display.
func Fold(name string) string {
	return apostrophes.Replace(strings.ToLower(name))
}

// Equal reports whether a and b are one name (see Fold).
func Equal(a, b string) bool {
	return a == b || Fold(a) == Fold(b)
}
