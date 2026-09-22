package speakerattr

import "testing"

// TestParseTaggedLinesRealFailure reproduces the exact production failure
// that motivated moving direction/sfx tagging off JSON entirely: chapter
// idx 66 of "Jake's Magical Market 3" ("Chapter 62" by its own displayed
// title), paragraph 115, whose entire real text is the short curly-quoted
// line “Not too bad,”. Under the old JSON design the model kept
// substituting its own curly quotes for the JSON string's ASCII
// delimiters while inserting a tag right after its own text - e.g.
// `{"idx": 115, "text": "Not too bad,"<|prosody:pause|>"}` - which ended
// the JSON string early and left `<|prosody:pause|>"` as invalid trailing
// syntax. Because JSON has to parse as one whole document, that single
// line permanently failed the *entire* batch (paragraphs 110-117) after 3
// retries, every single time it was re-run (confirmed live, four separate
// task runs in one evening, all dying on this same batch).
//
// The new line format has no JSON string to terminate early, so paragraph
// 115's own line parses without error - though it still correctly ends up
// untagged: the tag lands at the very end of paragraph 115's own text with
// nothing left to color (see parseTaggedLines' own trailing-insertion
// rule, unchanged from the old parseTaggedText) - paragraph 115 is just
// the standalone quote "“Not too bad,”", with the "I said..." dialogue tag
// that would give the pause somewhere to land a *separate* paragraph
// (116). That's a pre-existing, correct limitation of tagging one
// paragraph at a time, not something this format change fixes or
// regresses. What actually changes is the paragraph *after* it in the
// same response: under the old design this whole batch died with it;
// here it parses and keeps its own valid tag regardless.
func TestParseTaggedLinesRealFailure(t *testing.T) {
	orig := map[int]string{
		115: "“Not too bad,”",
		116: "I said, tucking the ribbon into my satchel.",
	}
	// <|prosody:pause|> is an inline tag (validInlineTags/sfx.go's own
	// TagSfx pass), not a sentence tag - matching which of the two LLM
	// passes directChapter's own "both passes" run actually produced this
	// real response.
	content := "115: “Not too bad,”<|prosody:pause|>\n" +
		"116: <|prosody:pause|>I said, tucking the ribbon into my satchel."

	out, err := parseTaggedLines(content, orig, validInlineTags)
	if err != nil {
		t.Fatalf("parseTaggedLines: %v", err)
	}
	if _, ok := out[115]; ok {
		t.Errorf("expected idx 115's trailing-only tag to be dropped (nothing left to color), got %q", out[115])
	}
	if want := "<|prosody:pause|>I said, tucking the ribbon into my satchel."; out[116] != want {
		t.Fatalf("out[116] = %q, want %q - paragraph 115's own unfixable trailing tag must not cost its sibling paragraph's valid one", out[116], want)
	}
}

// TestParseTaggedLinesFaultIsolation is the core robustness property this
// format was chosen for over JSON: one malformed/hallucinated line in a
// batch only costs that line, not every other line's own valid tagging in
// the same response - unlike a single JSON array, where one bad line broke
// json.Unmarshal for the whole batch.
func TestParseTaggedLinesFaultIsolation(t *testing.T) {
	orig := map[int]string{
		10: "He walked in.",
		11: "“Stop!”",
		12: "She sighed. She left.",
	}
	// Line 11 is garbled - reworded, not just tagged (extra invented word
	// "please") - deliverytags.ExtractInsertions must reject it without
	// touching lines 10/12, which are otherwise perfectly valid. Line 12's
	// tag sits mid-line, between its own two sentences, not at the very
	// end - a real insertion point, unlike TestParseTaggedLinesRealFailure's
	// own paragraph 115.
	content := `10: <|style:shouting|>He walked in.
11: "Stop please!"
12: She sighed. <|prosody:expressive_high|>She left.`

	out, err := parseTaggedLines(content, orig, validSentenceTags)
	if err != nil {
		t.Fatalf("parseTaggedLines: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 surviving entries, got %d: %+v", len(out), out)
	}
	if out[10] != "<|style:shouting|>He walked in." {
		t.Errorf("out[10] = %q", out[10])
	}
	if out[12] != "She sighed. <|prosody:expressive_high|>She left." {
		t.Errorf("out[12] = %q", out[12])
	}
	if _, ok := out[11]; ok {
		t.Errorf("expected idx 11 (reworded, not just tagged) to be dropped, got %q", out[11])
	}
}

