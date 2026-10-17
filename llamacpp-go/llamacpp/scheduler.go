package llamacpp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"
)

// GenRequest describes one independent prompt-in/text-out generation to run
// through a Scheduler -- the multi-sequence-batching counterpart to
// Context.GenerateFrom's own arguments.
type GenRequest struct {
	// Prompt is the full text to generate from. Tokenized fresh on
	// admission (see GenerateFrom's own doc comment on why the full text,
	// not just its own suffix, is always tokenized -- the same reasoning
	// applies here).
	Prompt string

	// StartPos and PrimedSeq let this request skip redecoding a prefix its
	// own Prompt starts with, the way GenerateFrom's startPos does for a
	// single reused sequence -- except here the prefix lives in a
	// different, permanently-resident sequence (PrimedSeq) that this
	// request's own generation slot copies from (Context.CopySeq) rather
	// than reusing in place, since a Scheduler multiplexes many concurrent,
	// unrelated requests across a small pool of slots and no single slot
	// can be reserved for one caller's exclusive reuse the way a
	// single-sequence caller's own Context can. Leave StartPos at 0 to
	// decode Prompt from scratch (PrimedSeq is then unused).
	//
	// Callers are responsible for verifying Prompt's own first StartPos
	// tokens actually match whatever was decoded into PrimedSeq -- same
	// requirement, same reasoning, as GenerateFrom's own startPos.
	StartPos  int32
	PrimedSeq int32

	// PrefixState, if set (with StartPos > 0), seeds the slot by restoring
	// this Context.SaveSeq snapshot of the prefix instead of CopySeq-ing
	// PrimedSeq - the path for a Context without KVUnified, where CopySeq
	// can't copy a partial range but a primed prefix also doesn't sit in
	// the shared cell range every other sequence's attention spans. If the
	// restore fails, the whole prompt is decoded instead.
	PrefixState []byte

	// Sampler is closed by the Scheduler itself, exactly once, whenever it
	// is actually done sampling from it -- never by the caller. This
	// matters specifically because Generate can return well before that
	// point: a caller whose ctx is cancelled while this request is still
	// queued, or already admitted into an active, still-generating slot,
	// gets ctx.Err() back immediately (see Generate's own doc comment),
	// but the Scheduler's own goroutine may still be several rounds away
	// from actually finishing with this Sampler. A caller that closed it
	// on its own right after Generate returned used to race exactly that
	// still-in-flight Sample call -- a real, previously-crashing
	// use-after-free (llama_sampler_sample segfaulting on a freed/nil
	// handle), not just a theoretical one. Ownership transfers to the
	// Scheduler the moment Generate successfully hands this request off
	// (into s.submit); before that (Generate returning via ctx.Done()/
	// s.closed in its first select, having never actually submitted), the
	// Scheduler never sees this request at all, so Generate itself closes
	// it in that case.
	Sampler   *Sampler
	MaxTokens int

	// OnPiece is called with each generated token's text as it's produced,
	// from the Scheduler's own goroutine -- keep it cheap and non-blocking,
	// the same constraint GenerateFrom's onPiece already has. May be nil.
	// Returning false stops this request's own generation early (other
	// requests sharing the Scheduler are unaffected).
	OnPiece func(piece string) bool

	// Stats, if non-nil, is filled in with this request's timing breakdown
	// when Generate returns its result (not when it returns early on ctx
	// cancellation - the Scheduler may still be running the request then,
	// so it never writes here directly).
	Stats *GenStats

	// Steps makes this a Decide request (see Scheduler.Decide): nothing is
	// sampled, Sampler and MaxTokens are unused.
	Steps []DecideStep
}

// DecideStep is one closed-set decision in a Decide request: Prefix is
// decoded after whatever came before it, and the model's next-token logits
// there are read over each option's first token. Options may share leading
// tokens ("Lord Gresper", "Lord Whiteship"): the shared ones are decoded
// and the logits read again where they part, as many times as it takes.
type DecideStep struct {
	Prefix  []Token
	Options [][]Token // each non-empty, none equal to or a prefix of another
}

// Decision is the outcome of one DecideStep.
type Decision struct {
	// Choice indexes the step's Options: the one whose tokens scored
	// highest at each point where the options still in the running part.
	Choice int
	// Probs is each option's probability, in Options order, normalized over
	// the options alone. Where options share leading tokens it is the chain
	// of those partings: the winner's is the product of its token's share
	// at each one, and an option dropped at one gets its branch's share
	// there (split evenly among the options dropped with it), so they still
	// sum to one.
	Probs []float64
	// Mass is how much of the model's whole next-token distribution the
	// options' first tokens hold - low when the model wanted to say
	// something that isn't an option.
	Mass float64
}

