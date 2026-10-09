package llamacpp_test

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rhino1998/lectable/llamacpp-go/llamacpp"
)

// testMTPModelPath returns LLAMACPP_TEST_MTP_MODEL - a GGUF with MTP layers
// (e.g. Qwen3.6-35B-A3B-MTP) - or skips.
func testMTPModelPath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("LLAMACPP_TEST_MTP_MODEL")
	if p == "" {
		t.Skip("LLAMACPP_TEST_MTP_MODEL not set")
	}
	return p
}

// TestSchedulerMTPIsGreedy runs concurrent requests - half seeded from a
// PrefixState snapshot - through a Scheduler with and without MTP drafting.
// MTP output can't be byte-identical to plain decoding (verifying several
// tokens per sequence runs different GPU kernels, and near-tie argmaxes
// flip either way - plain decoding disagrees with a replay of itself too),
// so instead each output is replayed through a fresh plain Context: every
// token must be that Context's own argmax, up to a near-tie margin. A
// broken rollback of rejected drafts corrupts the recurrent state, which
// shows up as confident disagreements, far past that margin.
func TestSchedulerMTPIsGreedy(t *testing.T) {
	initBackend()
	model, err := llamacpp.LoadModel(testMTPModelPath(t), llamacpp.ModelParams{NGPULayers: -1, LoadMTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	vocab := model.Vocab()

	const nSlots = 2
	const nDraft = 3
	system := "You label dialogue in fiction. For every quoted line, output one JSON object per line: " +
		`{"line": <n>, "speaker": <name>, "emotion": <one word>}. Output only the JSON lines.`
	users := []string{
		`1. "Get down!" Mara shouted. 2. "Why?" asked Tom. 3. "Because they're shooting at us," she said.`,
		`1. "I missed you," whispered Elena. 2. Jonas laughed. "You saw me yesterday." 3. "Still," she said.`,
		`1. "Count them," said the captain. 2. "Forty-two, sir," Pell answered. 3. "Count again."`,
		`1. "Is it raining?" asked Nia. 2. "Pouring," said her father. 3. "Then we stay in," Nia decided.`,
	}
	render := func(msgs []llamacpp.ChatMessage, addAssistant bool) string {
		s, err := vocab.ApplyChatTemplate(model.ChatTemplate(""), msgs, addAssistant)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	prefix := render([]llamacpp.ChatMessage{{Role: "system", Content: system}}, false)
	prompts := make([]string, len(users))
	for i, u := range users {
		prompts[i] = render([]llamacpp.ChatMessage{{Role: "system", Content: system}, {Role: "user", Content: u}}, true)
	}

	run := func(mtp bool) ([]string, llamacpp.RoundStats, time.Duration) {
		params := llamacpp.ContextParams{NCtx: 4096 * nSlots, NSeqMax: nSlots, NUBatch: 2048, NOutputsMax: nSlots * (1 + nDraft)}
		if mtp {
			params.NRsSeq = nDraft
		}
		ctx, err := model.NewContext(params)
		if err != nil {
			t.Fatal(err)
		}
		defer ctx.Close()
		if mtp {
			if err := ctx.EnableMTP(params, nDraft); err != nil {
				t.Fatal(err)
			}
		}
		n, err := ctx.DecodePromptSeq(prefix, 0)
		if err != nil {
			t.Fatal(err)
		}
		state := ctx.SaveSeq(0)
		ctx.TrimSequence(0, 0)

		sched := llamacpp.NewScheduler(ctx, nSlots)
		defer sched.Close()
		out := make([]string, len(prompts))
		var wg sync.WaitGroup
		start := time.Now()
		for i, p := range prompts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := llamacpp.GenRequest{Prompt: p, Sampler: llamacpp.NewSampler(llamacpp.SamplerParams{}), MaxTokens: 120}
				if i%2 == 1 {
					req.StartPos, req.PrefixState = n, state
				}
				var err error
				out[i], err = sched.Generate(t.Context(), req)
				if err != nil {
					t.Errorf("prompt %d (mtp=%v): %v", i, mtp, err)
				}
			}()
		}
		wg.Wait()
		return out, sched.RoundStats(), time.Since(start)
	}

	_, ps, pt := run(false)
	outs, ms, mt := run(true)
	t.Logf("plain: %v, %d decode rounds; MTP: %v, %d decode rounds, drafted %d, accepted %d (%.0f%%)",
		pt, ps.DecodeRounds, mt, ms.DecodeRounds, ms.DraftTokens, ms.DraftAccepted,
		100*float64(ms.DraftAccepted)/float64(max(ms.DraftTokens, 1)))
	if ms.DraftTokens == 0 || ms.DraftAccepted == 0 {
		t.Errorf("MTP drafted %d tokens, accepted %d", ms.DraftTokens, ms.DraftAccepted)
	}

	for i, p := range prompts {
		requireGreedy(t, model, p, outs[i])
	}
}

// nearTie is the logit margin under which requireGreedy lets a token
// differ from the argmax: GPU numeric noise between batch shapes (measured
// up to ~0.8 on Qwen3.6-35B-A3B and Qwen3-4B) flips choices that close.
const nearTie = 1.5

// requireGreedy replays prompt+out through a fresh plain Context in one
// decode and fails for any token of out that isn't that Context's argmax by
// more than nearTie - what greedy output from corrupted KV/recurrent state
// looks like, as opposed to numeric noise.
func requireGreedy(t *testing.T, model *llamacpp.Model, prompt, out string) {
	t.Helper()
	vocab := model.Vocab()
	pt, err := vocab.Tokenize(prompt, true, true)
	if err != nil {
		t.Fatal(err)
	}
	all, err := vocab.Tokenize(prompt+out, true, true)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := model.NewContext(llamacpp.ContextParams{NCtx: uint32(len(all) + 16)})
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()
	b := llamacpp.NewBatch(len(all))
	defer b.Close()
	for k, tok := range all {
		if err := b.Add(tok, int32(k), 0, k >= len(pt)-1); err != nil {
			t.Fatal(err)
		}
	}
	if err := ref.Decode(b); err != nil {
		t.Fatal(err)
	}
	for k := len(pt) - 1; k < len(all)-1; k++ {
		lg := ref.LogitsIth(int32(k))
		best := 0
		for j := range lg {
			if lg[j] > lg[best] {
				best = j
			}
		}
		if margin := lg[best] - lg[all[k+1]]; margin > nearTie {
			t.Errorf("output token %d: got %q where the model's argmax is %q (margin %.2f)",
				k+1-len(pt), vocab.TokenToPiece(all[k+1], false), vocab.TokenToPiece(llamacpp.Token(best), false), margin)
		}
	}
}

// TestSchedulerMTPAfterDecide checks that a Decide request, which never
// drafts, leaves its slot fit for drafting again: generations that follow
// it in the same slot - from the prefix it left there, and from scratch -
// must still be greedy and still have drafts accepted.
func TestSchedulerMTPAfterDecide(t *testing.T) {
	initBackend()
	model, err := llamacpp.LoadModel(testMTPModelPath(t), llamacpp.ModelParams{NGPULayers: -1, LoadMTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	vocab := model.Vocab()

	const nDraft = 3
	system := "You label dialogue in fiction. For every quoted line, output one JSON object per line: " +
		`{"line": <n>, "speaker": <name>, "emotion": <one word>}. Output only the JSON lines.`
	render := func(user string) string {
		msgs := []llamacpp.ChatMessage{{Role: "system", Content: system}}
		if user != "" {
			msgs = append(msgs, llamacpp.ChatMessage{Role: "user", Content: user})
		}
		s, err := vocab.ApplyChatTemplate(model.ChatTemplate(""), msgs, user != "")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	tokens := func(s string) []llamacpp.Token {
		toks, err := vocab.Tokenize(s, false, false)
		if err != nil {
			t.Fatal(err)
		}
		return toks
	}
	prompt := render(`1. "Get down!" Mara shouted. 2. "Why?" asked Tom. 3. "Because they're shooting at us," she said.`)

	params := llamacpp.ContextParams{NCtx: 4096, NSeqMax: 1, NUBatch: 2048, NOutputsMax: 1 + nDraft, NRsSeq: nDraft}
	ctx, err := model.NewContext(params)
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	if err := ctx.EnableMTP(params, nDraft); err != nil {
		t.Fatal(err)
	}
	n, err := ctx.DecodePromptSeq(render(""), 0)
	if err != nil {
		t.Fatal(err)
	}
	state := ctx.SaveSeq(0)
	ctx.TrimSequence(0, 0)

	sched := llamacpp.NewScheduler(ctx, 1)
	defer sched.Close()
	decide := func() {
		options := [][]llamacpp.Token{tokens(" yes"), tokens(" no")}
		decisions, err := sched.Decide(t.Context(), llamacpp.GenRequest{
			Prompt: prompt, StartPos: n, PrefixState: state,
			Steps: []llamacpp.DecideStep{{Prefix: tokens("1:"), Options: options}, {Prefix: tokens("\n2:"), Options: options}, {Prefix: tokens("\n3:"), Options: options}},
		})
		if err != nil || len(decisions) != 3 {
			t.Fatalf("Decide: %d decisions, %v", len(decisions), err)
		}
	}
	generate := func(fromPrefix bool) {
		req := llamacpp.GenRequest{Prompt: prompt, Sampler: llamacpp.NewSampler(llamacpp.SamplerParams{}), MaxTokens: 100}
		if fromPrefix {
			req.StartPos, req.PrefixState = n, state
		}
		out, err := sched.Generate(t.Context(), req)
		if err != nil {
			t.Fatalf("Generate (fromPrefix=%v): %v", fromPrefix, err)
		}
		requireGreedy(t, model, prompt, out)
	}

	decide()
	generate(true)
	decide()
	generate(false)
	if rs := sched.RoundStats(); rs.DraftTokens == 0 || 2*rs.DraftAccepted < rs.DraftTokens {
		t.Errorf("after Decide requests, MTP drafted %d tokens and had %d accepted", rs.DraftTokens, rs.DraftAccepted)
	}
}
