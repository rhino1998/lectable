package speakerattr

import (
	"encoding/json"
	"log"
	"regexp"
)

// repairDroppedClosingQuoteInArray is a targeted, narrow fixup for one
// specific, confirmed-in-production malformed-JSON pattern, not a general
// JSON repair tool: a model reproduces a paragraph's own text verbatim
// into a JSON string value (e.g. a line of quoted dialogue that itself
// ends in a curly closing quote, ”), correctly emits that quote as part
// of the string's own content (fine, no escaping needed - it's not the
// ASCII "), but then omits the actual ASCII " that should close the JSON
// string itself. encoding/json's own scanner then keeps consuming
// everything after it - "}, {" and on - as part of that same string,
// until it hits the next real " in the stream: the opening quote of the
// *next* object's "idx" key. It treats that as the string's own closing
// quote, then expects a comma or brace right where "idx" itself begins,
// and fails with something like "invalid character 'i' after object
// key:value pair".
//
// This is a real, 100% reproducible failure for the exact batch content
// that triggers it, not sampling noise: parseAttributions (this function's
// only remaining caller, via unmarshalRepairedArray - see its own doc
// comment for why direction/sfx tagging's own former caller, parseTaggedText,
// no longer needs any of this file at all) runs its batch at temp 0
// (greedy/deterministic - see retryTempBump's own doc comment), so the old
// "just call generate again" retry could never actually recover from this -
// the model decodes the identical dropped-quote tokens every single time
// for that prompt.
//
// Scoped narrowly to the one shape its caller actually has: a flat JSON
// array of objects whose *last* field is a plain string (parseAttributions'
// "speaker") - so a
// syntactically valid object in this shape always closes as `"}` (the
// last field's own closing quote, then the object's brace), and a
// syntactically valid array always ends as `"}]` (that same object-close,
// then the array's own bracket) or `"}, {"idx"` (that object-close, then
// the next object starting). If the caller's own encoding/json.Unmarshal
// already failed, look for a `}` immediately preceded by something other
// than an unescaped ASCII quote, digit, or whitespace, sitting right
// where one of those two legitimate continuations follows, and insert the
// missing ASCII " right before it.
//
// Deliberately conservative in both directions:
//   - It never fires on a `}` already preceded by a quote/digit/whitespace
//     (nothing to repair there - a digit means this is a different field
//     shape entirely, e.g. an idx-only object, not the dropped-quote
//     pattern this targets).
//   - The caller (unmarshalRepairedArray) only ever reaches for this after
//     encoding/json's own Unmarshal has already failed on the raw text,
//     and re-validates the repaired text through that same Unmarshal
//     before trusting any of it - so a batch this heuristic guesses wrong
//     about just fails to parse, exactly as it would have with no repair
//     attempted at all, rather than silently producing wrong data.
func repairDroppedClosingQuoteInArray(s string) string {
	s = reMissingQuoteBeforeNextObject.ReplaceAllString(s, `$1"}$2`)
	s = reMissingQuoteBeforeArrayEnd.ReplaceAllString(s, `$1"}$2`)
	return s
}

var (
	// e.g. `...to heal.”}, {"idx": 105, ...` -> `...to heal.”"}, {"idx": 105, ...`
	reMissingQuoteBeforeNextObject = regexp.MustCompile(`([^"\d\s}])\}(\s*,\s*\{\s*"idx"\s*:)`)
	// e.g. `...Jake,”}]` (the batch's last object) -> `...Jake,”"}]`
	reMissingQuoteBeforeArrayEnd = regexp.MustCompile(`([^"\d\s}])\}(\s*\])`)
)

// unmarshalRepairedArray tries json.Unmarshal on raw first, same as any
// direct call; only on failure does it retry once against
// repairDroppedClosingQuoteInArray's own repair applied - a repair that
// doesn't actually match is a no-op, so running it unconditionally on any
// failure is safe. Returns the *original* error when the repair didn't
// change anything, or the result still doesn't parse - callers never see
// a confusing error about repaired-but-still-broken text, just the same
// failure a plain json.Unmarshal would have reported.
//
// Used to have a second repair here too, repairMissingOpeningQuoteInArray -
// removed once parseAttributions became this function's only remaining
// caller (see repairDroppedClosingQuoteInArray's own doc comment): that
// second repair anchored specifically on a literal `"text":` key, which
// only ever existed in parseTaggedText's own JSON shape, never
// parseAttributions' (idx/speaker/role, no "text" field at all) - so it
// could no longer fire for anything reachable. If a future caller needs
// that same "model uses its own curly quotes as the JSON delimiter"
// repair, it's still in this file's git history to resurrect rather than
// carried forward unreachable.
func unmarshalRepairedArray(raw string, v any) error {
	err := json.Unmarshal([]byte(raw), v)
	if err == nil {
		return nil
	}
	repaired := repairDroppedClosingQuoteInArray(raw)
	if repaired == raw {
		return err
	}
	if rerr := json.Unmarshal([]byte(repaired), v); rerr == nil {
		log.Printf("speakerattr: repaired a malformed JSON array (missing string delimiter(s)) that would otherwise have failed to parse")
		return nil
	}
	return err
}
