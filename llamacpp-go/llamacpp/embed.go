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

// PoolingType is ContextParams.Pooling - how an embedding context turns a
// sequence's token states into one vector.
type PoolingType int

const (
	// PoolingDefault takes the pooling the model's GGUF names.
	PoolingDefault PoolingType = iota
	PoolingMean
	PoolingCLS
	PoolingLast
)

func (t PoolingType) cType() (C.enum_llama_pooling_type, bool) {
	switch t {
	case PoolingMean:
		return C.LLAMA_POOLING_TYPE_MEAN, true
	case PoolingCLS:
		return C.LLAMA_POOLING_TYPE_CLS, true
	case PoolingLast:
		return C.LLAMA_POOLING_TYPE_LAST, true
	}
	return 0, false
}

// Embed returns one embedding per sequence in seqs, as the model wrote
// them (not normalized). The context must have been made with
// ContextParams.Embeddings; every sequence is evaluated in a single
// llama_decode call, so seqs must fit the context: at most NSeqMax
// sequences and NBatch tokens between them (an embedding model attends
// across a whole sequence at once, so a sequence can't be split over
// calls - make the context with NUBatch equal to NBatch). Whatever the
// context held before is discarded. Not safe for concurrent use.
func (c *Context) Embed(seqs [][]Token) ([][]float32, error) {
	if len(seqs) == 0 {
		return nil, nil
	}
	if len(seqs) > int(c.NSeqMax()) {
		return nil, fmt.Errorf("llamacpp: Embed: %d sequences, context holds %d", len(seqs), c.NSeqMax())
	}
	total := 0
	for i, seq := range seqs {
		if len(seq) == 0 {
			return nil, fmt.Errorf("llamacpp: Embed: sequence %d is empty", i)
		}
		total += len(seq)
	}
	if total > int(c.NBatch()) {
		return nil, fmt.Errorf("llamacpp: Embed: %d tokens, context batch holds %d", total, c.NBatch())
	}

	batch := NewBatch(total)
	defer batch.Close()
	for i, seq := range seqs {
		for pos, tok := range seq {
			if err := batch.Add(tok, int32(pos), int32(i), true); err != nil {
				return nil, err
			}
		}
	}
	C.llama_memory_clear(C.llama_get_memory(c.handle), true)
	if ret := C.llama_decode(c.handle, batch.cBatch(0, batch.n)); ret != 0 {
		return nil, newError("llama_decode")
	}

	dim := int(C.llama_model_n_embd_out(c.model.handle))
	out := make([][]float32, len(seqs))
	for i := range seqs {
		p := C.llama_get_embeddings_seq(c.handle, C.llama_seq_id(i))
		if p == nil {
			return nil, newError("llama_get_embeddings_seq")
		}
		out[i] = append([]float32(nil), unsafe.Slice((*float32)(unsafe.Pointer(p)), dim)...)
	}
	return out, nil
}