// GenStats is one Scheduler request's timing breakdown - where its wall
// time went, split the way prompt-side and generation-side optimizations
// need it split. Phase durations are wall clock: rounds are shared across
// every active slot, so a phase also absorbs other slots' work packed into
// the same decode calls.
type GenStats struct {
	// ReusedTokens came from GenRequest.PrimedSeq or PrefixState instead of
	// being decoded; PromptTokens were actually prefilled by this request.
	ReusedTokens int
	PromptTokens int
	// GenTokens is the number of tokens sampled (including a final EOG).
	GenTokens int

	Queued  time.Duration // submitted until admitted into a slot
	Prefill time.Duration // admitted until the prompt's last token was decoded
	Decode  time.Duration // prompt decoded until the request finished
}

// Scheduler batches concurrent generation requests against one Context
// created with ContextParams.NSeqMax > 1, submitting one shared
// Context.Decode call per generation round across every currently active
// request instead of serializing them into separate calls each. This is
// what multi-sequence batching buys over N independent Contexts: llama_decode
// itself is not reentrant (only one call may be in flight against a given
// Context at a time -- see Context.Decode), but N single-token decode steps
// for N independent sequences can be packed into that one call, so the
// GPU/CPU work backing a decode round is shared across every active request
// instead of paid once per request.
//
// A Scheduler owns nSlots dedicated sequence ids, [0, nSlots), used as a
// rotating pool: an admitted request occupies one slot for its own
// generation and gives it back (after a full wipe -- TrimSequence(slot, 0),
// which for a whole-sequence removal never fails) once it finishes. Any
// other sequence id on the same Context (e.g. one primed via
// DecodePromptSeq for GenRequest.PrimedSeq) is the caller's own to manage --
// the Scheduler never touches a sequence outside its own slot pool.
type Scheduler struct {
	ctx    *Context
	nSlots int32

	submit chan *schedRequest
	done   chan struct{}
	closed chan struct{}

	closeOnce sync.Once

	roundsMu sync.Mutex
	rounds   RoundStats

	// kept[i] is the PrefixState prefix slot i's sequence still holds from
	// its last request (see release), touched only by run's goroutine.
	kept []keptPrefix
}

// keptPrefix identifies a restored prefix left in a slot after its request
// finished: the snapshot it came from (by its first byte's address - the
// same snapshot slice is passed for every request sharing that prefix)
// and how many tokens of it the slot holds.
type keptPrefix struct {
	state *byte
	n     int32
}

// RoundStats sums a Scheduler's decode rounds by kind: rounds that carried
// any prompt (prefill) tokens versus rounds carrying only generation
// tokens. Unlike GenStats (per request, where one slot's decode phase also
// absorbs rounds spent on another slot's prefill), these partition the
// Scheduler's actual busy time, so they show where wall time really goes.
type RoundStats struct {
	PrefillRounds, DecodeRounds int
	PrefillTokens, DecodeTokens int // tokens submitted in each kind of round
	PrefillTime, DecodeTime     time.Duration

	// PrefixReused counts PrefixState requests that found their prefix
	// still in the slot; PrefixRestored those that restored the snapshot.
	PrefixReused, PrefixRestored int
	RestoreTime                  time.Duration

	// With MTP (Context.EnableMTP): DraftTokens were drafted and verified,
	// DraftAccepted of them matched what the target sampled, and DraftTime
	// is the time spent drafting.
	DraftTokens, DraftAccepted int
	DraftTime                  time.Duration
}

// RoundStats returns the running totals since the Scheduler started.
func (s *Scheduler) RoundStats() RoundStats {
	s.roundsMu.Lock()
	defer s.roundsMu.Unlock()
	return s.rounds
}

type schedRequest struct {
	ctx       context.Context
	req       GenRequest
	result    chan schedResult
	submitted time.Time
}

type schedResult struct {
	text      string
	decisions []Decision
	err       error
	stats     GenStats
}

// NewScheduler starts a Scheduler backed by ctx, using nSlots of its
// sequence ids (must be <= int(ctx.NSeqMax())) as its own rotating
// generation-slot pool. ctx must not be used for anything else concurrently
// with the Scheduler (its own goroutine is the only thing that may call
// Decode/Sample/CopySeq/TrimSequence against it) -- a separate, unrelated
// sequence id on the same Context (e.g. a primed prefix) is fine to manage
// from other goroutines, since priming happens before/between rounds via
// the Scheduler's own request path (see GenRequest.PrimedSeq), never
// concurrently with a round in progress.
func NewScheduler(ctx *Context, nSlots int) *Scheduler {
	if nSlots < 1 {
		nSlots = 1
	}
	s := &Scheduler{
		ctx:    ctx,
		nSlots: int32(nSlots),
		submit: make(chan *schedRequest),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
		kept:   make([]keptPrefix, nSlots),
	}
	go s.run()
	return s
}

