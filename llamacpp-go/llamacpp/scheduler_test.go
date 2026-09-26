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
	"fmt"
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

// TestSchedulerReclaimsSlotAfterDecodeFailure is a regression test for a
// real bug: run's own Decode-failure branch used to call slot.finish
// (delivering the error, marking the Go-side slot free again) without
// also calling Context.TrimSequence the way every other completion path
// already does - so a request that failed mid-generation left its own
// already-decoded tokens permanently resident under that slot's sequence
// id. The next request placed into that same slot then decoded on top of
// stale, never-cleared KV cells, corrupting or failing outright - observed
// in production as "failed to find a memory slot" cascading across
// hundreds of otherwise-unrelated later calls, since every one of those
// failures leaked another slot the same way.
//
// nSlots=1 forces the second request to reuse the exact slot id the first,
// deliberately overflowed one just occupied. NCtx is sized so a short
// prompt plus a generous MaxTokens is guaranteed to exhaust the sequence's
// own context space partway through generation, forcing a real Decode
// failure (not a contrived one) - then the second, ordinary request must
// still come out byte-identical to a fresh single-sequence Context given
// the same prompt, proving the slot was actually reclaimed rather than
// just marked free.
func TestSchedulerReclaimsSlotAfterDecodeFailure(t *testing.T) {
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
	// own token count, so generation is guaranteed to hit the sequence's
	// own context limit and fail (confirmed live: "llama_decode failed:
	// decode: failed to find a memory slot for batch of size 1", the
	// exact production error this test guards against) before MaxTokens
	// ever would.
	if _, err := sched.Generate(t.Context(), llamacpp.GenRequest{
		Prompt:    "Tell me a very long, detailed story about a dragon, at least 500 words.",
		Sampler:   overflowSampler,
		MaxTokens: 1000,
	}); err == nil {
		t.Fatal("expected the deliberately-overflowed request to fail, got no error")
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
		t.Fatalf("follow-up request after a decode failure: %v", err)
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
			t.Errorf("suffix %q: Scheduler.Generate (restored prefix) = %q, want %q (fresh Generate)", suffix, got[i], want[i])
		}
	}
}
