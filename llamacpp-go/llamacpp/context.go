package llamacpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -lllama -lggml -lggml-base -lggml-cpu
#include <stdlib.h>
#include <llama.h>
*/
import "C"

import "unsafe"

// ContextParams configures Model.NewContext, mirroring the handful of
// llama_context_params fields most callers need. Everything else keeps
// llama_context_default_params()'s value.
type ContextParams struct {
	// NCtx is the context window size in tokens. 0 (the zero value) means
	// "use the model's trained context size" (Model.NCtxTrain()).
	NCtx uint32

	// NBatch is the max number of tokens llama_decode can be given in one
	// call; Context.Decode chunks a longer batch into pieces this size.
	// 0 defaults to 2048.
	NBatch uint32

	// NThreads is the number of threads used for single-token (generation)
	// decoding; NThreadsBatch for multi-token (prompt) decoding. 0 leaves
	// libllama's own default (typically all detected cores).
	NThreads      int32
	NThreadsBatch int32

	// NSeqMax is the number of independent sequences (distinct KV-cache
	// slots) this Context can hold decoded at once. 0 (the zero value)
	// leaves libllama's own default of 1 -- a single caller reusing one
	// sequence, e.g. Context.Generate/GenerateFrom. Pass a value > 1 to
	// back a Scheduler (see scheduler.go), which needs one sequence id per
	// concurrent in-flight generation plus one more per distinct primed
	// prefix it reuses across them (see Scheduler's own doc comment) --
	// the KV cache is sized for NSeqMax * NCtx tokens total, so raise this
	// only as far as actual concurrency needs, not "to be safe".
	NSeqMax uint32

	// KVUnified selects whether every sequence shares one KV-cache buffer
	// ("stream") or each gets its own. libllama itself defaults this to
	// false (n_seq_max separate per-sequence buffers) -- fine, and slightly
	// faster, for sequences that share no meaningful prefix, but it makes
	// Context.CopySeq between two different sequences an expensive
	// cross-stream buffer copy that only actually works when copying the
	// *entire* KV buffer (llama.cpp's own "seq_cp() is only supported for
	// full KV buffers" assertion) -- unusable for copying just a shared
	// prefix's own range into a fresh sequence. A Scheduler (see
	// scheduler.go) relies on exactly that partial-range copy to seed a
	// generation slot from a separately-primed prefix sequence, so any
	// Context backing one that ever uses GenRequest.PrimedSeq must set
	// KVUnified true -- true puts every sequence in the same stream, making
	// CopySeq a cheap same-stream metadata update instead (cells.seq_add in
	// llama.cpp's own implementation), with no full-buffer restriction.
	KVUnified bool
}

func (p ContextParams) cParams() C.struct_llama_context_params {
	c := C.llama_context_default_params()
	if p.NCtx != 0 {
		c.n_ctx = C.uint32_t(p.NCtx)
	}
	if p.NBatch != 0 {
		c.n_batch = C.uint32_t(p.NBatch)
		c.n_ubatch = C.uint32_t(p.NBatch)
	}
	if p.NThreads != 0 {
		c.n_threads = C.int32_t(p.NThreads)
	}
	if p.NThreadsBatch != 0 {
		c.n_threads_batch = C.int32_t(p.NThreadsBatch)
	}
	if p.NSeqMax != 0 {
		c.n_seq_max = C.uint32_t(p.NSeqMax)
	}
	c.kv_unified = C.bool(p.KVUnified)
	return c
}

// Context wraps a loaded model's llama_context -- the KV cache and
// per-sequence decode state a generation loop runs against.
type Context struct {
	handle *C.struct_llama_context
	model  *Model // keeps the model referenced (and alive) for as long as the Context is
}

