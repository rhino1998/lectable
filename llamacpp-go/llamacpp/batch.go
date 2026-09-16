package llamacpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -lllama -lggml -lggml-base -lggml-cpu
#include <stdlib.h>
#include <llama.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Batch wraps a llama_batch -- the unit llama_decode consumes. Tokens are
// appended one at a time with Add; each carries its position in its
// sequence and whether its logits should be kept around afterward
// (typically true only for the last token of a prompt, and every token
// while generating one at a time).
//
// A Batch is reusable across calls to Context.Decode: call Reset between
// generation steps instead of allocating a new one.
type Batch struct {
	cb  C.struct_llama_batch
	n   int // tokens appended so far
	cap int // capacity passed to llama_batch_init; 0 once Closed
}

// NewBatch allocates a Batch that can hold up to capacity tokens, each
// assignable to a single sequence. Close it when done.
func NewBatch(capacity int) *Batch {
	return &Batch{
		cb:  C.llama_batch_init(C.int32_t(capacity), 0, 1),
		cap: capacity,
	}
}

// Close frees the underlying llama_batch. Safe to call more than once, and
// on a nil *Batch.
func (b *Batch) Close() {
	if b == nil || b.cap == 0 {
		return
	}
	C.llama_batch_free(b.cb)
	b.cap = 0
}

// Reset empties the batch so it can be refilled with Add, without
// reallocating.
func (b *Batch) Reset() { b.n = 0 }

// NTokens returns the number of tokens currently in the batch.
func (b *Batch) NTokens() int { return b.n }

// Add appends tok at position pos of sequence seq. wantLogits requests that
// this token's logits be retrievable afterward via Context.LogitsIth; for a
// prompt, that only needs to be true for the final token, since generation
// only ever samples the next token after the last one decoded.
//
// Add returns an error without modifying the batch if it is already at the
// capacity passed to NewBatch.
func (b *Batch) Add(tok Token, pos int32, seq int32, wantLogits bool) error {
	if b.n >= b.cap {
		return fmt.Errorf("llamacpp: Batch.Add: batch is full (capacity %d)", b.cap)
	}
	i := b.n
	*ptrAt(b.cb.token, i) = C.llama_token(tok)
	*ptrAt(b.cb.pos, i) = C.llama_pos(pos)
	*ptrAt(b.cb.n_seq_id, i) = 1
	seqIDs := *ptrAt(b.cb.seq_id, i) // llama_batch_init allocates seq_id[i] with room for n_seq_max (1) ids
	*seqIDs = C.llama_seq_id(seq)
	var logit C.int8_t
	if wantLogits {
		logit = 1
	}
	*ptrAt(b.cb.logits, i) = logit
	b.n++
	return nil
}

// cBatch returns a llama_batch view over [start, start+n) of this Batch's
// storage, for Context.Decode to hand to llama_decode in NBatch-sized
// chunks.
func (b *Batch) cBatch(start, n int) C.struct_llama_batch {
	return C.struct_llama_batch{
		n_tokens: C.int32_t(n),
		token:    ptrAt(b.cb.token, start),
		pos:      ptrAt(b.cb.pos, start),
		n_seq_id: ptrAt(b.cb.n_seq_id, start),
		seq_id:   ptrAt(b.cb.seq_id, start),
		logits:   ptrAt(b.cb.logits, start),
	}
}

// ptrAt indexes a C array pointer by element, the way p[i] would in C --
// Go's unsafe.Pointer arithmetic equivalent, since cgo pointers don't
// support indexing directly.
func ptrAt[T any](p *T, i int) *T {
	return (*T)(unsafe.Add(unsafe.Pointer(p), i*int(unsafe.Sizeof(*p))))
}