// errSchedulerClosed is returned by Generate once Close has been called.
var errSchedulerClosed = errors.New("llamacpp: scheduler closed")

// Close stops the Scheduler's round loop and waits for it to exit. Any
// request already admitted into a slot is left to finish naturally (see
// run's own doc comment); a request still queued, or submitted after Close
// starts, gets errSchedulerClosed instead of running. Does not close the
// underlying Context -- that's still the caller's own to close once done.
func (s *Scheduler) Close() {
	s.closeOnce.Do(func() { close(s.closed) })
	<-s.done
}

// Generate submits one independent prompt-in/text-out request and blocks
// until it completes, sharing this Scheduler's Context/decode calls with
// however many other Generate calls are concurrently in flight (up to
// nSlots at once; beyond that, a request simply waits its turn in the
// Scheduler's own admission queue). ctx is checked before this request is
// admitted into a slot (so one queued behind a cancelled caller never
// starts), but -- like Context.GenerateFrom -- can't interrupt a generation
// already underway, since llama.cpp's own decode loop exposes no
// cancellation hook mid-call: a ctx cancellation once this request is
// already admitted into an active slot still returns ctx.Err() here right
// away, but the Scheduler's own goroutine keeps that slot running to
// completion regardless, unaware this caller stopped waiting on it (see
// GenRequest.Sampler's own doc comment for why that's specifically why its
// Sampler must never be closed by this function's caller after the fact).
func (s *Scheduler) Generate(ctx context.Context, req GenRequest) (string, error) {
	sr := &schedRequest{ctx: ctx, req: req, result: make(chan schedResult, 1), submitted: time.Now()}
	select {
	case s.submit <- sr:
	case <-ctx.Done():
		// Never actually handed off to the Scheduler (still sitting in
		// this local sr, about to be dropped) -- nothing else will ever
		// close its Sampler, so this is the one case Generate itself must.
		req.Sampler.Close()
		return "", ctx.Err()
	case <-s.closed:
		req.Sampler.Close()
		return "", errSchedulerClosed
	}
	select {
	case res := <-sr.result:
		if req.Stats != nil {
			*req.Stats = res.stats
		}
		return res.text, res.err
	case <-ctx.Done():
		// Already handed off above -- the Scheduler goroutine owns sr now
		// (queued or already admitted into a slot) and will close its
		// Sampler itself once genuinely done with it, whether or not this
		// caller is still around to receive the result.
		return "", ctx.Err()
	}
}

