package llamacpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo CXXFLAGS: -std=c++17 -I${SRCDIR}/include
#cgo LDFLAGS: -lllama-common
#include <stdlib.h>
#include "mtp.h"
*/
import "C"

import (
	"encoding/binary"
	"errors"
	"unsafe"
)

// mtpDrafter is a Context's MTP (multi-token prediction) draft side: a
// second llama_context running the model's own MTP head, driven by
// llama.cpp's common_speculative draft-mtp implementation (mtp.cpp).
type mtpDrafter struct {
	h    *C.lcg_mtp
	dft  *C.struct_llama_context
	nMax int
}

// EnableMTP attaches MTP speculative decoding to c: a draft context
// running the MTP head of c's own model, which must have been loaded with
// ModelParams.LoadMTP, drafting up to nMax tokens per step. params should
// be the ContextParams c itself was created with (the draft context mirrors
// its batch sizes, KV types and sequence layout). For a hybrid (recurrent)
// model, c must have been created with ContextParams.NRsSeq >= nMax, or
// rejected drafts can't be rolled back.
//
// From then on every Decode also feeds the draft context, and
// TrimSequence/CopySeq/SaveSeq/RestoreSeq apply to both, so callers keep
// using c exactly as before; a Scheduler over c drafts and verifies (see
// Scheduler). Plain Generate/GenerateFrom don't draft, but stay correct.
func (c *Context) EnableMTP(params ContextParams, nMax int) error {
	if c.mtp != nil {
		return errors.New("llamacpp: EnableMTP: already enabled")
	}
	if nMax < 1 {
		return errors.New("llamacpp: EnableMTP: nMax must be >= 1")
	}
	cp := params.cParams()
	cp.n_ctx = C.llama_n_ctx(c.handle)
	h := C.lcg_mtp_init(c.handle, cp, C.int32_t(c.NSeqMax()), C.int32_t(nMax))
	if h == nil {
		return newError("lcg_mtp_init")
	}
	c.mtp = &mtpDrafter{h: h, dft: C.lcg_mtp_ctx(h), nMax: nMax}
	return nil
}

// MTPEnabled reports whether EnableMTP succeeded on c.
func (c *Context) MTPEnabled() bool { return c.mtp != nil }

func (m *mtpDrafter) close() {
	C.lcg_mtp_free(m.h)
}

func (m *mtpDrafter) process(cb C.struct_llama_batch) error {
	if !C.lcg_mtp_process(m.h, cb) {
		return newError("lcg_mtp_process")
	}
	return nil
}

// mtpDraftReq is one sequence's draft request for mtpDrafter.draft.
type mtpDraftReq struct {
	seq    int32
	idLast Token // sampled, not yet decoded
	pos0   int32 // idLast's position
	nMax   int
}

// draft returns up to reqs[i].nMax drafted tokens for each request, all
// drafted together in shared draft-context decodes.
func (m *mtpDrafter) draft(reqs []mtpDraftReq) [][]Token {
	n := len(reqs)
	if n == 0 {
		return nil
	}
	seqs := make([]C.int32_t, n)
	last := make([]C.llama_token, n)
	pos0 := make([]C.llama_pos, n)
	nMax := make([]C.int32_t, n)
	for i, r := range reqs {
		seqs[i] = C.int32_t(r.seq)
		last[i] = C.llama_token(r.idLast)
		pos0[i] = C.llama_pos(r.pos0)
		nMax[i] = C.int32_t(r.nMax)
	}
	stride := m.nMax
	out := make([]C.llama_token, n*stride)
	outN := make([]C.int32_t, n)
	C.lcg_mtp_draft(m.h, C.int32_t(n), &seqs[0], &last[0], &pos0[0], &nMax[0], &out[0], C.int32_t(stride), &outN[0])
	res := make([][]Token, n)
	for i := range reqs {
		d := make([]Token, int(outN[i]))
		for k := range d {
			d[k] = Token(out[i*stride+k])
		}
		res[i] = d
	}
	return res
}

func (m *mtpDrafter) accept(seq int32, nAccepted int) {
	C.lcg_mtp_accept(m.h, C.llama_seq_id(seq), C.int32_t(nAccepted))
}

// mtpStateMagic prefixes a SaveSeq snapshot that carries both contexts'
// state; see Context.SaveSeq.
var mtpStateMagic = [8]byte{'L', 'C', 'G', 'M', 'T', 'P', '0', '1'}

// joinMTPState packs a target and a draft sequence snapshot into one.
func joinMTPState(tgt, dft []byte) []byte {
	buf := make([]byte, 0, 16+len(tgt)+len(dft))
	buf = append(buf, mtpStateMagic[:]...)
	buf = binary.LittleEndian.AppendUint64(buf, uint64(len(tgt)))
	buf = append(buf, tgt...)
	return append(buf, dft...)
}

// splitMTPState undoes joinMTPState; ok is false for a plain snapshot.
func splitMTPState(data []byte) (tgt, dft []byte, ok bool) {
	if len(data) < 16 || [8]byte(data[:8]) != mtpStateMagic {
		return nil, nil, false
	}
	n := binary.LittleEndian.Uint64(data[8:16])
	if n > uint64(len(data)-16) {
		return nil, nil, false
	}
	return data[16 : 16+n], data[16+n:], true
}

func seqStateGet(ctx *C.struct_llama_context, seqID int32) []byte {
	size := C.llama_state_seq_get_size(ctx, C.llama_seq_id(seqID))
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	n := C.llama_state_seq_get_data(ctx, (*C.uint8_t)(unsafe.Pointer(&buf[0])), size, C.llama_seq_id(seqID))
	return buf[:n]
}

func seqStateSet(ctx *C.struct_llama_context, seqID int32, data []byte) bool {
	if len(data) == 0 {
		return false
	}
	n := C.llama_state_seq_set_data(ctx, (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), C.llama_seq_id(seqID))
	return n > 0
}
