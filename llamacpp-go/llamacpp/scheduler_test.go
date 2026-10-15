package llamacpp_test

// Correctness coverage for Scheduler (scheduler.go), the multi-sequence
// batching primitive backing backend/internal/llmworker's concurrent
// speaker-attribution/characterization/direction-tagging support. The bar
// is the same one prefixcache_test.go already sets for single-sequence
// prefix reuse: every result, however many requests ran concurrently
// through the shared Context, must exactly match what a brand-new
// single-sequence Context given the same prompt would produce under
// deterministic (temp 0, greedy) sampling.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rhino1998/lectable/llamacpp-go/llamacpp"
)

func TestSchedulerMatchesFreshGenerateConcurrently(t *testing.T) {
	initBackend()
	modelPath := testModelPath(t)

	model, err := llamacpp.LoadModel(modelPath, llamacpp.ModelParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	const nSlots = 4
	sctx, err := model.NewContext(llamacpp.ContextParams{NCtx: 2048, NSeqMax: nSlots})
	if err != nil {
		t.Fatal(err)
	}
	defer sctx.Close()

	sched := llamacpp.NewScheduler(sctx, nSlots)
	defer sched.Close()

	prompts := []string{
		"Reply with exactly the word: hello",
		"Reply with exactly the word: goodbye",
		"What is 2+2? Reply with only the digit.",
		"What is the capital of France? Reply with only the city name.",
		"Spell the word cat, one letter per line.",
		"Reply with exactly the word: pineapple",
	}

	want := make([]string, len(prompts))
	for i, p := range prompts {
		want[i] = freshGenerate(t, model, p, 12)
	}

	var wg sync.WaitGroup
	got := make([]string, len(prompts))
	errs := make([]error, len(prompts))
	for i, p := range prompts {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sampler := llamacpp.NewSampler(llamacpp.SamplerParams{})
			defer sampler.Close()
			got[i], errs[i] = sched.Generate(t.Context(), llamacpp.GenRequest{
				Prompt:    p,
				Sampler:   sampler,
				MaxTokens: 12,
			})
		}(i, p)
	}
	wg.Wait()

	for i, p := range prompts {
		if errs[i] != nil {
			t.Errorf("prompt %q: Generate error: %v", p, errs[i])
			continue
		}
		if got[i] != want[i] {
			t.Errorf("prompt %q: Scheduler.Generate = %q, want %q (fresh single-sequence Generate)", p, got[i], want[i])
		}
	}
}

