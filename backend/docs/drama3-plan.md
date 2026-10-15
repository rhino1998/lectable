# Fish Audio Drama 3: audio.cpp port and listening experiment

Plan written 2026-10-14. On the same day the user decided there would be
**no Python reference runs**: Drama 3 is built straight into audio.cpp,
and the listening experiment runs on that port, locally, with the backend
stopped. The port is Phase 1; the experiment that decides whether lectable
adopts it is Phase 2.

**Why:** much more expressive emotion control. Drama 3 clones a voice from
a reference clip and takes a free-form direction for each line ("dry,
barely holding back a laugh") plus in-line tags (`[sighs]`, `[laughs]`)
that can shift mid-sentence. Today a line gets one label from a fixed set
of 13, and the delivery comes from a BreezeTTS-rendered variant clip per
voice (see "Emotions" in `speakerattr.md`).

**The risk Phase 2 tests:** Higgs also had inline emotion tags, and they
lost the listening test because they pulled the voice off its reference.
If a directed Drama 3 line doesn't stay on the cloned voice, lectable
doesn't adopt it (Phase 3).

**Checking the port without Fish's Python:**

- *Backbone:* llama.cpp already runs the `qwen35moe` architecture. Convert
  the text backbone to a llama.cpp GGUF and compare hidden states and
  logits on the same prompt. That's an independent check on this machine.
- *Tower, prompt and sampling:* there's no reference output, so these are
  checked by reading Fish's code line by line and by the audio itself:
  - intelligible speech that stops at eos
  - Parakeet WER against the input text
  - the reference voice is kept
  - a greedy run is stable

  A garbled result with a correct backbone points at the tower.

## What's already known (verified 2026-10-14)

- Weights are at `fishaudio/drama-3` on Hugging Face: not gated, ~40 GB,
  8 safetensors shards plus `codec.pth`. Fish's own code is in fish-speech
  `v3.0.0-beta`, `fish_speech/models/text2semantic/fish_omni/`
  (`backbone.py`, `tower.py`, `generate.py`, `chunking.py`, `moe.py`,
  `layers.py`, `fp8_moe.py`). Their own serving runtime is SGLang-Omni
  (`sgl-omni serve --model-path fishaudio/drama-3`).
- The license is the Fish Audio Community License v1.0. Personal or
  non-commercial use is free; a hosted service needs a separate license.
- **Codec:** `codec.pth` has the same sha256 as `fishaudio/s2-pro`'s
  (`74fc41c5…`). audio.cpp's existing `fish_audio` family already decodes
  it and encodes reference clips with it, at 44.1 kHz, ~21.5 frames/s,
  10 codebooks of 4096 codes each.
- **Backbone:** the Qwen3.5-35B-A3B architecture.
  - 40 layers: 3 of every 4 are GatedDeltaNet (linear attention), every
    4th is gated full attention.
  - 256 experts with top-8 routing (`norm_topk_prob`), plus a shared
    expert with a sigmoid gate; `moe_intermediate_size` is 512.
  - hidden 2048, 16 query heads, 2 KV heads, head_dim 256, zero-centred
    RMSNorm, QK norm.
  - Plain 1-D neox RoPE (no mrope) with `partial_rotary_factor` 0.25 and
    theta 1e7.
  - vocab 252288: Qwen's plus `<|semantic:0..4095|>` at
    248099..252194. eos `<|im_end|>` is 248046.
  - Experts are FP8 e4m3 with 128x128 block scales (`weight_scale_inv`);
    everything else is bf16.
- **Audio input to the backbone:** an audio row's embedding is
  `(embed(main token) + codebook_embeddings[code 1]) / sqrt(2)`
  (`vq_backbone_codebooks` = 1, `tower.py:embed_backbone`). Text rows use
  the plain token embedding.
- **Tower** (`audio_decoder_config`, `decoder_type` `codebook_mtp_shared`):
  - 4 shared layers plus 9 residual layers, each a dim-2048 transformer
    block (16 heads, 2 KV heads, head_dim 256, interleaved partial RoPE
    0.25, dense FFN 6144) with its own **KV cache over time**. This is
    unlike S2's Fast AR, which runs within one frame.
  - Each layer mixes a code embedding into the hidden state:
    `eh_proj(cat(enorm(e), hnorm(h)))`, then the block, then
    `final_layernorm`.
  - The first shared layer also adds the summed embeddings of the codes
    the backbone didn't see (2..9).
  - There are 9 heads, one per codebook 1..9.
  - The prompt is teacher-forced through the tower too (`prefill`).
- **Sampling** (`tower.py:decode_step`):
  - The main token is drawn only from eos plus the semantic band, with
    top-k/top-p.
  - Repetition-aware resample: if the draw repeats one of the last 16
    main tokens, redraw it at temperature 1.0, top-k 30, top-p 0.95.
  - Defaults are temperature 0.7, top-p 0.7, top-k 30.
- **Prompt** (`generate.py:build_drama3_prompt`):
  - System turn: `convert the provided text to speech reference to the
    following:`, then per reference `\n\n[Reference N]\nText:
    <|speaker:0|> <transcript>\nSpeech: ` followed by its code rows.
  - Then optional history turns, then the user turn (text with a speaker
    tag), then the open assistant voice turn.
  - Each literal is tokenized separately.
  - Long text is chunked at ~300 UTF-8 bytes, with at most 3 speakers per
    chunk (`chunking.py`).
- **audio.cpp today:**
  - `framework/modules/transformers/qwen35_decoder_runtime.cpp` has the
    dense hybrid layers (`ggml_gated_delta_net`, `ggml_ssm_conv`).
  - No MoE anywhere: nothing uses `ggml_mul_mat_id`.
  - `audiocpp_gguf` reads safetensors and `.pth` but has no FP8.
  - Upstream (0xShug0/audio.cpp) had no Drama 3 work on 2026-10-14. Check
    again first: a port may have landed since.
- **VRAM on the 7900 XTX (24.5 GB):** about 22 GB in total at ~4-bit
  (backbone ~19-20 GB). Benchmarks and production use both have the card
  to themselves: stop the backend for benchmarks, and in production
  `internal/jobs` runs one pool at a time, unloading Higgs or the LLM on a
  switch. What's left to watch is the ~20 GB load on each TTS<->LLM switch
  and the room for batching.

## Phase 0: preparation

1. Weight download: started 2026-10-14 into
   `~/audio.cpp/models/drama-3-src`, with resumable curl per file (no `hf`
   CLI on this box). Check that all 8 shards and `codec.pth` are present
   and the right size; rerun the same curl `-C -` loop to finish any that
   aren't. There is 465 GB free on `/`.
2. Fetch the fish-speech `v3.0.0-beta` sources listed above into the
   session's scratchpad. They are reference reading only: never run them
   from that directory.
3. **Build the test set** from the live library (the `lectable` MCP's
   `sql`/`speakers`/`read` tools):
   - 6 characters with base reference clips (`voice-refs/<presetID>.wav`
     plus the preset's reference transcript): men, women, a child or
     teen, and one character with an accent.
   - For each, 4 dialogue lines spread across the strongest labels
     (whisper, shout, sad, afraid, angry, teasing, pleading), each with
     its current `paragraph_audio` clip (Higgs from a Breeze variant).
     Those clips already exist, so condition A costs nothing.
   - 4 lines whose delivery changes mid-line ("He laughed. 'No,' he
     said, his voice breaking."), 4 neutral dialogue lines as controls,
     and 2 narration paragraphs in the narrator's voice.
   - For each line, write a short prose direction plus any in-line tags,
     from the surrounding paragraphs.
   - Save it as `testset.json` (line text, speaker, clip paths, current
     emotion, direction, tags) together with the audio, in a dated
     directory under the scratchpad, and copy it to
     `~/drama3-eval/` so it outlives the session.

## Phase 1: the audio.cpp port (several sessions)

Work in `~/audio.cpp` on top of the local lectable patches. First consider
rebasing onto upstream, which was 10+ commits ahead on 2026-10-14. Follow
`audiocpp-patches.md` and the [[rocm7-toolchain]] notes: build into a
fresh dir, and swap the build in by rename. Save the result as
`lectable-drama3.diff` beside the other diffs.

1. **Weight prep.**
   - Write a Python script (run with `-I`) that dequantizes the FP8
     experts shard by shard: e4m3 times the 128x128 `weight_scale_inv`
     block scale, to bf16.
   - Stack each layer's 256 experts into 3-D `[n_expert, out, in]`
     tensors (gate, up, down; or gate+up fused), which `ggml_mul_mat_id`
     needs: 30,720 tensors become 120.
   - Write bf16 safetensors (~70 GB intermediate; delete it after
     conversion).
   - Then run `audiocpp_gguf --family fish_drama3 --type q4_k` with
     `--keep-type` for the embeddings, the router gates, the
     shared-expert gate, the tower, and the norms at q8_0/orig.
   - The S2 recipe's lesson (`docs/proposals/fish_audio_quantization_recipe.md`):
     keep-type patterns need the `model_weights/` prefix, or nothing
     matches and the tower gets crushed silently.
   - Quantization-type options for the experts are in "Open questions"
     below.
2. **MoE in `qwen35_decoder_runtime`.**
   - Router logits, then softmax, top-8, and renormalization; then
     `ggml_mul_mat_id` for gate/up/down with SiLU-GLU; then a weighted
     sum; plus the shared expert times `sigmoid(shared_expert_gate · x)`.
   - Reference: llama.cpp's `build_moe_ffn` (`src/llama-graph.cpp`) and
     its `qwen35moe` model graph in `~/llama.cpp-latest`.
   - Add partial neox RoPE at 0.25 with theta 1e7 if the runtime lacks it.
   - **Check it independently on this machine:** convert the text
     backbone alone (the `model.language_model.*` tensors plus `lm_head`,
     under a Qwen3.5-MoE config) with llama.cpp's
     `convert_hf_to_gguf.py`. Run the same text-only prompt through both,
     and compare the final hidden states and the top-k logits.
3. **New family `fish_drama3`** (`src/models/fish_drama3`,
   `include/engine/models/fish_drama3`, `model_specs/fish_drama3.json`).
   - Reuse `fish_audio`'s codec, reference encoding, text tokenizer
     wrapper, and HIP sampler (`hip_fast_sampler.cu`).
   - New code: the prompt builder (port `build_drama3_prompt`
     exactly, including separate tokenization per literal); backbone
     input mixing; the 13-layer tower with KV caches, prefill and decode
     step; RAS sampling; chunking with history carried across chunks
     (`previous_takes`); and request options (temperature, top-p, top-k,
     max tokens, seed).
   - Check: the audio checks listed at the top. Codes to wav already
     works, since the codec is S2's. To isolate the codec path, encode a
     reference clip and decode it straight back.
4. **Performance on the 7900 XTX.**
   - The backbone ceiling is llama.cpp's speed on Qwen3.6-35B-A3B
     (~74 tok/s single stream).
   - Target: RTF ≤ 0.5 (21.5 frames/s is real time). Profile the tower
     and sampling if it falls short, keep sampling on the GPU, and use
     one graph per step.
   - Measure peak VRAM, including a 2-reference prompt and a 300-byte
     chunk.
5. **CLI smoke test**, then `audiocpp-go` if anything beyond existing
   request options is needed. Directions and tags travel in the text.

## Phase 2: listening experiment (decides Phase 3)

Run on the port, locally, with the backend stopped. The test set comes from Phase 0.

**Conditions**, run for every test line:

| | Model | Reference clip | Direction |
|---|---|---|---|
| A | Higgs (today's clip) | Breeze emotion variant | none (from the variant) |
| B | Drama 3 | base clip | none |
| C | Drama 3 | base clip | prose direction |
| D | Drama 3 | base clip | direction plus in-line tags |
| E | Drama 3 | Breeze emotion variant | none (drop-in for today's emotion system) |

Use 2 seeds each for B-E.

**Measurements:**

- A **blind listening page**: shuffled conditions per line, with
  on-voice (1-5) and expressiveness (1-5) ratings and a free-text note.
  Publish it as an Artifact that keeps the ratings, using the database
  capability.
- WER through the worker's Parakeet `/transcribe` (`internal/audioworker`),
  for mispronunciations, skipped words, and tags spoken aloud.
- Loudness against the base clip, the old variant problem: is a whisper
  still audible?
- Speaker-embedding similarity as a secondary signal only.
  `speakerattr.md` "Age stages" notes it ranked things wrongly for age,
  so ears decide.

**Gate to Phase 3:** C or D beats A clearly on expressiveness, is no worse
on-voice, and has comparable WER. If only E wins, Drama 3 is still worth
porting as a plain clone model, but Phase 3's emotion rework shrinks to
nothing. Write the results into this file.

## Phase 3: lectable integration

- **Clone model:**
  - Add `audiocpp-drama3` to `voices.CloneModels`.
  - Map the family and model path in `internal/audioworker`, with an env
    var `LECTABLE_AUDIOCPP_DRAMA3_MODEL_PATH`.
  - Add a residency kind of its own, and keep generation-pool concurrency
    within what VRAM allows.
  - Measure the TTS<->LLM swap time with a 20 GB load.
- **Emotion:**
  - Under Drama 3, `jobs.Manager.emotionVariant` returns the base clip,
    so no variant clips are made.
  - The line's direction goes into the text sent for generation.
  - Add a stored free-form direction per paragraph next to the label.
  - The emotion pass (`httpapi.directChapter`) asks for a short prose
    direction plus optional tags. Keep the 13 labels as presets, and as
    the fallback for other clone models.
  - The direction becomes part of the clip's content key
    (`internal/clipkey`).
  - The reader's per-line override becomes a text field on web and
    Android. Run `make -C backend generate` for the wire types.
  - Narration stays neutral (`EffectiveEmotion`) unless Phase 2 suggests
    otherwise.
- **Docs and checks:**
  - Update the root `CLAUDE.md` (the clone-model and emotion paragraphs),
    `backend/CLAUDE.md`, `speakerattr.md` "Emotions", `api.md`, and
    `audiocpp-patches.md`.
  - Run `make check` and `npx tsc -b --noEmit` in `frontend`, then deploy
    with `make deploy`.

## Open questions

- **Quantization type for the experts:**
  - q4_k first.
  - q3_k (~15 GB) if VRAM headroom matters; check by ear.
  - IQ4_XS, which the speaker LLM uses, needs IQ types added to
    `audiocpp_gguf` and an imatrix, so it's a later step.
- **Contributing upstream:** whether to offer the port to
  0xShug0/audio.cpp. The license allows non-commercial distribution of
  derivatives.

## Progress (2026-10-14)

**The port runs.** A plain-TTS sentence transcribed word-perfect on its
first real run, so the backbone, audio decoder, prompt and sampling are
right without a Python reference.

Everything is in `~/audio.cpp`, uncommitted, on top of the lectable
patches.

**Weights:**

- `tools/models/fish_drama3/prepare_fish_drama3.py` (numpy only, about
  10 minutes) writes `models/drama-3-prep` (~70 GB bf16):
  - the FP8 experts, dequantized and stacked into 3-D per layer
  - `lm_head_audio` (eos plus the 4096 semantic rows only)
  - the codec, with weight norm fused and `wqkv` split
- `src/framework/assets/tensor_source.cpp`: `audiocpp_gguf` now quantizes
  3-D `.experts.` tensors row-wise.
- `models/Fish-Drama-3-GGUF/fish-drama3-q4_k.gguf` is 22.4 GB:
  - experts at Q4_K, listed by exact name (`--keep-type` matches only
    exact names or prefixes)
  - everything else at Q8_0, `embed_tokens` included
  - lookup tables at F16
  - The conversion command is in the session scratchpad's `convert.sh`;
    it is one `--keep-type ...=q4_k` per expert tensor, with base
    `--type q8_0`.

**Runtime:**

- `qwen35_decoder_runtime`: optional MoE, following llama.cpp's
  `build_moe_ffn` (`num_experts` and related fields), plus
  `load_lm_head=false`.
- The new family `fish_drama3`:
  - `assets.cpp` parses config.json and checks the layer pattern.
  - `tower.cpp` holds the 13-layer audio decoder, with per-layer KV
    caches, a teacher-forced prefill, and single-row decode graphs that
    chain the hidden state on the GPU.
  - `generator.cpp` has the prompt builder (Fish's format, literal for
    literal), Fish's top-k/top-p sampler and repetition resample, and the
    loop.
  - `session.cpp` and `loader.cpp` mirror S2's.
  - `model_specs/fish_drama3.json`.
- `fish_dac_codec_runtime.cpp` gained one log line (the decode graph
  buffer size).

**Build:** `build-rocm7-drama3`, which is never the live `build-rocm7`:
`AUDIOCPP_MODEL_SET=custom`, `AUDIOCPP_MODELS=fish_audio,fish_drama3`.
It builds in seconds.

**Measured on the 7900 XTX (queue paused with `?drain=1`):**

- Load: 23 s. The GPU holds 19.3 GiB of backbone, 1.4 GiB of audio
  decoder and 0.6 GiB of codec.
- Generation: 38 frames/s (real time is 21.5), so RTF ~0.56 before any
  optimization.
- Prefill: 36 rows in 0.33 s.
- **Codec:** whole-clip decode left only ~0.6 GB of VRAM free and
  oversubscribed it (18-71 s per clip, paged by WSL). It now decodes
  frame by frame through the causal streaming decoder: about 1.3 s per
  5 s clip, matching the whole-clip output at correlation 0.99999.

**First clone and direction test** (clips in `~/drama3-eval/first-clips`, references
from lectable's voice-refs):

- Five lines in Erin Solstice's and Jack's (Cat's Cradle) voices, all
  transcribed word-perfect with no tag read aloud:
  - `[whispering, frightened]` came out at RMS 0.026, against 0.134
    neutral.
  - `[laughing] ... [sighs]` produced a real laugh (Whisper heard it as
    "Ha ha ha ha!").
  - Every line stopped at eos after 71-127 frames.
- Not yet listened to by a person.

**VRAM is tight:**

- With the Windows desktop (dwm) using little VRAM, free memory runs:
  23.3 GiB at start, 3.5 after the backbone, 1.9 after the audio decoder,
  1.25 after the codec.
- The reference encode graph (877 MiB for the ~10 s Erin clip) leaves
  ~370 MiB.
- When dwm had leaked VRAM, the encode spilled into WSL-paged memory and
  took 39-327 s instead of 0.4 s. The whole-clip codec decode did the
  same before the streaming switch.
- **Before integration:**
  - Make room: move the 0.55 GiB `embed_tokens` lookup to the host, and/or
    lower the default `cache_steps` to 2048.
  - Bound the encoder: encode long references in chunks.
  - In the worker, the aligner and transcriber are *not*
    residency-gated, so either one resident would break Drama 3.

**Speed pass (2026-10-14 evening).** RTF (lines 2-5 of a five-line
Erin-voice set, single stream) went 0.79 -> 0.47:

- **Codec:** exact chunked decode (`FishDacCodecRuntime::decode_codes_chunked`).
  The quantizer and transformer half runs over the whole clip, then the
  causal conv decoder runs in 32-frame chunks with 8 frames of context:
  ~3x faster than frame by frame, with a fixed 330 MiB graph.
- **Audio decoder:** one fused graph per frame, sampling on the GPU
  (Gumbel-max). HIP's TOP_K/ARGSORT only handle rows of 1024 or fewer, so
  the top k is taken per 1024-code row first.
- **Delta-net head repeat:** `repeat_linear_attention_heads` used one
  slice+concat per head (~1900 kernels per step). It is now a single
  `ggml_repeat_4d`: backbone 17.8 -> 12.8 ms per frame.
- **Zero-centred norms:** 1+w is folded in at load
  (`zero_centered_norm_weight_from_source`).
- **IQ4_XS experts** (`fish-drama3-iq4xs.gguf`, 21.4 GB): 1 GB smaller and
  faster (backbone 10.8 ms per frame), all lines word-perfect.
  IQ4_XS+Q4_K on the attention/delta-net matrices **broke the output**
  (looping, dropped text): those stay Q8. Q6_K is untested.
  `audiocpp_gguf` learned `iq4_xs`.
- **Higgs baseline** (Q8_0, single stream, same lines): RTF 0.23.

**Batching (in progress).** Backbone step cost by tokens per step: 1 ->
11.95 ms, 2 -> 12.5, 4 -> 14.8, 8 -> 23.7, 16 -> 30.9. So batch 4 is
3.2x the throughput.

- `Qwen35DecoderRuntime::create_batched_decode_session`: per-slot KV,
  conv tails and delta-net states, `load_slot` from a single-sequence
  prefill.
- Bit-exact against the single path at batch 1. Batch 2+ differs by
  kernel rounding plus router near-ties on random inputs; this needs an
  audio check.
- Still to do: a batched audio decoder, and a continuous-batching session
  (a Higgs-style batch thread with `fish_drama3.decode_batch_size`).

**Batching and VRAM (2026-10-14 night):**

- **Continuous batching works with concurrent callers.**
  - Session option `fish_drama3.decode_batch_size` (2-32);
    `fish_drama3.decode_batch_cache_steps` sets rows per slot (default
    2048).
  - Each text chunk becomes a job for one batch thread, which alone
    touches the GPU.
  - Measured through the Go binding with 8 concurrent lines in Erin's
    voice (IQ4_XS): batch 4 gives effective RTF 0.28, against 0.49 with
    batch 1, all lines correct. Clips are in `~/drama3-eval/batch4`.
  - Every request in a batch must use the same `top_k`.
- **VRAM:**
  - The Qwen3.5 decode graphs (single and batched) now allocate their
    intermediates with gallocr. Caches, delta-net state and inputs sit in
    a separate persistent buffer.
  - The KV caches are f16, in the backbone and the tower, single and
    batched. Output is unchanged against f32 caches.
  - Setting up batching at 4 x 2048 dropped from 2.2 to 1.3 GB.
  - **The tower's graphs stay on static allocation:** under gallocr its
    output was corrupted (cause not found; the backbone is fine). It costs
    ~70 MiB.
- **Reference codes:**
  - A `reference_codes` input artifact (time-major, 10 codebooks) is used
    instead of encoding the reference audio, and `reference_text` is still
    required.
  - `return_reference_codes=1` returns the codes that were used.
  - This is the same contract as lectable's Higgs patch, so the worker can
    cache Drama 3 encodings per voice. Encoding stays on the GPU.
  - Later chunks of a request reuse the first chunk's encoding.
- **Still tight:**
  - With Windows using more VRAM, a first-time reference encode (877 MiB
    graph) still spilled into WSL paging (43 s).
  - Cached codes avoid it after the first use of a voice.
- **Requested by the direction-planning session, not done yet:** pin
  chunk 1 in history alongside the latest chunk, with Fish's candidate
  fallback (see `drama3-direction-plan.md` section 3).

**Peak VRAM pass:**

- The prefill graphs (backbone ~40 MiB, tower ~30 MiB) and the codec's
  decode graphs (330 MiB chunk graph) are now freed after use.
- Low point at batch 4 is ~0.95 GB free, during codec decode.
- The GPU reference encode graph is 877 MiB for 8 s of reference and
  1.2 GiB for 11 s. It spilled again (29 s) even with 1.8 GB reported
  free, so WSL's usable budget sits below the reported free memory.
- **CPU reference encoding was tried and removed.** The CPU codec path
  gives codes with 0% agreement with the GPU's on the same clip. Clones
  from those codes still produced the right words, but voices went off
  and sighs/laughs ran away. Encoding stays on the GPU.
- **Prompt-prefix cache** (`fish_drama3.prompt_cache_slots`, default 8):
  - The system turn, with its references, is snapshotted after its first
    prefill: backbone KV rows and delta-net tails/states, plus tower
    cache rows.
  - It is keyed on the exact rows plus the next row, and a hit restores
    it.
  - Prefill on a hit takes ~165-190 ms instead of ~300 ms.
  - This needed a continuing prefill (`prefill_embeddings` now starts at
    `valid_steps()`) in the backbone and the tower.
- **Clean comparison:** 12 lines in Erin's voice, GPU-encoded reference,
  prompt cache on, all 36 outputs correct.

  | Batch x slot rows | Effective RTF |
  |---|---|
  | 1 | 0.45 |
  | 4 x 2048 | 0.23 |
  | 8 x 1024 | 0.26 |

  12 lines don't keep 8 slots busy (the long lines dominate the tail),
  so batch 8 needs a bigger job to show its gain.

**Speed pass 2:**

- Native partial RoPE, `-exp(A_log)` precomputed with broadcast
  bias/decay, and the expert sum as one batched matmul: backbone 10.6 ->
  9.8 ms per frame, tower 5.3 -> 5.2.
- Prefill on a prompt-cache hit dropped from 168-202 ms to 80-112 ms:
  - delta-net state is zeroed on the device (it used to upload ~60 MB of
    zeros, twice per line);
  - backbone snapshots sit in pinned host memory (restore 38 -> 11 ms);
  - batched `load_slot` copies device to device.
- Batch mode: finished clips decode on a codec thread (the codec has its
  own backend; `codec_mutex_` serializes encode/decode).
- A 40-paragraph backlog (Wandering Inn chapter-3 paragraphs, Erin's
  voice) gave RTF 0.41 unbatched, 0.21 at batch 4 x 2048 and 0.18 at
  batch 8 x 1024. These figures include runaways (below).

**Runaways (all configurations, not batching):**

- Unbatched, 3 of 40 lines ran long. One looped ("Pawn. ..." became
  "Pawn Pawn Pawn" for 95 s). One stopped mid-text and emitted
  near-silence until the cap (33 s). One is unclear: line 27 transcribes
  in full, but at 32.8 s for 263 chars it may just be slow speech.
- Batch 4 and batch 8 also had 2-3 runaways each, partly the same lines.
- Not yet known: whether this is IQ4_XS, the port, or the model itself
  (no reference to compare).
- Candidate fixes:
  - cap frames by text length so a runaway costs seconds, not ~40 s of
    compute;
  - compare the runaway rate against Q4_K;
  - rely on lectable's completeness check to retry.

**Chunk history and style:**

- `fish_drama3.history_pinned` implements Fish's Drama3History: chunk 1
  pinned plus the latest chunk, with the candidates fallback when a
  prompt doesn't fit. **It defaults off.** By ear it added artifacts; it
  doesn't carry a style (in a 5-chunk "[whispering]" passage, chunk 2
  was back at -21.9 dBFS, pinned or not); and it costs ~390 ms of
  prefill per chunk from chunk 3 on, against ~297 ms with only the
  latest chunk.
- **Style carry:** a leading [style] that isn't a point tag is repeated
  on every chunk. The point-tag list in `session.cpp` `is_point_tag`
  mirrors lectable's direction.PointTags. Spans cut by a chunk boundary
  are closed and reopened. CPU-checked; the GPU check (do chunks 2-5
  stay near -35 dBFS?) is pending.

**Q4_0 experts (2026-10-15):** slower than IQ4_XS (backbone 10.6-10.7
vs 9.6-9.9 ms per frame; RTF 0.45 vs 0.42) and 1 GB bigger. Dropped and
the file deleted. IQ4_XS stays.

**Not done:**

- **Batch 8 correctness.** On the current tree batch 8 ran away on 8/80
  test lines (lines 9, 14, 20, 21, 26, 27) against 2-3/80 at batch 4, so
  lectable defaults to 4 (`LECTABLE_AUDIOCPP_DRAMA3_DECODE_BATCH`). Find
  out whether batch 8 is genuinely worse or that was noise: more seeds
  at 4 vs 8, and check that a slot's frames don't depend on what else
  is batched with it (`FISH_DRAMA3_TEST_BATCHED` at 8). It is the faster
  setting (RTF 0.14 vs 0.16) if it holds up.
- The llama.cpp backbone cross-check. Lower priority now that the audio
  is right.
- Performance work: on-device sampling, fewer graph launches per frame.
- `audiocpp-go` and lectable integration (Phase 3).
- A full-model-set build for the live worker.
- Streaming output.

**Expert imatrix (2026-10-15):** IQ4_XS without an imatrix ran away on
8/80 test lines against Q4_K's 2/80. Added an importance-matrix path for
the MoE experts:

- `fish_drama3.collect_expert_stats=<path>` (unbatched only) taps every
  MoE layer's input, activated intermediates and chosen experts on each
  decode step, and writes a "D3IM" file (per layer and expert: routed
  count, sums of squared gate/up inputs and down inputs) when the
  session closes.
- `audiocpp_gguf --imatrix <file>` quantizes each stacked expert tensor
  per expert with that expert's means (no imatrix for never-routed ones).
- Collected with Q4_K over 100 random library paragraphs, two voices
  (`~/drama3-eval/imatrix/`): 28,638 frames; 875 of 10,240 experts never
  routed. Output: `fish-drama3-iq4xs-im.gguf` (same size as plain IQ4_XS).
- Runaway result (severe = cap, recall < 0.6, or a word loop; 40 lines x
  seeds 11 and 1011, batch 4): plain IQ4_XS 8/80, Q4_K 2/80, imatrix
  IQ4_XS 1/80 (dev build) and 2-3/80 (current tree) - the same within
  noise. Batch 8 on the current tree: 8/80, so lectable defaults to 4.
  Tower embedding tables F16 vs Q8_0 (`FISH_DRAMA3_TOWER_EMBED_F16`):
  2/80 vs 3/80 - no difference, Q8_0 stays. Line 20 ("Pawn...") fails
  in most configurations.

**ggml upgrade (separate session, 2026-10-15):** llama.cpp's current ggml
merged into a copy of this tree gave no gain on identical Drama 3 code
(batch 4: ~1%, within noise; unbatched: slightly slower; VRAM: same;
prefill on a prompt-cache hit ~100 -> ~81 ms, 1-2% of a line).
Not merged. That session's gdb runs found a real race: the C API calls
`prepare()` before every run, which reassigned `Drama3Session::defaults_`
while other callers' `make_request` copied it (`basic_string::_M_create`
under concurrent batched callers). Now guarded by a mutex.