// NewContext creates a Context for this model. The returned *Context must
// be closed before the Model is.
func (m *Model) NewContext(params ContextParams) (*Context, error) {
	// ContextParams.NCtx's own doc comment promises 0 means "use the
	// model's trained context size" - but llama_context_default_params()
	// (what cParams() leaves NCtx at when params.NCtx is 0) actually
	// defaults to a fixed 512, not the model's own NCtxTrain(). Substitute
	// it explicitly here so that promise holds; without this, a caller
	// that (reasonably) leaves NCtx at its zero value silently gets a
	// 512-token context regardless of the model's real capacity, which
	// fails outright ("failed to find a memory slot") the moment a single
	// prompt+generation exceeds 512 tokens combined.
	if params.NCtx == 0 {
		if t := m.NCtxTrain(); t > 0 {
			params.NCtx = uint32(t)
		}
	}
	handle := C.llama_init_from_model(m.handle, params.cParams())
	if handle == nil {
		return nil, newError("llama_init_from_model")
	}
	return &Context{handle: handle, model: m}, nil
}

// Close frees the underlying llama_context. Safe to call more than once, and
// on a nil *Context.
func (c *Context) Close() {
	if c == nil || c.handle == nil {
		return
	}
	C.llama_free(c.handle)
	c.handle = nil
}

// Model returns the Model this Context was created from.
func (c *Context) Model() *Model { return c.model }

// NCtx returns the context's actual size in tokens (which may differ from
// the requested ContextParams.NCtx -- see llama.h's note on
// llama_context_params).
func (c *Context) NCtx() uint32 {
	return uint32(C.llama_n_ctx(c.handle))
}

// NBatch returns the context's actual max batch size.
func (c *Context) NBatch() uint32 {
	return uint32(C.llama_n_batch(c.handle))
}

// NSeqMax returns the number of independent sequences this Context was
// actually created to hold at once (see ContextParams.NSeqMax).
func (c *Context) NSeqMax() uint32 {
	return uint32(C.llama_n_seq_max(c.handle))
}

// Decode runs the model forward over batch, chunking it into pieces no
// larger than NBatch if needed. It updates the context's KV cache and the
// logits retrievable via LogitsIth.
func (c *Context) Decode(batch *Batch) error {
	maxBatch := int(c.NBatch())
	if batch.n <= maxBatch {
		return c.decodeChunk(batch.cBatch(0, batch.n))
	}
	for start := 0; start < batch.n; start += maxBatch {
		n := maxBatch
		if start+n > batch.n {
			n = batch.n - start
		}
		if err := c.decodeChunk(batch.cBatch(start, n)); err != nil {
			return err
		}
	}
	return nil
}

func (c *Context) decodeChunk(cb C.struct_llama_batch) error {
	if ret := C.llama_decode(c.handle, cb); ret != 0 {
		return newError("llama_decode")
	}
	return nil
}

// LogitsIth returns the raw logits (one per vocabulary entry) for the ith
// token in the most recently decoded batch that requested logits (see
// Batch.SetLogits) -- pass -1 for the last such token.
func (c *Context) LogitsIth(i int32) []float32 {
	nVocab := int(C.llama_vocab_n_tokens(C.llama_model_get_vocab(c.model.handle)))
	ptr := C.llama_get_logits_ith(c.handle, C.int32_t(i))
	if ptr == nil {
		return nil
	}
	return unsafe.Slice((*float32)(unsafe.Pointer(ptr)), nVocab)
}

// TrimSequence removes every cached token for seqID at position >= p0 from
// this Context's KV cache, leaving [0, p0) intact. This is the building
// block for reusing one Context across independent prompts that share a
// fixed prefix (e.g. a constant system prompt): decode the shared prefix
// once (DecodePrompt), remember its length as p0, and before each later,
// unrelated continuation call TrimSequence(seqID, p0) to discard whatever
// the previous call decoded past that point (its own user turn, sampled
// tokens) without losing the reusable prefix itself -- then decode/
// generate the new call's own content starting at position p0 (see
// GenerateFrom). Returns false if part of the sequence couldn't be
// removed (a memory-type limitation -- e.g. certain SWA cache
// configurations); a caller getting false back should treat this
// Context's state as no longer trustworthy and fall back to a fresh
// Context rather than continue decoding into it.
func (c *Context) TrimSequence(seqID int32, p0 int32) bool {
	mem := C.llama_get_memory(c.handle)
	return bool(C.llama_memory_seq_rm(mem, C.llama_seq_id(seqID), C.llama_pos(p0), C.llama_pos(-1)))
}

