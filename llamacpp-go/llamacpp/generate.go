package llamacpp

import (
	"fmt"
	"strings"
)

// Generate tokenizes prompt, decodes it, and then repeatedly samples and
// decodes one token at a time (feeding each sampled token back in as the
// next input) until sampler produces an end-of-generation token, maxTokens
// tokens have been generated, or onPiece returns false. onPiece is called
// with each generated token's text as it's produced and may be nil.
// Generate returns the full generated text (everything passed to onPiece,
// concatenated), regardless of which stop condition ended the loop.
//
// It always runs the prompt and every generated token in sequence 0 --
// build the batch/decode loop by hand (see Batch and Context.Decode) for
// multi-sequence use. Equivalent to GenerateFrom(prompt, 0, ...) -- see
// that method's own doc comment for reusing a Context across independent
// prompts that share a fixed prefix.
func (c *Context) Generate(prompt string, sampler *Sampler, maxTokens int, onPiece func(piece string) bool) (string, error) {
	return c.GenerateFrom(prompt, 0, sampler, maxTokens, onPiece)
}

// DecodePrompt tokenizes and decodes prompt into sequence 0 starting at
// position 0, without sampling any continuation. It's for priming a
// Context's KV cache with a fixed, reusable prefix (e.g. a constant system
// prompt) ahead of one or more later GenerateFrom calls -- the returned
// token count is the position a later GenerateFrom's startPos should use,
// and the p0 TrimSequence should be given to reclaim everything decoded
// after it once each of those calls is done.
func (c *Context) DecodePrompt(prompt string) (int32, error) {
	return c.DecodePromptSeq(prompt, 0)
}

// DecodePromptSeq is DecodePrompt, but decodes into seqID instead of always
// sequence 0 -- the primitive a Scheduler (see scheduler.go) uses to prime a
// dedicated, permanently-resident sequence with a shared prefix once, ahead
// of copying it (Context.CopySeq) into each of several concurrent
// generations' own sequences rather than decoding it redundantly for each.
func (c *Context) DecodePromptSeq(prompt string, seqID int32) (int32, error) {
	tokens, err := c.model.Vocab().Tokenize(prompt, true, true)
	if err != nil {
		return 0, fmt.Errorf("llamacpp: tokenize prompt: %w", err)
	}
	if len(tokens) == 0 {
		return 0, fmt.Errorf("llamacpp: empty prompt tokenized to zero tokens")
	}
	batch := NewBatch(len(tokens))
	defer batch.Close()
	for i, tok := range tokens {
		if err := batch.Add(tok, int32(i), seqID, false); err != nil {
			return 0, fmt.Errorf("llamacpp: build prompt batch: %w", err)
		}
	}
	if err := c.Decode(batch); err != nil {
		return 0, fmt.Errorf("llamacpp: decode prompt: %w", err)
	}
	return int32(len(tokens)), nil
}

// GenerateFrom is Generate, but assumes the first startPos tokens of
// prompt's own tokenization are already present in this Context's KV
// cache at sequence 0 (from a prior DecodePrompt/Generate/GenerateFrom
// call against this same Context) and only decodes/generates from
// startPos onward -- the counterpart to TrimSequence for reusing a
// Context across independent prompts that share a fixed prefix: prime it
// once with DecodePrompt, then before each later call needing that same
// prefix, TrimSequence(0, startPos) to discard whatever the previous call
// left past it, and call GenerateFrom(fullPrompt, startPos, ...) instead
// of re-decoding fullPrompt's own prefix tokens from scratch. prompt is
// still tokenized in full (not just its own suffix) on every call --
// tokenization is cheap CPU work, and doing it this way (rather than
// tokenizing only the new suffix text on its own) sidesteps any risk of a
// subword merge landing differently right at the prefix/suffix text
// boundary than it would in one continuous tokenization pass; only the
// already-cached prefix *tokens* are skipped, never re-tokenized text.
// Callers that reuse a Context this way are responsible for verifying
// prompt's own first startPos tokens actually still match whatever was
// decoded there before calling this (e.g. by keeping the token slice
// DecodePrompt's caller tokenized and comparing it token-for-token) --
// GenerateFrom itself has no way to detect a mismatch and would otherwise
// silently generate from a corrupted context.
func (c *Context) GenerateFrom(prompt string, startPos int32, sampler *Sampler, maxTokens int, onPiece func(piece string) bool) (string, error) {
	vocab := c.model.Vocab()
	tokens, err := vocab.Tokenize(prompt, true, true)
	if err != nil {
		return "", fmt.Errorf("llamacpp: tokenize prompt: %w", err)
	}
	if len(tokens) == 0 {
		return "", fmt.Errorf("llamacpp: empty prompt tokenized to zero tokens")
	}
	if int(startPos) > len(tokens) {
		return "", fmt.Errorf("llamacpp: startPos %d exceeds prompt token count %d", startPos, len(tokens))
	}

	batch := NewBatch(len(tokens) - int(startPos))
	defer batch.Close()

	for i := int(startPos); i < len(tokens); i++ {
		if err := batch.Add(tokens[i], int32(i), 0, i == len(tokens)-1); err != nil {
			return "", fmt.Errorf("llamacpp: build prompt batch: %w", err)
		}
	}
	if batch.NTokens() > 0 {
		if err := c.Decode(batch); err != nil {
			return "", fmt.Errorf("llamacpp: decode prompt: %w", err)
		}
	}

	var out strings.Builder
	pos := int32(len(tokens))
	for n := 0; n < maxTokens; n++ {
		next := sampler.Sample(c, -1)
		if vocab.IsEOG(next) {
			break
		}

		piece := vocab.TokenToPiece(next, false)
		out.WriteString(piece)
		if onPiece != nil && !onPiece(piece) {
			break
		}

		batch.Reset()
		if err := batch.Add(next, pos, 0, true); err != nil {
			return out.String(), fmt.Errorf("llamacpp: build generation batch: %w", err)
		}
		if err := c.Decode(batch); err != nil {
			return out.String(), fmt.Errorf("llamacpp: decode generated token: %w", err)
		}
		pos++
	}
	return out.String(), nil
}
