# Local audio.cpp patches

Moved out of `../CLAUDE.md`. What this box's `/home/rhino/audio.cpp`
checkout carries on top of upstream, why, and what was measured: the Higgs
graph-cache leak and its fix, the non-terminating-generation guard, the
throughput patches, and the latents/codec artifact API. None of it is
committed there or upstreamed, so a fresh or reset checkout has none of it.

## The Higgs leak

audio.cpp (the C++ engine doing TTS inference, wrapped by `../../audiocpp-go`)
has a confirmed native memory leak - found via heaptrack against a real
workload, in `higgs_audio_tts`'s KV-cache/decode-graph reconstruction path
(ASan can't even get a clean trace here: it crashes inside the HIP driver's
own init on this box, and an `RTLD_DEEPBIND` workaround just traded that
crash for a different allocator-mismatch one - heaptrack, not ASan, is what
actually pinned this down). Root-caused by reading audio.cpp's own source
(`src/models/higgs_audio_tts/ar.cpp`): `HiggsGenerator`'s prefill graph is
torn down and rebuilt on *every single* `generate()` call (its shape
depends on `prompt_steps`, i.e. the input text's own length, so it's
essentially never the same shape twice - `generator.cpp` unconditionally
does `prefill_graph_.reset(); prefill_graph_ = make_unique<...>(); ...run();
prefill_graph_.reset();` every call), and its decode graph/KV cache rebuild
whenever the bucketed cache size changes or grows mid-generation on a long
paragraph. All five of the graph-owning classes' destructors in that file
(`HiggsQwen3DecodeGraph::Impl`, `HiggsQwen3PrefillGraph::Impl`, and the
long-reference-prefill-only `EmbeddingGraph`/`LayerGraph`/`FinalGraph`) call
`engine::core::release_backend_graph_resources(runtime->backend(), graph)` -
the **2-argument overload**, which defaults its `evict_cuda_graph_cache`
parameter to `false` (see that function's own doc comment in
`src/framework/core/backend.cpp`: deliberate, for families that rebuild
*same-shape* graphs between requests, so they can keep a warm CUDA/HIP-graph
cache). Higgs's graphs are essentially never same-shape, yet every one of
its own call sites was left on that `false` default - unlike ~56 other call
sites elsewhere in audio.cpp (including the shared `greedy_qwen_decoder.cpp`
most *other* families reuse for their own KV-cache decode loop instead of
hand-rolling one the way Higgs does), which already pass `true` for exactly
this reason. Net effect: since `QWEN_TTS_AUDIOCPP_BACKEND=hip` here and this
box's audio.cpp build has `GGML_HIP_GRAPHS=ON`, every paragraph rendered
through Higgs (formerly this app's default clone model) leaves one more captured
HIP-graph-cache entry permanently resident, never evicted.

**Patched, but only in this box's local checkout, deliberately left
uncommitted there**: adding `, true` as a third argument to all five call
sites above, in `/home/rhino/audio.cpp` (a separate git repo from this one -
`git diff` there shows the fix; it is *not* committed or upstreamed). An A/B
test (same `ttsworker` binary, swapping `LD_LIBRARY_PATH` between a
pre-fix and post-fix build of just `libaudiocpp.so`, 50 `/generate` calls
each against `audiocpp-higgs-4b` with varying-length text) measured
steady-state RSS growth dropping from ~1.37 MB/call (unpatched) to
~0.50 MB/call (patched) - roughly a 2.7x reduction, the right direction and
magnitude for a slow trickle that eventually trips `RSSLimitBytes` on a real
book, though the residual ~0.5MB/call wasn't run long enough (only 50 calls)
to be certain it's pure allocator noise rather than a smaller remaining
leak. **Any fresh clone or reset of the `/home/rhino/audio.cpp` checkout
loses this patch** - reapply the `, true` change to all five call sites in
`src/models/higgs_audio_tts/ar.cpp` before trusting a rebuilt
`libaudiocpp.so` not to leak again.

**A second, distinct failure mode, also patched only in the same local
checkout**: Higgs occasionally fails to reach EOC and grinds on for far
longer than the text warrants - observed live (repeated `/generate` calls
against real book paragraphs, `audiocpp-higgs-4b`, this box's real GPU) as
single calls taking 58-108s against a ~1-4s norm, one of them producing
~194s of audio for a 26-word sentence, another actually exhausting
`max_tokens` (8192) outright. Each such call jumped RSS by hundreds of MB
to +3.3GB in one shot - bursty, not the steady per-call trickle described
above - and since `Worker.runMu` serializes every `Generate`/`Design`/
`Align` call in the process against this box's one GPU, a single stuck call
blocks every other request queued behind it for its entire duration, not
just its own caller's. Also measured live: unloading the stuck session
afterward (`POST /unload`, the same path `IdleUnloadAfter`/
`evictCloneModel` take) recovered only ~1% of the accumulated RSS
(32MB of 2.68GB), and reloading afterward landed *above* the pre-unload
level, not below it - only a full `ttsworker` process restart actually
returns RSS to baseline (confirmed: matched the fresh-process baseline
exactly). So periodic session-level unload/reload is not a viable
mitigation for either failure mode - the RSS-threshold watchdog restart
below is, and stays load-bearing regardless of either patch.

**The fix**: `HiggsGenerator::generate()`'s AR decode loop
(`src/models/higgs_audio_tts/generator.cpp`) now bails out early - through
the exact same `"...before EOC for this text chunk"` error message/suffix
`max_tokens` exhaustion already throws, so callers that pattern-match on it
(this app's own `Worker.Generate`/`isMaxTokensOverflow`, see
`internal/audioworker/generate.go`) need no changes - once
`result.delayed_frames` crosses a per-call budget derived from *that call's
own reference clip*: its codec frames per character of its own transcript,
projected onto the input text's length, times a multiplier. Exposed as the
`higgs_audio_tts.decode_frame_budget_multiplier` session option
(`session.h`/`session.cpp`; default 4.0, floored at 128 frames, capped at
`max_tokens`, `<= 0` disables it entirely) and threaded through
`HiggsGenerator`'s constructor (`generator.h`). Deliberately reference-pace-
derived rather than a flat wall-clock or token-count number: a voice cloned
from a slow, deliberate reference delivery legitimately needs more frames
per word than a brisk one does (see `higgsMaxTokens`' own comment below for
the confirmed-live case that still overflowed even 8192), so a single flat
bound either has to be loose enough for the app's slowest voice - too loose
to catch a short paragraph's runaway quickly for every other voice - or
risks false-failing that voice's own legitimately long output. This app's
own Go side no longer threads a multiplier through for this - dropped
after the real, observed OOM failures (see `voicerefs.maxRefClipSeconds`/
`speakerattr.maxRefLineWords`) turned out to be an oversized reference
clip inflating the *prefill* graph, not a runaway *decode* loop this
option guards against; audio.cpp's own compiled-in default (`4.0`) still
applies unconfigured. **Also only in the local checkout, uncommitted** -
touches `include/engine/models/
higgs_audio_tts/generator.h`, `src/models/higgs_audio_tts/generator.cpp`,
`include/engine/models/higgs_audio_tts/session.h`, and `src/models/
higgs_audio_tts/session.cpp`; a fresh clone/reset of `/home/rhino/audio.cpp`
loses this too, same as the `evict_cuda_graph_cache` patch above - both
need reapplying together before trusting a rebuilt `libaudiocpp.so`.
(Both were once found sitting only in `git stash` there, with a clean
working tree - check `git diff --stat` in `/home/rhino/audio.cpp` before
rebuilding; a full copy of every local patch is kept at
`/home/rhino/audio.cpp/lectable-local-patches.diff`.)

**Patch base.** Rebased 2026-10-06 onto upstream `6fa5a3c8` (61 commits
past `ed96b730`): `main` there sits at that commit with the patch
uncommitted, and the same patch is also committed as branch
`lectable-patches`. Earlier bases are kept: `ed96b730` (2026-09-29) as tag
`lectable-patches-ed96b730`, `lectable-local-patches.ed96b730.diff` and the
built `build-rocm7-bin.ed96b730/`; `e3de8e3f` as tag
`lectable-patches-e3de8e3` and `lectable-local-patches.e3de8e3.diff`. The
library is built in `build-rocm7-next/` and its `bin/` copied over
`build-rocm7/bin/`, so the live path never holds a half-built library.

What the 2026-10-06 rebase changed in the patch:
- Our `StableAudioSameRuntime::encode` short-window fix is dropped;
  upstream fixed it (#815).
- Upstream added a `TurnDetection` task; `Codec` now follows it in the
  enum and vocabulary.
- BreezeTTS's default `graph_arena_mb`/`weight_context_mb` fell from
  1024/2048 to 32/4 (#821); the codec session's shared helpers follow.
- Upstream renamed several options, keeping the old names as aliases
  (`duration_seconds` -> `duration_sec`, `inpaint_mask_*_seconds` ->
  `..._sec`, PocketTTS `language`/`offline_graph_capacity_mode` ->
  `pocket_tts.*`). `audioworker` still sends the old names.
- Higgs is untouched upstream. `include/audiocpp.h` and the C API are
  too, so a `ttsworker` built against the previous library loads this one.

Every model-gated `audiocpp-go` test passes on HIP against it.

From the 2026-09-29 rebase: upstream renamed `HiggsAR*` to
`HiggsQwen3*` (our own `HiggsARBatchedDecodeGraph` kept its name) and
`modules::QwenDecoderLayer*` to `modules::DecoderLayer*`. Upstream's
gallocr prefill (#704/#722, on for HIP) is kept: seeded output is
byte-identical, and peak VRAM on a reference prefill drops ~2 GiB.
Upstream's CUDA-only spare KV cache (#669) was dropped in favor of our
`kv_cache_slots_`. Its `lazy_import_scratch` is kept, and now also on for
HIP.

**Higgs throughput patches, same local checkout, same caveats.** Profiled
2026-09-24 against 37-120 real paragraphs (book "Spire's Spite" ch. 41,
Q8, RX 7900 XTX): single-stream RTF 0.34 -> 0.28, and 0.30 -> 0.108
throughput RTF at batch 4 (0.094 at batch 8, the default). Each piece, all in
`src/models/higgs_audio_tts/` plus their headers:
- **Reference-prefix KV snapshots** (`higgs_audio_tts.reference_kv_slots`,
  `generator.cpp`/`ar.cpp` `HiggsQwen3KVPrefixSnapshot`): an LRU of
  device-resident copies of each reference clip's prompt-prefix KV rows,
  restored on-device (~0.7ms) instead of re-prefilling the whole reference
  (~400ms for a 20s clip). Upstream only reused the *last* voice's prefix,
  and dialogue switches voice on ~75% of paragraph transitions. A restored
  prefix + text-only prefill is not bit-identical to one full prefill
  (first-step logits differ ~0.1-0.2%, same as upstream's own prefix reuse),
  so seeded output differs between the two paths.
- **KV cache churn**: `HiggsQwen3KVCache::reset()` is now logical (it used to
  re-upload the whole cache as zeros from the host, ~120ms per call); the
  buffer is zeroed on-device once at allocation instead, since masked rows
  must never hold NaN. Buckets are powers of two, one resident cache +
  decode graph per bucket (`kv_cache_slots_`), and a mid-decode grow copies
  on-device into the next bucket's resident slot instead of a host round
  trip + rebuild.
- **Pinned logits read-back** in both decode graphs: 1.26 -> 1.06ms per
  frame. The rest is per-sync latency (HIP under WSL), not the copy.
- **Continuous batched decode** (`higgs_audio_tts.decode_batch_size`,
  `HiggsARBatchedDecodeGraph` + `HiggsTTSSession::batch_loop`): with it >1,
  `run()` may be called concurrently on one session; one scheduler thread
  owns all GPU work (reference encode, single-sequence prefill, then a
  per-slot KV copy into a `[batch, 2048, kv_heads, dim]` cache, batched
  decode steps, codec decode as each chunk hits EOC). Decode is
  bandwidth-bound, so a batch-4 step costs about the same as batch 1.
  `generate()` itself was split into `begin_sequence`/`accept_logits`/
  `finish_sequence` (bit-identical single-sequence output, verified). With
  batching, k-quants lose their single-stream edge (Q5_K batch 8: 0.123 vs
  Q8's 0.094), so Q8 stays the model.
- **Frame-budget floor** (`kMinimumFrameBudget` = 250 frames) - see
  `audioworker.higgsFrameBudgetMultiplier`.

**Latents/codec patch, same local checkout, same caveats** (2026-09-26;
this change alone is in `/home/rhino/audio.cpp/lectable-latents-codec.diff`,
and it's in the full `lectable-local-patches.diff` too). Groundwork for
storing music seeds as Stable Audio latents instead of WAV (see
`lac/latent-storage-report.md`). It adds no C functions: latents ride the
existing artifact API.
- **`AUDIOCPP_ARTIFACT_LATENTS`**: appended to the artifact enum.
  - Payload: f32, time-major `[frames][dim]`.
  - Meta: `frames`/`dim`/`hop_samples`/`sample_rate`/`family`, plus family
    extras.
  - The shared helper is `engine/framework/runtime/latents.h`.
- **A new framework task `codec`** (one row in `task_vocabulary.cpp`):
  - audio in → latents artifact out (encode);
  - latents artifact in → audio out (decode);
  - it loads only the family's autoencoder.

  Stable Audio 3 implements it (`StableAudioCodecSession`: SAME weights
  only, no T5/DiT).
- **Stable Audio `gen` changes:**
  - Request option `return_latents=true` attaches each batch item's
    full-window latents. The meta carries `decode_seed`/
    `decode_rng_offset`/`chunked_decode`/`valid_frames`/
    `duration_samples`, so a `codec` decode reproduces the generated audio
    bit for bit on the same backend (batch 1; verified on HIP).
  - A latents input artifact replaces the audio input for
    `init_audio`/`inpaint_audio`, skipping the SAME encode.
- **Higgs** (discrete codes, via the existing `ACOUSTIC_TOKENS` kind, same
  conventions; helpers `runtime::Codes` in `latents.h`):
  - A `codec` session (`HiggsCodecSession`) loads only the codec, not the
    AR model.
  - `return_latents=true` on TTS attaches the generated codes (`"codes"`,
    with per-text-chunk `chunk_frames` meta) on both the single-stream and
    batched (`decode_batch_size>1`) paths.
  - `return_reference_codes=true` attaches the cloning reference's codes.
    A `"reference_codes"` input artifact then replaces reference audio
    entirely, skipping `encode_reference`. It hits the same
    reference-prefix KV snapshots, which key on the codes' content.
- **PocketTTS** (continuous, `LATENTS`):
  - A `codec` session (`PocketTTSCodecSession`) loads only the Mimi
    encoder and decoder (`load_pocket_tts_backend_weights(...,
    include_flow=false)`).
  - `return_latents=true` attaches the normalized latents with
    `chunk_frames`.
  - The encode path is Mimi's encoder stopped before `speaker_proj`
    (`encode_prompt_embedding(..., project_to_flow=false)`), normalized
    with `emb_mean`/`emb_std`. Encoding generated audio lands at cosine
    0.986 to the generator's own latents: the same space.
  - `resolve_pocket_tts_graph_capacity_config` is now a free function, so
    codec decodes use generation's exact decoder settings.
  - `return_reference_latents=true` attaches a clone reference's
    normalized Mimi latents (`"reference_latents"`). Passed back as an
    input artifact, they replace reference audio, skipping the Mimi
    encoder (`VoiceConditioner::prepare` -> `MimiEncoder::project_latents`,
    just `speaker_proj`). The audio path goes through the same
    normalize/denormalize round trip, so both condition bit-identically
    (`TestPocketTTSReferenceLatents`, CPU and HIP). A 10 s reference:
    ~180 -> ~95 ms on HIP, ~1.5 -> ~0.35 s on CPU (the flow LM prefill
    remains). Two framework changes carry this: latents artifacts take an
    id (`make_latents_artifact(l, id)`, `find_latents_input(artifacts,
    id)`), and `SessionPreparationRequest.input_artifacts` (PocketTTS
    conditions in `prepare()`, which the C API calls before every run).
    The session voice-state LRU keeps the latents, so a cache hit can
    still return them.
    (This change and the BreezeTTS one below alone:
    `/home/rhino/audio.cpp/lectable-reference-encodings.diff`, on top of
    the rest; also in the full `lectable-local-patches.diff`.)
- **BreezeTTS** (discrete codes, like Higgs): `return_reference_codes=true`
  attaches the cloning reference's speech codes (16 x 2048-entry
  codebooks, 12.5 fps), and a `"reference_codes"` input replaces reference
  audio (`BreezeTTSSession::request_reference_codes`). The GGUF's embedded
  spec predates the option, so `validate_request_options` strips it before
  contract validation (like `stream_*`).
  - `return_latents=true` attaches the generated codes (`"codes"`, with
    per-text-chunk `chunk_frames` meta; `BreezeGeneratorRuntime::generate`
    hands them out via `codes_out`).
  - A `codec` session (`BreezeTTSCodecSession`) loads only the speech
    encoder/decoder, each on first use. It builds them from the same
    `weight_type`/`graph_arena_mb`/`weight_context_mb`/`attention`
    session options as generation, and clamps to [-1, 1] after decoding
    as generation does, so a decode matches generation bit for bit
    (`TestBreezeTTSCodec`, HIP).
  - The GGUF's embedded spec takes precedence over `model_specs/` and
    lacks the `codec` task, so the loader adds it (and the `return_*`
    option keys) through a new `SpecBackedVoiceModelConfig::
    extend_contract` hook (`spec_backed_model.h`).
  - This part alone: `/home/rhino/audio.cpp/lectable-breeze-latents.diff`,
    on top of the rest; also in the full `lectable-local-patches.diff`.
    The live `libaudiocpp.so` is rebuilt with it (2026-09-26); the build
    before it is kept as `bin/libaudiocpp.so.0.1.0.pre-breeze-latents`.
- **Exact decode for every latents family:** decoding returned latents/codes
  reproduces the generated audio bit for bit on HIP (`audiocpp-go` tests).
- **An upstream bug, since fixed there** (#815, so no longer in the
  patch): `StableAudioSameRuntime::encode` wrote before its output buffer
  (heap corruption) for any window under 128 latent frames (~11.9 s). A
  short init/inpaint request, or a short codec encode, hit it.
- **Status (2026-09-26):** the live `libaudiocpp.so` is rebuilt with this
  patch; the pre-patch build is kept as `bin/libaudiocpp.so.0.1.0.prev`.
