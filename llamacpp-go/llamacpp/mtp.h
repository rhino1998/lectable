// C shim over llama.cpp's common_speculative MTP (multi-token prediction)
// driver, which is C++ only (common/speculative.h). See mtp.go.
#pragma once

#include <stdbool.h>
#include <stdint.h>

#include <llama.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct lcg_mtp lcg_mtp;

// lcg_mtp_init creates an MTP draft context running the MTP head of tgt's
// own model (loaded with load_mtp), configured like tgt from dft_params, and
// a draft-mtp speculative driver over n_seq sequences drafting up to n_max
// tokens per step. Returns NULL on failure.
lcg_mtp * lcg_mtp_init(struct llama_context * tgt, struct llama_context_params dft_params, int32_t n_seq, int32_t n_max);

void lcg_mtp_free(lcg_mtp * m);

// lcg_mtp_ctx returns the draft context, for mirroring KV-cache edits.
struct llama_context * lcg_mtp_ctx(lcg_mtp * m);

// lcg_mtp_process feeds a batch tgt just decoded to the draft context.
// Every batch tgt decodes must go through here, prompt chunks included.
bool lcg_mtp_process(lcg_mtp * m, struct llama_batch batch);

// lcg_mtp_draft drafts for n sequences at once: sequence seq_ids[i], whose
// last sampled (not yet decoded) token is id_last[i] at position pos0[i],
// gets up to n_max[i] tokens in out_tokens[i*stride...], count in out_n[i].
void lcg_mtp_draft(lcg_mtp * m, int32_t n, const int32_t * seq_ids, const llama_token * id_last,
                   const llama_pos * pos0, const int32_t * n_max, llama_token * out_tokens, int32_t stride,
                   int32_t * out_n);

// lcg_mtp_accept tells the driver how many of seq_id's drafted tokens the
// target accepted in the batch last passed to lcg_mtp_process.
void lcg_mtp_accept(lcg_mtp * m, llama_seq_id seq_id, int32_t n_accepted);

#ifdef __cplusplus
}
#endif
