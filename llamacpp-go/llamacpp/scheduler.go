package llamacpp

import (
	"context"
	"errors"
	"fmt"
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
	text  string
	err   error
	stats GenStats
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
			if err := batch.Add(sl.nextTok, sl.pos, int32(i), true); err != nil {
				continue // batch genuinely full; picked up again next round
			}
			outputIdx[i] = batchPos
			hasOutput[i] = true
			batchPos++
			sl.pos++
			budget--
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
			if sl.left <= 0 {
				// MaxTokens <= 0: the prompt is decoded (same as
				// GenerateFrom always does, unconditionally) but nothing is
				// ever sampled from it, matching GenerateFrom's own
				// `for n := 0; n < maxTokens; n++` never running its body.
				s.release(int32(i), sl)
				sl.finish(schedResult{text: string(sl.generated)})
				continue
			}
			next := sl.req.req.Sampler.Sample(s.ctx, outputIdx[i])
			sl.stats.GenTokens++
			if vocab.IsEOG(next) {
				s.release(int32(i), sl)
				sl.finish(schedResult{text: string(sl.generated)})
				continue
			}

			piece := vocab.TokenToPiece(next, false)
			sl.generated = append(sl.generated, piece...)
			if onPiece := sl.req.req.OnPiece; onPiece != nil && !onPiece(piece) {
				s.release(int32(i), sl)
				sl.finish(schedResult{text: string(sl.generated)})
				continue
			}

			sl.left--
			if sl.left <= 0 {
				s.release(int32(i), sl)
				sl.finish(schedResult{text: string(sl.generated)})
				continue
			}
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
	startPos := sr.req.StartPos
	if int(startPos) > len(tokens) {
		return fmt.Errorf("llamacpp: startPos %d exceeds prompt token count %d", startPos, len(tokens))
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
		left:      sr.req.MaxTokens,
		admitted:  time.Now(),
		stats: GenStats{
			ReusedTokens: int(startPos),
			PromptTokens: len(tokens) - int(startPos),
		},
	}
	return nil
}
