package llamacpp_test

// Correctness coverage for the prefix-reuse primitives (Context.
// DecodePrompt/TrimSequence/GenerateFrom) added for internal/speakerattr's
// benefit (backend/CLAUDE.md's "Speaker attribution" section) - reusing one
// Context's KV cache across independent calls that share a fixed prompt
// prefix, instead of re-decoding that prefix from scratch every time. The
// whole point is that this must be byte-for-byte equivalent to the naive
// "fresh Context, full re-decode" approach it replaces - these tests prove
// that under deterministic (temp 0, greedy) sampling rather than just
// asserting "it runs".

import (
	"testing"

	"github.com/rhino1998/lectable/llamacpp-go/llamacpp"
)

// freshGenerate decodes the full prompt from scratch in a brand-new
// Context and generates maxTokens greedily - the baseline every
// prefix-reuse call must match exactly.
func freshGenerate(t *testing.T, model *llamacpp.Model, prompt string, maxTokens int) string {
	t.Helper()
	ctx, err := model.NewContext(llamacpp.ContextParams{NCtx: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	sampler := llamacpp.NewSampler(llamacpp.SamplerParams{}) // Temp 0 => greedy, deterministic
	defer sampler.Close()
	out, err := ctx.Generate(prompt, sampler, maxTokens, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestGenerateFromMatchesFreshGenerate primes one Context with a fixed
// prefix once, then reuses it (TrimSequence + GenerateFrom) across several
// different full prompts that all share that same prefix text - each
// result must exactly match what a brand-new Context, given that same full
// prompt, would produce via the ordinary Generate path. If it didn't, the
// KV cache left behind by trimming/reusing would be silently corrupting
// generation - this is the test that would catch that.
func TestGenerateFromMatchesFreshGenerate(t *testing.T) {
	initBackend()
	modelPath := testModelPath(t)

	model, err := llamacpp.LoadModel(modelPath, llamacpp.ModelParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	const prefix = "You are a helpful assistant that follows instructions exactly and replies tersely.\n\n"
	suffixes := []string{
		"Reply with exactly the word: hello",
		"Reply with exactly the word: goodbye",
		"What is 2+2? Reply with only the digit.",
	}

	primed, err := model.NewContext(llamacpp.ContextParams{NCtx: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer primed.Close()

	prefixLen, err := primed.DecodePrompt(prefix)
	if err != nil {
		t.Fatalf("DecodePrompt: %v", err)
	}
	if prefixLen == 0 {
		t.Fatal("DecodePrompt returned 0 tokens for a non-empty prefix")
	}

	for _, suffix := range suffixes {
		full := prefix + suffix
		want := freshGenerate(t, model, full, 12)

		if ok := primed.TrimSequence(0, prefixLen); !ok {
			t.Fatalf("TrimSequence(0, %d) returned false", prefixLen)
		}
		sampler := llamacpp.NewSampler(llamacpp.SamplerParams{})
		got, err := primed.GenerateFrom(full, prefixLen, sampler, 12, nil)
		sampler.Close()
		if err != nil {
			t.Fatalf("GenerateFrom(%q): %v", suffix, err)
		}

		if got != want {
			t.Errorf("suffix %q: GenerateFrom (primed/trimmed) = %q, want %q (fresh Generate)", suffix, got, want)
		}
	}
}

// TestGenerateFromStartPosZeroMatchesGenerate is the degenerate case
// (nothing actually cached yet, startPos=0) - GenerateFrom must behave
// identically to Generate, since Generate is now defined in terms of it.
func TestGenerateFromStartPosZeroMatchesGenerate(t *testing.T) {
	initBackend()
	modelPath := testModelPath(t)

	model, err := llamacpp.LoadModel(modelPath, llamacpp.ModelParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	const prompt = "Reply with exactly the word: hello"

	ctx, err := model.NewContext(llamacpp.ContextParams{NCtx: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	sampler := llamacpp.NewSampler(llamacpp.SamplerParams{})
	defer sampler.Close()

	got, err := ctx.GenerateFrom(prompt, 0, sampler, 12, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := freshGenerate(t, model, prompt, 12)
	if got != want {
		t.Errorf("GenerateFrom(prompt, 0, ...) = %q, want %q", got, want)
	}
}
