package llamacpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -lllama -lggml -lggml-base -lggml-cpu
#include <stdlib.h>
#include <llama.h>
*/
import "C"

// SamplerParams configures NewSampler's chain: top-k / top-p / min-p
// filtering followed by temperature, ending in a distribution sample (or
// greedy argmax if Temp <= 0). This covers ordinary chat/completion
// sampling; build a chain by hand with llama_sampler_chain_add (not
// currently exposed) if a call site needs more exotic samplers
// (mirostat, DRY, grammars, logit bias).
type SamplerParams struct {
	// Seed for the final distribution sample. 0 (the zero value) uses
	// LLAMA_DEFAULT_SEED, which libllama seeds from entropy.
	Seed uint32

	// TopK keeps only the k highest-probability tokens before sampling.
	// <= 0 disables this filter.
	TopK int32

	// TopP keeps the smallest set of tokens whose cumulative probability
	// exceeds p (nucleus sampling). A value outside (0, 1) disables this
	// filter.
	TopP float32

	// MinP discards tokens with probability below p * (probability of the
	// most likely token). <= 0 disables this filter.
	MinP float32

	// Temp scales logits before sampling; lower is more deterministic.
	// <= 0 replaces the whole chain with greedy argmax (Seed, TopK, TopP,
	// MinP are then ignored).
	Temp float32
}

// Sampler wraps a llama_sampler chain built from SamplerParams.
type Sampler struct {
	handle *C.struct_llama_sampler
}

// NewSampler builds a sampler chain per params. Close it when done.
func NewSampler(params SamplerParams) *Sampler {
	chain := C.llama_sampler_chain_init(C.llama_sampler_chain_default_params())

	if params.Temp <= 0 {
		C.llama_sampler_chain_add(chain, C.llama_sampler_init_greedy())
		return &Sampler{handle: chain}
	}

	if params.TopK > 0 {
		C.llama_sampler_chain_add(chain, C.llama_sampler_init_top_k(C.int32_t(params.TopK)))
	}
	if params.TopP > 0 && params.TopP < 1 {
		C.llama_sampler_chain_add(chain, C.llama_sampler_init_top_p(C.float(params.TopP), 1))
	}
	if params.MinP > 0 {
		C.llama_sampler_chain_add(chain, C.llama_sampler_init_min_p(C.float(params.MinP), 1))
	}
	C.llama_sampler_chain_add(chain, C.llama_sampler_init_temp(C.float(params.Temp)))

	seed := params.Seed
	if seed == 0 {
		seed = uint32(C.LLAMA_DEFAULT_SEED)
	}
	C.llama_sampler_chain_add(chain, C.llama_sampler_init_dist(C.uint32_t(seed)))

	return &Sampler{handle: chain}
}

// Close frees the sampler chain. Safe to call more than once, and on a nil
// *Sampler.
func (s *Sampler) Close() {
	if s == nil || s.handle == nil {
		return
	}
	C.llama_sampler_free(s.handle)
	s.handle = nil
}

// Reset clears any per-sequence state the chain has accumulated (relevant
// once a call site adds stateful samplers like repetition penalties;
// harmless no-op for the top-k/top-p/temp/dist chain NewSampler builds).
// Call it between independent generations that reuse the same Sampler.
func (s *Sampler) Reset() {
	C.llama_sampler_reset(s.handle)
}

// Sample picks and accepts the next token from the idx-th output of the
// context's last Decode call (pass -1 for the last token decoded).
func (s *Sampler) Sample(ctx *Context, idx int32) Token {
	return Token(C.llama_sampler_sample(s.handle, ctx.handle, C.int32_t(idx)))
}