// TestSchedulerWithPrimedPrefix mirrors prefixcache_test.go's
// TestGenerateFromMatchesFreshGenerate, but through the Scheduler: a fixed
// prefix is decoded once into its own dedicated sequence, then several
// concurrent requests each copy it (Context.CopySeq, via GenRequest.
// PrimedSeq/StartPos) into their own generation slot instead of decoding it
// themselves - the mechanism backend/internal/llmworker uses to reuse
// speakerattr's own constant system prompts across concurrent callers.
func TestSchedulerWithPrimedPrefix(t *testing.T) {
	initBackend()
	modelPath := testModelPath(t)

	model, err := llamacpp.LoadModel(modelPath, llamacpp.ModelParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	const (
		nSlots    = 3
		primedSeq = nSlots // dedicated sequence id outside the Scheduler's own slot pool
	)
	sctx, err := model.NewContext(llamacpp.ContextParams{NCtx: 2048, NSeqMax: nSlots + 1, KVUnified: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sctx.Close()

	const prefix = "You are a helpful assistant that follows instructions exactly and replies tersely.\n\n"
	prefixLen, err := sctx.DecodePromptSeq(prefix, primedSeq)
	if err != nil {
		t.Fatalf("DecodePromptSeq: %v", err)
	}

	sched := llamacpp.NewScheduler(sctx, nSlots)
	defer sched.Close()

	suffixes := []string{
		"Reply with exactly the word: hello",
		"Reply with exactly the word: goodbye",
		"What is 2+2? Reply with only the digit.",
	}

	var wg sync.WaitGroup
	got := make([]string, len(suffixes))
	want := make([]string, len(suffixes))
	errs := make([]error, len(suffixes))
	for i, suffix := range suffixes {
		full := prefix + suffix
		want[i] = freshGenerate(t, model, full, 12)

		wg.Add(1)
		go func(i int, full string) {
			defer wg.Done()
			sampler := llamacpp.NewSampler(llamacpp.SamplerParams{})
			defer sampler.Close()
			got[i], errs[i] = sched.Generate(t.Context(), llamacpp.GenRequest{
				Prompt:    full,
				StartPos:  prefixLen,
				PrimedSeq: primedSeq,
				Sampler:   sampler,
				MaxTokens: 12,
			})
		}(i, full)
	}
	wg.Wait()

	for i, suffix := range suffixes {
		if errs[i] != nil {
			t.Errorf("suffix %q: Generate error: %v", suffix, errs[i])
			continue
		}
		if got[i] != want[i] {
			t.Errorf("suffix %q: Scheduler.Generate (primed) = %q, want %q (fresh Generate)", suffix, got[i], want[i])
		}
	}
}

// TestSchedulerMoreRequestsThanSlots checks that requests beyond nSlots
// correctly queue and run in turn rather than being dropped or corrupting
// an in-flight slot's state.
func TestSchedulerMoreRequestsThanSlots(t *testing.T) {
	initBackend()
	modelPath := testModelPath(t)

	model, err := llamacpp.LoadModel(modelPath, llamacpp.ModelParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	const nSlots = 2
	sctx, err := model.NewContext(llamacpp.ContextParams{NCtx: 2048, NSeqMax: nSlots})
	if err != nil {
		t.Fatal(err)
	}
	defer sctx.Close()

	sched := llamacpp.NewScheduler(sctx, nSlots)
	defer sched.Close()

	const nRequests = 6 // 3x nSlots
	var wg sync.WaitGroup
	errs := make([]error, nRequests)
	for i := 0; i < nRequests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sampler := llamacpp.NewSampler(llamacpp.SamplerParams{})
			defer sampler.Close()
			out, err := sched.Generate(t.Context(), llamacpp.GenRequest{
				Prompt:    fmt.Sprintf("Reply with exactly the digit: %d", i),
				Sampler:   sampler,
				MaxTokens: 8,
			})
			if err == nil && out == "" {
				err = fmt.Errorf("empty output")
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("request %d: %v", i, err)
		}
	}
}

// TestSchedulerReclaimsSlotAfterOverflow: a request whose reply outgrows
// its sequence fails alone with ErrContextFull, and the slot it held is
// clean for the next one. Before admitInto held each request to its own
// sequence, the overflow failed the round's Decode instead ("failed to find
// a memory slot") - for every slot in the batch - and that path once left
// the failed request's tokens resident under its sequence id, so later
// requests in the slot failed the same way, hundreds in a row.
//
// nSlots=1 forces the second request to reuse the exact slot id the first,
// deliberately overflowed one just occupied; it must come out
// byte-identical to a fresh single-sequence Context given the same prompt.
func TestSchedulerReclaimsSlotAfterOverflow(t *testing.T) {
	initBackend()
	modelPath := testModelPath(t)

	model, err := llamacpp.LoadModel(modelPath, llamacpp.ModelParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	const (
		nSlots = 1
		// 256 is respected exactly (confirmed via Context.NCtx() against
		// this test's own model) - llama.cpp silently rounds a smaller
		// request up to some internal minimum floor instead of actually
		// allocating that few cells, which would defeat the deliberate
		// overflow below.
		nCtx = 256
	)
	sctx, err := model.NewContext(llamacpp.ContextParams{NCtx: nCtx, NSeqMax: nSlots})
	if err != nil {
		t.Fatal(err)
	}
	defer sctx.Close()
	if got := sctx.NCtx(); got != nCtx {
		t.Fatalf("Context.NCtx() = %d, want exactly %d (this test relies on that to force a real overflow) - adjust nCtx if this model/llama.cpp build rounds it differently", got, nCtx)
	}

	sched := llamacpp.NewScheduler(sctx, nSlots)
	defer sched.Close()

	overflowSampler := llamacpp.NewSampler(llamacpp.SamplerParams{})
	defer overflowSampler.Close()
	// MaxTokens (1000) comfortably exceeds nCtx (256) minus this prompt's
	// own token count, so the reply reaches the end of the sequence before
	// MaxTokens ever would.
	if _, err := sched.Generate(t.Context(), llamacpp.GenRequest{
		Prompt:    "Tell me a very long, detailed story about a dragon, at least 500 words.",
		Sampler:   overflowSampler,
		MaxTokens: 1000,
	}); !errors.Is(err, llamacpp.ErrContextFull) {
		t.Fatalf("the deliberately-overflowed request: err = %v, want ErrContextFull", err)
	}
	if _, err := sched.Generate(t.Context(), llamacpp.GenRequest{
		Prompt:    strings.Repeat("Tell me a very long, detailed story about a dragon. ", 100),
		Sampler:   overflowSampler,
		MaxTokens: 8,
	}); !errors.Is(err, llamacpp.ErrContextFull) {
		t.Fatalf("a prompt longer than the sequence: err = %v, want ErrContextFull", err)
	}

	const followUp = "Reply with exactly the word: hello"
	want := freshGenerate(t, model, followUp, 8)

	sampler := llamacpp.NewSampler(llamacpp.SamplerParams{})
	defer sampler.Close()
	got, err := sched.Generate(t.Context(), llamacpp.GenRequest{
		Prompt:    followUp,
		Sampler:   sampler,
		MaxTokens: 8,
	})
	if err != nil {
		t.Fatalf("follow-up request after a decode overflow: %v", err)
	}
	if got != want {
		t.Errorf("follow-up request reused a leaked slot: got %q, want %q (fresh Generate)", got, want)
	}
}

// TestSchedulerWithPrefixState is TestSchedulerWithPrimedPrefix for
// GenRequest.PrefixState: a non-unified Context (one KV stream per slot),
// the prefix snapshotted once via SaveSeq and restored into whichever slot
// each request lands in, must still match a fresh single-sequence Generate.
func TestSchedulerWithPrefixState(t *testing.T) {
	initBackend()
	modelPath := testModelPath(t)

	model, err := llamacpp.LoadModel(modelPath, llamacpp.ModelParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	const nSlots = 3
	sctx, err := model.NewContext(llamacpp.ContextParams{NCtx: 2048 * nSlots, NSeqMax: nSlots})
	if err != nil {
		t.Fatal(err)
	}
	defer sctx.Close()

	const prefix = "You are a helpful assistant that follows instructions exactly and replies tersely.\n\n"
	prefixLen, err := sctx.DecodePromptSeq(prefix, 0)
	if err != nil {
		t.Fatalf("DecodePromptSeq: %v", err)
	}
	state := sctx.SaveSeq(0)
	if state == nil {
		t.Fatal("SaveSeq returned no state")
	}
	sctx.TrimSequence(0, 0)

	sched := llamacpp.NewScheduler(sctx, nSlots)
	defer sched.Close()

	suffixes := []string{
		"Reply with exactly the word: hello",
		"Reply with exactly the word: goodbye",
		"What is 2+2? Reply with only the digit.",
		"Name one primary color. Reply with one word.",
		"Reply with exactly the word: lantern",
	}

	var wg sync.WaitGroup
	got := make([]string, len(suffixes))
	want := make([]string, len(suffixes))
	errs := make([]error, len(suffixes))
	for i, suffix := range suffixes {
		full := prefix + suffix
		want[i] = freshGenerate(t, model, full, 12)

		wg.Add(1)
		go func(i int, full string) {
			defer wg.Done()
			var stats llamacpp.GenStats
			got[i], errs[i] = sched.Generate(t.Context(), llamacpp.GenRequest{
				Prompt:      full,
				StartPos:    prefixLen,
				PrefixState: state,
				Sampler:     llamacpp.NewSampler(llamacpp.SamplerParams{}),
				MaxTokens:   12,
				Stats:       &stats,
			})
			if errs[i] == nil && stats.ReusedTokens != int(prefixLen) {
				errs[i] = fmt.Errorf("ReusedTokens = %d, want %d", stats.ReusedTokens, prefixLen)
			}
		}(i, full)
	}
	wg.Wait()

	for i, suffix := range suffixes {
		if errs[i] != nil {
			t.Errorf("suffix %q: %v", suffix, errs[i])
			continue
		}
		if got[i] != want[i] {
			// A restored prefix and a freshly decoded one take different
			// numeric paths, which since llama.cpp's 2026-09 kernels can
			// flip a near-tie ("lantern" hits a 0.55-logit one); a broken
			// restore would disagree with the model confidently instead.
			t.Logf("suffix %q: restored prefix %q, fresh %q - checking it is still greedy", suffix, got[i], want[i])
			requireGreedy(t, model, prefix+suffixes[i], got[i])
		}
	}
}

// TestSchedulerDecideMatchesGreedy checks Scheduler.Decide against plain
// decoding: at each step the option holding the model's own greedy next
// token must win, and the winner must be what the next step is conditioned
// on - so the second step's greedy token is only right if the first
// option's token really was decoded after the prompt.
func TestSchedulerDecideMatchesGreedy(t *testing.T) {
	initBackend()
	modelPath := testModelPath(t)

	model, err := llamacpp.LoadModel(modelPath, llamacpp.ModelParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	const prompt = "The capital of France is"
	argmax := func(logits []float32) llamacpp.Token {
		best := 0
		for i, l := range logits {
			if l > logits[best] {
				best = i
			}
		}
		return llamacpp.Token(best)
	}

	// Reference: the two greedy tokens after the prompt, on a fresh Context.
	ref, err := model.NewContext(llamacpp.ContextParams{NCtx: 512})
	if err != nil {
		t.Fatal(err)
	}
	n, err := ref.DecodePrompt(prompt)
	if err != nil {
		t.Fatal(err)
	}
	first := argmax(ref.LogitsIth(-1))
	batch := llamacpp.NewBatch(1)
	if err := batch.Add(first, n, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := ref.Decode(batch); err != nil {
		t.Fatal(err)
	}
	second := argmax(ref.LogitsIth(-1))
	batch.Close()
	ref.Close()

	other := func(tok llamacpp.Token) llamacpp.Token {
		if tok == 0 {
			return 1
		}
		return tok - 1
	}

	sctx, err := model.NewContext(llamacpp.ContextParams{NCtx: 1024, NSeqMax: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer sctx.Close()
	sched := llamacpp.NewScheduler(sctx, 2)
	defer sched.Close()

	decisions, err := sched.Decide(t.Context(), llamacpp.GenRequest{
		Prompt: prompt,
		Steps: []llamacpp.DecideStep{
			{Options: [][]llamacpp.Token{{other(first)}, {first}}},
			{Options: [][]llamacpp.Token{{second}, {other(second)}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || decisions[0].Choice != 1 || decisions[1].Choice != 0 {
		t.Fatalf("decisions = %+v, want choices 1 then 0", decisions)
	}
	for i, d := range decisions {
		if sum := d.Probs[0] + d.Probs[1]; sum < 0.999 || sum > 1.001 {
			t.Errorf("step %d: probabilities sum to %v", i, sum)
		}
		if d.Mass <= 0 || d.Mass > 1.0001 {
			t.Errorf("step %d: mass %v", i, d.Mass)
		}
	}

	// Malformed steps are refused before anything is decoded.
	if _, err := sched.Decide(t.Context(), llamacpp.GenRequest{Prompt: prompt, Steps: []llamacpp.DecideStep{{Options: [][]llamacpp.Token{{first}, {first}}}}}); err == nil {
		t.Error("options sharing a first token were accepted")
	}
}
