// Built by cgo alongside mtp.go. Must be compiled against the same
// llama.cpp commit's common.h/speculative.h as the libllama-common it links:
// common_params_speculative is a C++ struct passed across that boundary.

#include "mtp.h"

#include "common.h"
#include "speculative.h"

#include <vector>

static const uint32_t LCG_MTP_MAX_UBATCH = 512;

struct lcg_mtp {
    llama_context *                   dft = nullptr;
    common_speculative *              spec = nullptr;
    std::vector<llama_tokens>         results;
    std::vector<llama_tokens>         prompts; // unused by MTP, but dereferenced by debug logging
};

extern "C" {

lcg_mtp * lcg_mtp_init(llama_context * tgt, llama_context_params dft_params, int32_t n_seq, int32_t n_max) {
    // Mirrors common_speculative_init_result for draft-mtp: the draft
    // context shares the target's weights and sizing, keeps no recurrent
    // rollback snapshots, and points back at the target.
    dft_params.ctx_type  = LLAMA_CONTEXT_TYPE_MTP;
    dft_params.n_ctx     = llama_n_ctx(tgt);
    dft_params.n_rs_seq  = 0;
    dft_params.ctx_other = tgt;
    // The draft context's compute buffer scales with its ubatch - 3.9 GiB at
    // the target's 2048 for Qwen3.6-35B-A3B (its vocab-wide output rows),
    // which pushed model + both contexts past 24 GB and into shared memory.
    // It decodes a few tokens per draft step; prompt chunks just take more
    // ubatches.
    if (dft_params.n_ubatch > LCG_MTP_MAX_UBATCH) {
        dft_params.n_ubatch = LCG_MTP_MAX_UBATCH;
    }
    // Only draft steps read logits, one row per drafting sequence.
    dft_params.n_outputs_max         = (uint32_t) n_seq;
    dft_params.n_outputs_max_per_seq = 1;

    llama_context * dft = llama_init_from_model(const_cast<llama_model *>(llama_get_model(tgt)), dft_params);
    if (dft == nullptr) {
        return nullptr;
    }

    common_params_speculative params;
    params.types        = { COMMON_SPECULATIVE_TYPE_DRAFT_MTP };
    params.draft.n_max  = n_max;
    params.draft.ctx_tgt = tgt;
    params.draft.ctx_dft = dft;

    common_speculative * spec = nullptr;
    try {
        spec = common_speculative_init(params, (uint32_t) n_seq);
    } catch (...) {
        spec = nullptr;
    }
    if (spec == nullptr) {
        llama_free(dft);
        return nullptr;
    }

    auto * m = new lcg_mtp;
    m->dft  = dft;
    m->spec = spec;
    m->results.resize(n_seq);
    m->prompts.resize(n_seq);
    return m;
}

void lcg_mtp_free(lcg_mtp * m) {
    if (m == nullptr) {
        return;
    }
    common_speculative_free(m->spec);
    llama_free(m->dft);
    delete m;
}

llama_context * lcg_mtp_ctx(lcg_mtp * m) {
    return m->dft;
}

bool lcg_mtp_process(lcg_mtp * m, llama_batch batch) {
    return common_speculative_process(m->spec, batch);
}

void lcg_mtp_draft(lcg_mtp * m, int32_t n, const int32_t * seq_ids, const llama_token * id_last,
                   const llama_pos * pos0, const int32_t * n_max, llama_token * out_tokens, int32_t stride,
                   int32_t * out_n) {
    for (int32_t i = 0; i < n; ++i) {
        const llama_seq_id seq = seq_ids[i];
        m->results[seq].clear();
        common_speculative_get_draft_params(m->spec, seq) = {
            /* .drafting = */ true,
            /* .n_max    = */ n_max[i],
            /* .pos0     = */ pos0[i],
            /* .id_last  = */ id_last[i],
            /* .prompt   = */ &m->prompts[seq],
            /* .result   = */ &m->results[seq],
        };
    }

    common_speculative_draft(m->spec);

    // Drafting decoded into the draft context from pos0 on; the target's
    // verify batch re-feeds those positions through lcg_mtp_process, so drop
    // them first (llama-server does the same after its draft step).
    llama_memory_t mem = llama_get_memory(m->dft);
    for (int32_t i = 0; i < n; ++i) {
        llama_memory_seq_rm(mem, seq_ids[i], pos0[i], -1);
    }

    for (int32_t i = 0; i < n; ++i) {
        const llama_seq_id seq = seq_ids[i];
        const auto & r = m->results[seq];
        int32_t k = 0;
        for (; k < (int32_t) r.size() && k < stride; ++k) {
            out_tokens[(size_t) i * stride + k] = r[k];
        }
        out_n[i] = k;
        common_speculative_get_draft_params(m->spec, seq).drafting = false;
    }
}

void lcg_mtp_accept(lcg_mtp * m, llama_seq_id seq_id, int32_t n_accepted) {
    common_speculative_accept(m->spec, seq_id, (uint16_t) n_accepted);
}

} // extern "C"