// CopySeq copies every cached token of srcSeq in [p0, p1) into dstSeq,
// leaving srcSeq itself untouched -- the building block a Scheduler (see
// scheduler.go) uses to give each of several concurrent generations its own
// copy of one shared, already-decoded prefix (e.g. a constant system
// prompt), instead of TrimSequence's single-sequence in-place reuse, which
// only ever serves one caller of that prefix at a time. p1 < 0 means "to the
// end of srcSeq's own cached range". Unlike TrimSequence, llama.cpp's own
// seq_cp has no failure signal (it returns void) -- there is no bool to
// check here; a copy from an exotic memory type that can't actually be
// duplicated this way would silently do nothing rather than report false,
// so callers relying on this for correctness (not just an optimization)
// should still verify the result the same way tryPrimed-style callers
// already verify TrimSequence's own reuse (token-for-token, against what
// was actually decoded) rather than trusting the copy blindly.
func (c *Context) CopySeq(srcSeq, dstSeq int32, p0, p1 int32) {
	mem := C.llama_get_memory(c.handle)
	C.llama_memory_seq_cp(mem, C.llama_seq_id(srcSeq), C.llama_seq_id(dstSeq), C.llama_pos(p0), C.llama_pos(p1))
}

// StateSeqSize returns the exact number of bytes SaveSeq(seqID) would
// currently copy out (llama_state_seq_get_size) - only meaningful to call
// right before SaveSeq, per llama_state_seq_get_size's own doc comment
// ("only use when saving the state, not when restoring it").
func (c *Context) StateSeqSize(seqID int32) int {
	return int(C.llama_state_seq_get_size(c.handle, C.llama_seq_id(seqID)))
}

// SaveSeq copies seqID's entire current per-sequence state - recurrent/SSM
// state and/or KV cache, whatever this architecture's memory type actually
// holds - into a freshly-allocated buffer, returning it. This is the save
// half of a save/restore pair (with RestoreSeq) usable as a fallback for
// architectures whose memory TrimSequence can't rewind in place (see its
// own doc comment on "memory-type limitation"): llama.cpp explicitly
// documents recurrent caches (e.g. Mamba-style, which the same category
// covers Gated-Delta-Network-style hybrid architectures like Qwen3.5's) as
// a target use case for this state save/restore API
// (LLAMA_STATE_SEQ_FLAGS_PARTIAL_ONLY's own doc comment in llama.h),
// distinct from llama_memory_seq_rm's in-place edit, which those same
// architectures can decline. Returns nil if there's nothing to save
// (StateSeqSize would be 0).
func (c *Context) SaveSeq(seqID int32) []byte {
	size := C.llama_state_seq_get_size(c.handle, C.llama_seq_id(seqID))
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	n := C.llama_state_seq_get_data(c.handle, (*C.uint8_t)(unsafe.Pointer(&buf[0])), size, C.llama_seq_id(seqID))
	return buf[:n]
}

// RestoreSeq restores seqID's state from data previously returned by
// SaveSeq (typically from an earlier call on this same Context, or one
// created from the same Model) - the restore half of SaveSeq's save/
// restore pair. Returns false if data was rejected as invalid/incompatible
// (llama_state_seq_set_data returns 0 - a malformed buffer, or one from an
// incompatible model/context configuration), in which case seqID's own
// state is left in whatever state llama.cpp's own implementation leaves a
// failed load in - callers should treat it as untrustworthy, the same as a
// false return from TrimSequence.
func (c *Context) RestoreSeq(seqID int32, data []byte) bool {
	if len(data) == 0 {
		return false
	}
	n := C.llama_state_seq_set_data(c.handle, (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), C.llama_seq_id(seqID))
	return n > 0
}