// Decide runs req.Steps in order through one slot, sharing rounds with
// every other request like Generate: after the prompt and a step's Prefix
// are decoded, the next-token logits are read over that step's options
// instead of sampled, the winning option's tokens are decoded as if the
// model had written them, and the next step follows. So a reply of several
// answers is built the way greedy generation would build it, one decode
// round per answer, and each answer comes with its probability. Prefix
// reuse (StartPos/PrimedSeq/PrefixState) and ctx work as in Generate.
func (s *Scheduler) Decide(ctx context.Context, req GenRequest) ([]Decision, error) {
	if len(req.Steps) == 0 {
		return nil, errors.New("llamacpp: decide: no steps")
	}
	for i, st := range req.Steps {
		if len(st.Options) < 2 {
			return nil, fmt.Errorf("llamacpp: decide: step %d has fewer than two options", i)
		}
		for a, o := range st.Options {
			if len(o) == 0 {
				return nil, fmt.Errorf("llamacpp: decide: step %d has an empty option", i)
			}
			for b, other := range st.Options {
				if a != b && len(o) <= len(other) && slices.Equal(o, other[:len(o)]) {
					return nil, fmt.Errorf("llamacpp: decide: step %d's option %d is the same as or a prefix of option %d", i, a, b)
				}
			}
		}
	}
	req.Sampler = nil
	sr := &schedRequest{ctx: ctx, req: req, result: make(chan schedResult, 1), submitted: time.Now()}
	select {
	case s.submit <- sr:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closed:
		return nil, errSchedulerClosed
	}
	select {
	case res := <-sr.result:
		if req.Stats != nil {
			*req.Stats = res.stats
		}
		return res.decisions, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// decideState is a Decide slot's progress through its current step: which
// options are still in the running, how many of their (shared) tokens are
// decoded, and the probabilities settled so far.
type decideState struct {
	started bool
	alive   []int
	depth   int
	pathP   float64
	probs   []float64
	mass    float64
}

// read takes one decoded row's logits at the point the options still in
// the running next differ (their token at st.depth) and narrows them to
// the best-scoring token's options. With one left it returns the step's
// Decision and the rest of the winner's tokens; otherwise it returns the
// tokens to decode before reading again - the chosen one and any more the
// survivors all share - and done false.
func (st *decideState) read(logits []float32, step DecideStep) (d Decision, next []Token, done bool) {
	if !st.started {
		*st = decideState{started: true, pathP: 1, probs: make([]float64, len(step.Options))}
		for i := range step.Options {
			st.alive = append(st.alive, i)
		}
	}
	maxAll := float64(logits[0])
	for _, l := range logits {
		maxAll = math.Max(maxAll, float64(l))
	}
	groups := map[Token][]int{}
	var order []Token
	for _, o := range st.alive {
		tok := step.Options[o][st.depth]
		if groups[tok] == nil {
			order = append(order, tok)
		}
		groups[tok] = append(groups[tok], o)
	}
	var sumOpts float64
	weight := make(map[Token]float64, len(order))
	best := order[0]
	for _, tok := range order {
		weight[tok] = math.Exp(float64(logits[tok]) - maxAll)
		sumOpts += weight[tok]
		if weight[tok] > weight[best] {
			best = tok
		}
	}
	if st.depth == 0 {
		var sumAll float64
		for _, l := range logits {
			sumAll += math.Exp(float64(l) - maxAll)
		}
		st.mass = sumOpts / sumAll
	}
	for _, tok := range order {
		if tok == best {
			continue
		}
		share := st.pathP * weight[tok] / sumOpts / float64(len(groups[tok]))
		for _, o := range groups[tok] {
			st.probs[o] = share
		}
	}
	st.pathP *= weight[best] / sumOpts
	st.alive = groups[best]
	if len(st.alive) == 1 {
		w := st.alive[0]
		st.probs[w] = st.pathP
		d = Decision{Choice: w, Probs: st.probs, Mass: st.mass}
		next = step.Options[w][st.depth:]
		*st = decideState{}
		return d, next, true
	}
	// The survivors share best; decode it, plus every further token they
	// all still share, and read again where they part. None is a prefix of
	// another (Decide checks), so they part before any of them ends.
	next = []Token{best}
	st.depth++
	for {
		tok := step.Options[st.alive[0]][st.depth]
		shared := true
		for _, o := range st.alive[1:] {
			shared = shared && step.Options[o][st.depth] == tok
		}
		if !shared {
			break
		}
		next = append(next, tok)
		st.depth++
	}
	return Decision{}, next, false
}

// slot is one of the Scheduler's nSlots rotating sequence ids and whatever
// request currently occupies it, if any.
type slot struct {
	active bool
	req    *schedRequest

	pos       int32   // next position to write in this slot's own sequence
	remaining []Token // prompt tokens not yet decoded; nil once prefill is done
	generated []byte  // accumulated output text
	nextTok   Token   // sampled token waiting to be decoded next round
	haveNext  bool    // whether nextTok is actually set yet (false on a slot's very first round)
	left      int     // generation tokens still allowed before MaxTokens is hit
	ctxFull   bool    // left was cut to what the sequence has room for (admitInto)

	decisions []Decision  // a Decide request's answers so far
	decide    decideState // and its progress through the current step

	// MTP only: draft is this round's drafted continuation of nextTok,
	// decoded right after it; posBase is nextTok's own position.
	draft   []Token
	posBase int32

	stats       GenStats
	admitted    time.Time
	prefillDone time.Time
}

func (sl *slot) finish(res schedResult) {
	now := time.Now()
	st := sl.stats
	st.Queued = sl.admitted.Sub(sl.req.submitted)
	if sl.prefillDone.IsZero() {
		st.Prefill = now.Sub(sl.admitted)
	} else {
		st.Prefill = sl.prefillDone.Sub(sl.admitted)
		st.Decode = now.Sub(sl.prefillDone)
	}
	res.stats = st
	finishRequest(sl.req, res)
	*sl = slot{}
}

// finishRequest delivers res to sr's own caller and closes sr's Sampler --
// every path that ends sr's life inside the Scheduler (a queued request
// whose ctx was already cancelled by the time admit reached it, an
// admission failure, the Scheduler itself closing with sr still queued, or
// an admitted slot actually finishing via slot.finish) must go through
// this rather than sending to sr.result directly, so GenRequest.Sampler is
// always closed exactly once, by whichever of those is the one that
// actually happens for this request -- never by Generate's own caller,
// who may have already stopped waiting on sr long before any of them run
// (see GenRequest.Sampler's own doc comment).
func finishRequest(sr *schedRequest, res schedResult) {
	sr.req.Sampler.Close()
	sr.result <- res
}

// run is the Scheduler's own single goroutine -- the only thing that ever
// calls into s.ctx, since llama_decode is not reentrant. It admits queued
// requests into free slots, builds one combined Batch per round mixing
// still-prefilling slots' prompt chunks with still-generating slots' single
// next tokens, and repeats until told to stop.
func (s *Scheduler) run() {
	defer close(s.done)

	slots := make([]slot, s.nSlots)
	var queue []*schedRequest
	nBatch := int(s.ctx.NBatch())
	if nBatch <= 0 {
		nBatch = 2048
	}
	batch := NewBatch(nBatch)
	defer batch.Close()

	vocab := s.ctx.model.Vocab()

	admit := func() {
		for i := range slots {
			if slots[i].active {
				continue
			}
			for len(queue) > 0 {
				sr := queue[0]
				queue = queue[1:]
				if err := sr.ctx.Err(); err != nil {
					finishRequest(sr, schedResult{err: err})
					continue
				}
				if err := s.admitInto(int32(i), &slots[i], sr, vocab); err != nil {
					finishRequest(sr, schedResult{err: err})
					continue
				}
				break
			}
			if !slots[i].active {
				break // queue drained; no point scanning the rest of slots
			}
		}
	}

	for {
		admit()

		anyActive := false
		for i := range slots {
			if slots[i].active {
				anyActive = true
				break
			}
		}
		if !anyActive {
			select {
			case sr := <-s.submit:
				queue = append(queue, sr)
				continue
			case <-s.closed:
				for _, sr := range queue {
					finishRequest(sr, schedResult{err: errSchedulerClosed})
				}
				return
			}
		}

		// Drain any newly-submitted requests without blocking, so a burst
		// of concurrent Generate calls all get queued promptly rather than
		// trickling in one round at a time.
		draining := true
		for draining {
			select {
			case sr := <-s.submit:
				queue = append(queue, sr)
			case <-s.closed:
				for _, sr := range queue {
					finishRequest(sr, schedResult{err: errSchedulerClosed})
				}
				return
			default:
				draining = false
			}
		}
		admit()

		outputIdx := make([]int32, len(slots))
		hasOutput := make([]bool, len(slots))
		// batchPos is the raw 0-based position of the *next* token added to
		// this round's Batch, counting every token (not just ones that want
		// logits) -- llama_get_logits_ith's own index argument is exactly
		// this raw batch position (it looks up output_ids[i] internally to
		// find that token's actual output row, or fails if that position
		// never requested logits at all), not a counter over logit-
		// requesting tokens alone. Getting this wrong (as an earlier
		// version of this loop did, counting only wantLogits adds) makes
		// Sample look up the wrong row for any slot preceded in the batch
		// by a multi-token prefill chunk, crashing on
		// GGML_ASSERT(logits != nullptr) the moment a real prefill chunk
		// (more than one token) sits in front of a later slot in the same
		// round.
		batchPos := int32(0)

		batch.Reset()
		budget := nBatch
		prefillN := 0 // prompt tokens in this round, for recordRound

		// verifying marks decode-phase slots whose tokens this round are
		// nextTok plus its MTP draft, checked against the target below.
		verifying := make([]bool, len(slots))
		if s.ctx.mtp != nil {
			s.draftSlots(slots)
		}

		// Continuing (decode-phase) slots first -- one token each, cheap,
		// and keeping them fed is what keeps their own latency low instead
		// of being starved behind a newly-admitted slot's large prefill.
		for i := range slots {
			sl := &slots[i]
			if !sl.active || sl.remaining != nil {
				continue
			}
			if !sl.haveNext || budget <= 0 {
				continue
			}
			// nextTok, then any MTP draft after it, every one wanting
			// logits: row j is what the target samples after token j.
			added := 0
			for j := 0; j <= len(sl.draft) && budget > 0; j++ {
				tok := sl.nextTok
				if j > 0 {
					tok = sl.draft[j-1]
				}
				if err := batch.Add(tok, sl.pos, int32(i), true); err != nil {
					break // batch genuinely full; picked up again next round
				}
				batchPos++
				sl.pos++
				budget--
				added++
			}
			if added == 0 {
				continue
			}
			sl.draft = sl.draft[:added-1]
			sl.posBase = sl.pos - int32(added)
			outputIdx[i] = batchPos - int32(added)
			hasOutput[i] = true
			verifying[i] = s.ctx.mtp != nil
		}

		// Prefilling slots next, each contributing as much of its own
		// remaining prompt as still fits this round's budget. Only the
		// chunk that reaches the true end of the prompt requests logits --
		// a partial chunk just advances pos/remaining for the next round,
		// the same chunking Context.Decode itself already does for one
		// long single-sequence prompt, generalized across slots here.
		for i := range slots {
			sl := &slots[i]
			if !sl.active || sl.remaining == nil || budget <= 0 {
				continue
			}
			n := len(sl.remaining)
			if n > budget {
				n = budget
			}
			last := n == len(sl.remaining)
			lastTokenPos := int32(-1)
			for j := 0; j < n; j++ {
				want := last && j == n-1
				if err := batch.Add(sl.remaining[j], sl.pos, int32(i), want); err != nil {
					n = j // batch filled up mid-chunk; stop here
					last = false
					break
				}
				if want {
					lastTokenPos = batchPos
				}
				batchPos++
				sl.pos++
			}
			if n == 0 {
				continue
			}
			budget -= n
			prefillN += n
			if last {
				outputIdx[i] = lastTokenPos
				hasOutput[i] = true
				sl.remaining = nil
			} else {
				sl.remaining = sl.remaining[n:]
			}
		}

		if batch.NTokens() == 0 {
			// Every active slot is already fully caught up for this round
			// (shouldn't normally happen given the loops above always make
			// progress on any active slot) -- avoid a busy spin.
			continue
		}

		// Synchronize so the round's time is its real GPU time: llama_decode
		// only queues work, which otherwise lands on whichever later call
		// first reads logits (and a partial prefill chunk reads none).
		roundStart := time.Now()
		err := s.ctx.Decode(batch)
		if err == nil {
			s.ctx.Synchronize()
		}
		s.recordRound(batch.NTokens(), prefillN, time.Since(roundStart))
		if err != nil {
			for i := range slots {
				if slots[i].active {
					slots[i].finish(schedResult{err: fmt.Errorf("llamacpp: decode: %w", err)})
					// Every other path that force-finishes a slot (EOG,
					// MaxTokens exhausted, OnPiece early-stop, below) wipes
					// its own sequence's KV cache right after - this path
					// didn't, a real bug: TrimSequence is a KV-cache
					// metadata operation (llama_memory_seq_rm), independent
					// of whether the Decode call that just failed actually
					// wrote anything, and "never fails" for a full wipe
					// (p0=0) - so it's always safe to call here regardless
					// of what Decode's own error was. Without it, this
					// slot's already-decoded tokens from before the failed
					// round stayed resident under this sequence id forever;
					// admitInto's own TrimSequence(slotID, 0) still runs
					// before the *next* request placed here, but only
					// clears tokens actually tagged with this seq id at that
					// point - it can't undo a stale high-water-mark the KV
					// allocator itself was still tracking, so each
					// leaked slot permanently ate into the shared KV pool.
					// Confirmed in production: one real decode failure (any
					// cause) cascaded into "failed to find a memory slot"
					// on every subsequent round, for batches as small as 3-4
					// tokens, hundreds of times in a row, because each of
					// those failures leaked another slot's worth of space
					// the same way.
					s.ctx.TrimSequence(int32(i), 0)
					s.kept[i] = keptPrefix{}
				}
			}
			continue
		}

		decoded := time.Now()
		for i := range slots {
			sl := &slots[i]
			if !sl.active || !hasOutput[i] {
				continue
			}
			if sl.prefillDone.IsZero() {
				sl.prefillDone = decoded
			}
			if steps := sl.req.req.Steps; steps != nil {
				// A Decide request: read this step's answer off the row,
				// then prefill the answer itself and the next step's prefix.
				step := steps[len(sl.decisions)]
				logits := s.ctx.LogitsIth(outputIdx[i])
				if logits == nil {
					s.ctx.TrimSequence(int32(i), 0)
					s.kept[i] = keptPrefix{}
					sl.finish(schedResult{err: errors.New("llamacpp: decide: no logits for the step's last token")})
					continue
				}
				d, next, done := sl.decide.read(logits, step)
				if !done {
					// Options still share this token: decode it and read
					// again where they part.
					sl.remaining = next
					sl.stats.PromptTokens += len(sl.remaining)
					continue
				}
				sl.decisions = append(sl.decisions, d)
				if len(sl.decisions) == len(steps) {
					s.release(int32(i), sl)
					sl.finish(schedResult{decisions: sl.decisions})
					continue
				}
				sl.remaining = append(append([]Token{}, next...), steps[len(sl.decisions)].Prefix...)
				sl.stats.PromptTokens += len(sl.remaining)
				continue
			}
			if sl.left <= 0 {
				// MaxTokens <= 0: the prompt is decoded (same as
				// GenerateFrom always does, unconditionally) but nothing is
				// ever sampled from it, matching GenerateFrom's own
				// `for n := 0; n < maxTokens; n++` never running its body.
				s.release(int32(i), sl)
				sl.finish(schedResult{text: string(sl.generated)})
				continue
			}
			// Sample row j; while it matches draft token j, that token is
			// already decoded after it, so keep going. The first mismatch
			// (or the row after the whole draft) is the new nextTok.
			var next Token
			accepted := 0
			done := false
			for j := 0; ; j++ {
				next = sl.req.req.Sampler.Sample(s.ctx, outputIdx[i]+int32(j))
				sl.stats.GenTokens++
				if vocab.IsEOG(next) {
					done = true
					break
				}
				piece := vocab.TokenToPiece(next, false)
				sl.generated = append(sl.generated, piece...)
				if onPiece := sl.req.req.OnPiece; onPiece != nil && !onPiece(piece) {
					done = true
					break
				}
				sl.left--
				if sl.left <= 0 {
					done = true
					break
				}
				if j < len(sl.draft) && next == sl.draft[j] {
					accepted++
					continue
				}
				break
			}
			if verifying[i] && len(sl.draft) > 0 {
				s.recordDraft(len(sl.draft), accepted)
			}
			if done && sl.left <= 0 && sl.ctxFull {
				s.release(int32(i), sl)
				sl.finish(schedResult{err: fmt.Errorf("%w: a prompt of %d tokens left room for %d of the %d asked for", ErrContextFull, sl.stats.ReusedTokens+sl.stats.PromptTokens, sl.stats.GenTokens, sl.req.req.MaxTokens)})
				continue
			}
			if done {
				s.release(int32(i), sl)
				sl.finish(schedResult{text: string(sl.generated)})
				continue
			}
			if verifying[i] {
				// Drop the rejected draft tokens' KV (target and draft
				// contexts), then point the driver at the last kept row.
				keep := sl.posBase + 1 + int32(accepted)
				if accepted < len(sl.draft) && !s.ctx.TrimSequence(int32(i), keep) {
					s.ctx.TrimSequence(int32(i), 0)
					s.kept[i] = keptPrefix{}
					sl.finish(schedResult{err: errors.New("llamacpp: MTP: could not roll back rejected draft tokens")})
					continue
				}
				s.ctx.mtp.accept(int32(i), accepted)
				sl.pos = keep
			}
			sl.draft = nil
			sl.nextTok = next
			sl.haveNext = true
		}
	}
}

// release clears slot slotID's sequence once its request is done. A
// request seeded from GenRequest.PrefixState keeps that prefix instead of
// wiping it, so the next request with the same snapshot can reuse it in
// place (admitInto). Only that path keeps anything: it runs on a
// non-unified Context, where one slot's leftover cells don't widen any
// other sequence's attention range.
func (s *Scheduler) release(slotID int32, sl *slot) {
	st := sl.req.req.PrefixState
	n := int32(sl.stats.ReusedTokens)
	if st != nil && n > 0 && s.ctx.TrimSequence(slotID, n) {
		s.kept[slotID] = keptPrefix{state: &st[0], n: n}
		return
	}
	s.ctx.TrimSequence(slotID, 0)
	s.kept[slotID] = keptPrefix{}
}

// draftSlots sets each generating slot's MTP draft for this round - all
// drafted together - leaving room under MaxTokens for every drafted token
// plus the one sampled after them.
func (s *Scheduler) draftSlots(slots []slot) {
	var reqs []mtpDraftReq
	var idx []int
	for i := range slots {
		sl := &slots[i]
		sl.draft = nil
		if !sl.active || sl.remaining != nil || !sl.haveNext || sl.left <= 1 {
			continue
		}
		reqs = append(reqs, mtpDraftReq{seq: int32(i), idLast: sl.nextTok, pos0: sl.pos, nMax: min(s.ctx.mtp.nMax, sl.left-1)})
		idx = append(idx, i)
	}
	if len(reqs) == 0 {
		return
	}
	t := time.Now()
	for k, d := range s.ctx.mtp.draft(reqs) {
		slots[idx[k]].draft = d
	}
	s.roundsMu.Lock()
	s.rounds.DraftTime += time.Since(t)
	s.roundsMu.Unlock()
}

func (s *Scheduler) recordDraft(drafted, accepted int) {
	s.roundsMu.Lock()
	defer s.roundsMu.Unlock()
	s.rounds.DraftTokens += drafted
	s.rounds.DraftAccepted += accepted
}

// recordRound adds one Decode call to s.rounds - a prefill round if it
// carried any prompt tokens at all.
func (s *Scheduler) recordRound(nTokens, prefillN int, d time.Duration) {
	s.roundsMu.Lock()
	defer s.roundsMu.Unlock()
	if prefillN > 0 {
		s.rounds.PrefillRounds++
		s.rounds.PrefillTokens += nTokens
		s.rounds.PrefillTime += d
		return
	}
	s.rounds.DecodeRounds++
	s.rounds.DecodeTokens += nTokens
	s.rounds.DecodeTime += d
}

// admitInto starts sr running in slot slotID: wipes that slot's own
// sequence (a whole-sequence removal, which never fails, so this never
// leaves stale tokens from whatever request last occupied it) - or, when
// the slot still holds sr's own PrefixState prefix, trims back to it -
// seeds it with sr's prefix if it doesn't already hold it (Context.CopySeq
// from PrimedSeq, or Context.RestoreSeq of PrefixState), and sets up sl to
// begin prefilling sr's own prompt from StartPos onward next round.
func (s *Scheduler) admitInto(slotID int32, sl *slot, sr *schedRequest, vocab *Vocab) error {
	tokens, err := vocab.Tokenize(sr.req.Prompt, true, true)
	if err != nil {
		return fmt.Errorf("llamacpp: tokenize prompt: %w", err)
	}
	if len(tokens) == 0 {
		return fmt.Errorf("llamacpp: empty prompt tokenized to zero tokens")
	}
	if len(sr.req.Steps) > 0 {
		tokens = append(tokens, sr.req.Steps[0].Prefix...)
	}
	startPos := sr.req.StartPos
	if int(startPos) > len(tokens) {
		return fmt.Errorf("llamacpp: startPos %d exceeds prompt token count %d", startPos, len(tokens))
	}

	// A sequence that runs out of cells fails the whole round's Decode, and
	// with it every other slot's request ("failed to find a memory slot"),
	// so a request is held to its own sequence here: one whose prompt
	// doesn't fit is refused, and one whose reply could outgrow the room
	// left fails alone when it does (ErrContextFull).
	room := int(s.ctx.NCtxSeq()) - len(tokens)
	if room <= 0 {
		return fmt.Errorf("%w: a prompt of %d tokens in a sequence of %d", ErrContextFull, len(tokens), s.ctx.NCtxSeq())
	}
	left := sr.req.MaxTokens
	ctxFull := len(sr.req.Steps) == 0 && left > room
	if ctxFull {
		left = room
	}

	kept := s.kept[slotID]
	s.kept[slotID] = keptPrefix{}
	switch {
	case startPos == 0:
		s.ctx.TrimSequence(slotID, 0)
	case sr.req.PrefixState != nil:
		// Slot affinity: the slot's last request left this same prefix
		// behind (release), so cut back to it instead of restoring.
		if kept.state == &sr.req.PrefixState[0] && kept.n == startPos && s.ctx.TrimSequence(slotID, startPos) {
			s.roundsMu.Lock()
			s.rounds.PrefixReused++
			s.roundsMu.Unlock()
			break
		}
		t := time.Now()
		s.ctx.TrimSequence(slotID, 0)
		if !s.ctx.RestoreSeq(slotID, sr.req.PrefixState) {
			s.ctx.TrimSequence(slotID, 0)
			startPos = 0
		}
		s.roundsMu.Lock()
		s.rounds.PrefixRestored++
		s.rounds.RestoreTime += time.Since(t)
		s.roundsMu.Unlock()
	default:
		s.ctx.TrimSequence(slotID, 0)
		s.ctx.CopySeq(sr.req.PrimedSeq, slotID, 0, startPos)
	}

	*sl = slot{
		active:    true,
		req:       sr,
		pos:       startPos,
		remaining: tokens[startPos:],
		left:      left,
		ctxFull:   ctxFull,
		admitted:  time.Now(),
		stats: GenStats{
			ReusedTokens: int(startPos),
			PromptTokens: len(tokens) - int(startPos),
		},
	}
	return nil
}