// TestParseTaggedLinesIgnoresReasoningPreamble covers the model's own
// observed habit of writing free-form line-by-line reasoning ("Line 34:
// ...no tag.") before its real answer, even without a formal <think>
// trace - text that superficially resembles this format's own "<idx>:
// <text>" answer lines but isn't one. Real answer lines are still found
// and parsed regardless of what precedes them, and prose that merely
// starts with a number is rejected by the same idx-membership/pure-
// insertion validation every real line goes through, not by requiring an
// explicit preamble/answer boundary marker.
func TestParseTaggedLinesIgnoresReasoningPreamble(t *testing.T) {
	orig := map[int]string{34: "He shrugged."}
	content := "Let me think through this line by line.\n" +
		"Line 34: \"He shrugged.\" - no strong delivery cue here, no tag needed based on strict rules.\n" +
		"Wait, on reflection this line does call for a tag after all.\n" +
		"34: <|prosody:speed_slow|>He shrugged.\n"

	out, err := parseTaggedLines(content, orig, validSentenceTags)
	if err != nil {
		t.Fatalf("parseTaggedLines: %v", err)
	}
	if out[34] != "<|prosody:speed_slow|>He shrugged." {
		t.Fatalf("out[34] = %q", out[34])
	}
}

// TestParseTaggedLinesRejectsInvalidTag confirms a line inserting a tag
// outside the caller's own validTags set is dropped entirely, same as the
// old JSON parser.
func TestParseTaggedLinesRejectsInvalidTag(t *testing.T) {
	orig := map[int]string{5: "Hello there."}
	// <|sfx:laughter|> is an inline tag, not a valid sentence tag.
	content := "5: <|sfx:laughter|>Hello there."

	out, err := parseTaggedLines(content, orig, validSentenceTags)
	if err != nil {
		t.Fatalf("parseTaggedLines: %v", err)
	}
	if _, ok := out[5]; ok {
		t.Fatalf("expected idx 5 to be dropped for an out-of-vocabulary tag, got %q", out[5])
	}
}

// TestParseTaggedLinesDropsTrailingTag mirrors the old parseTaggedText's
// own trailing-insertion rule: a tag landing at the very end of the line,
// with nothing left to color, is stripped rather than kept.
func TestParseTaggedLinesDropsTrailingTag(t *testing.T) {
	orig := map[int]string{7: "Hello there."}
	// <|prosody:expressive_high|>, not <|prosody:pause|>: this must be a
	// tag validSentenceTags actually accepts, so the trailing-offset check
	// is what drops it - not an incidental out-of-vocabulary rejection
	// (see TestParseTaggedLinesRejectsInvalidTag for that, separately).
	content := "7: Hello there.<|prosody:expressive_high|>"

	out, err := parseTaggedLines(content, orig, validSentenceTags)
	if err != nil {
		t.Fatalf("parseTaggedLines: %v", err)
	}
	if _, ok := out[7]; ok {
		t.Fatalf("expected idx 7's trailing-only tag to be dropped entirely, got %q", out[7])
	}
}

// TestParseTaggedLinesEmptyResponseIsNotAnError covers the overwhelmingly
// common case both system prompts explicitly ask for: most batches tag
// nothing at all. Unlike the old JSON design (which required at least an
// explicit `[]` to avoid a "no JSON array found" error), a response with
// no matching lines is just an ordinary empty result.
func TestParseTaggedLinesEmptyResponseIsNotAnError(t *testing.T) {
	orig := map[int]string{1: "Fine as is.", 2: "Also fine."}
	out, err := parseTaggedLines("Nothing here needs a delivery tag.", orig, validSentenceTags)
	if err != nil {
		t.Fatalf("parseTaggedLines: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected no entries, got %+v", out)
	}
}

func TestTaggedReplyBudget(t *testing.T) {
	byIdx := map[int]string{1: string(make([]byte, 1000)), 2: string(make([]byte, 500))}
	maxChars, maxTokens := taggedReplyBudget(byIdx, directionMaxTokens)
	if want := 1500 + 2*taggedReplyLineAllowance; maxChars != want {
		t.Fatalf("maxChars = %d; want %d", maxChars, want)
	}
	if want := maxChars/2 + taggedReplyHeadroomTokens; maxTokens != want {
		t.Fatalf("maxTokens = %d; want %d", maxTokens, want)
	}
	if _, capped := taggedReplyBudget(byIdx, 100); capped != 100 {
		t.Fatalf("maxTokens = %d; want capped at 100", capped)
	}
}

func TestTaggedReplyParserRetriesRunawayThenKeepsValidLines(t *testing.T) {
	orig := map[int]string{3: "Stop right there!"}
	valid := map[string]bool{"<|style:shouting|>": true}
	good := "3: <|style:shouting|>Stop right there!"
	runaway := good + "\n" + string(make([]byte, 400))

	parse := taggedReplyParser(orig, valid, len(good)+10, "test")
	for attempt := 1; attempt <= maxGenerateRetries; attempt++ {
		if _, err := parse(runaway); err == nil {
			t.Fatalf("attempt %d: runaway accepted; want an error so generateAndParse retries", attempt)
		}
	}
	out, err := parse(runaway)
	if err != nil {
		t.Fatalf("final attempt: %v; want the valid lines kept", err)
	}
	if out[3] != good[3:] {
		t.Fatalf("out[3] = %q; want %q", out[3], good[3:])
	}

	if out, err := taggedReplyParser(orig, valid, len(good)+10, "test")(good); err != nil || out[3] == "" {
		t.Fatalf("normal reply = %v, %v; want accepted", out, err)
	}
}
