# Report: storing model latents/codes instead of audio

Companion to `mastering-format-report.md` (lossless codecs). This one
covers keeping the generators' own pre-decode state (discrete codec codes
or continuous latents) in place of, or alongside, PCM. Everything was
measured on this box (RX 7900 XTX, HIP build of the local audio.cpp
checkout, 2026-09-26) using a patched *copy* of audio.cpp that dumps and
reloads codes/latents at the decoder and encoder boundaries
(`experiments/det-probe/decoder-probe.patch`). The live checkout was not
modified. Nothing here is implemented in lectable.

## TL;DR

| Use | Stored state | Size | Verdict |
|---|---|---|---|
| **Music seeds** as Stable Audio latents | 256-d latents @ 10.8 Hz, f16 | 5.1 GB → **~0.17 GB** | **Go** (blind seam A/B passed). Also removes a lossy round trip every continuation pays today. Needs an audio.cpp patch. |
| **Higgs reference-code cache** | 8 × 10-bit codes @ 25 Hz | ~5 KB per 20 s ref | Worthwhile, small. Skips a ~0.4 s cold encode after each worker restart; exact for Higgs cloning. |
| **Narration master** for Higgs books | same codes | ~66 MB for 73.5 h (vs ~6.3 GB FLAC) | Viable if a master is wanted. Reproduces within 1 LSB with f32 codec weights, not bit-exact. |
| Ambience loops as latents | — | — | No. They're consumed as audio (tiled and mixed in Go). |
| Voice refs as Breeze codes | — | — | No. Post-processed after decode, and other clone models need real audio. |
| Lossless archive via latents | — | — | No. Not bit-exact across backends or builds (below). |
| Merging music + ambience in latent space | — | — | No. It produces a blend, not a mix (below). |

## What each generator emits

| Audio | Generator | State before the decoder | Rate | % of 16-bit PCM |
|---|---|---|---|---|
| voice refs, emotion variants | BreezeTTS 2 (Mimi-style codec) | 16 codebooks × 2048 (11 bit) @ 12.5 Hz | 2.2 kbps | 0.57% |
| narration, Higgs books | Higgs Audio v3 | 8 codebooks × 1024 (10 bit) @ 25 Hz | 2.0 kbps | 0.52% |
| music seeds, ambience | Stable Audio 3 Medium (SAME autoencoder) | 256-d float latents @ 44100/4096 ≈ 10.8 Hz | 88 kbps f32, 44 kbps f16 | 6.3% / 3.1% |

All three pair an **encoder** with the decoder, and the encoder emits the
decoder's input format:

- Higgs `encode_reference` returns the same `{codes, frames, codebooks}`
  that `decode_codes` consumes, through the same quantizer weights
  (`codec.cpp:974-1012` vs `1061-1064`).
- Stable Audio's `same_->encode` produces latents in the DiT's space. It's
  used for `init_audio` and `inpaint_audio`.
- Breeze has a speech encoder for clone references.

## Decoder determinism

Same codes/latents, decoded repeatedly:

| Decoder | same GPU, same/new process | GPU vs CPU, shipped q8_0 | GPU vs CPU, f32 weights |
|---|---|---|---|
| Breeze | bit-identical | 42 dB SNR; 78% of samples differ; max 470 LSB | 86 dB; 8% differ; max 4 LSB |
| Higgs | bit-identical; CPU independent of thread count | 61 dB; 60% differ; max 48 LSB | **116 dB; 0.2% differ; max 1 LSB** |
| Stable Audio | bit-identical | 33 dB; 90% differ; max 396 LSB | 41 dB; 68% differ; max 167 LSB |
| PocketTTS (Mimi) | bit-identical | 44 dB; 87% differ; max 338 LSB | 104 dB; 0.9% differ; max 1 LSB |

Reading SNR: +6 dB ≈ one bit, or half the error amplitude. The served
48 kbps Opus narration is at 18.7 dB against its source. 16-bit rounding
sits ~79 dB below a typical −22 dBFS clip. Every row above is inaudible;
none is bit-exact across backends.

The divergence comes mostly from q8_0 weights: ggml quantizes activations
differently per backend. The rest is float reduction order and
approximations, which Stable Audio's decoder amplifies on its own. The
decoder noise Stable Audio injects is host-generated with the same RNG
policy on both backends, so it isn't the cause.

Speed from codes/latents:

| Decoder | GPU | CPU, 4 threads |
|---|---|---|
| Higgs | ~300x real time | ~8x |
| Breeze | 45x | 1.9x |
| Stable Audio | 45x | 1.1x |

For comparison, the lossless codecs run at 130-450x on one core.

audio.cpp has no decode-only entry point: decoding loads the whole model
(Higgs: 4.85 GB, of which the decode path is ~43 MB at f16 by layer
arithmetic).

## Autoencoders are not idempotent

latent A → decode → 16-bit PCM → encode → latent B:

| Model | A vs B | decode(A) vs decode(B) |
|---|---|---|
| Stable Audio | per-frame cosine median 0.987 (min 0.957); relative L2 error 17%. Changing the encoder's noise seed alone moves B by 4%. | waveform SNR 16.7 dB; log-spectra differ by 3.5 dB on average |
| Higgs | 47% of codes identical: 77% in codebook 0, down to 20-24% in the finest codebooks; 10% of frames match in all 8 | waveform SNR 12.5 dB |

So every decode → re-encode pass costs about one lossy-codec generation
(comparable to the 48 kbps Opus encode). Today's music pipeline pays it on
every continuation (below).

## Merging latents (music + ambience)

Tested with a 10 s piano latent and a 10 s rain latent (same length),
decoding merged latents and comparing them against the true audio mix
`decode(zM) + decode(zA)`:

| Merge | SNR vs audio mix | Energy explained as a·music + b·ambience | Level vs sources |
|---|---|---|---|
| zM + zA | 3.6 dB | 66% (a 0.43, b 0.39) | −2 to −4 dB |
| (zM + zA) / 2 | 6.4 dB | 78% (a 0.38, b 0.34) | −4 to −6 dB |
| (zM + zA) / √2 | 5.1 dB | 75% (a 0.41, b 0.37) | −3 to −5 dB |

A latent-space merge is a *blend*: both sources are attenuated, and
22-34% of the energy is neither source. By ear, the averaged merge
level-matched to the audio mix came surprisingly close but still wasn't
acceptable, so latent mixing is ruled out. The latent encodes "what the sound
is", not a waveform to superimpose. So rendering the two layers separately
and mixing in audio (`musicgen.MixAmbience`) remains correct. The
alternatives are:

- encoding the audio mix (lossy, per the table above);
- conditioning one render on the other (`init_audio`), which changes the
  music;
- a single combined prompt, already tried and dropped (`backend/CLAUDE.md`:
  SA3 tags training clips as music *or* SFX).

## Use 1: music seeds as Stable Audio latents (recommended)

### How seeds work today

1. When a region finishes, `jobs` writes the last 10 s of its music layer
   to `<regionID>.seed.wav` (`jobs/manager.go:4910`). It holds 1.76 MB of
   PCM; 3,101 seeds total 5.1 GB for Spire's Spite.
2. The next region reads it back (`manager.go:4807`), and
   `musicgen.generateContinuationChunk` sends it as base64 WAV with
   `inpaint_audio` and a mask over `[seed, seed + new)`.
3. The worker decodes the WAV, and audio.cpp runs `same_->encode` on it to
   get inpainting latents (`stable_audio/session.cpp:314`). That encode
   took 650 ms for a 20 s window in a cold process; warm cost not measured.
4. Inside a region, each chunk is seeded from the previous chunk's tail
   through the same WAV → encode round trip, in memory.

The seed is never played. It exists only to be re-encoded, and each
re-encode is the lossy 17% round trip measured above.

### Proposal

Store the generator's own latents for the seed window (f16) and pass them
straight to inpainting:

- **Size:** a 10 s seed is ~108 frames × 256 × 2 bytes ≈ 55 KB (110 KB at
  f32), versus 1.76 MB WAV or ~460 KB with lac/FLAC. Book total: 5.1 GB →
  ~170 MB (~340 MB at f32). f16 rounding adds ~0.05% relative error, far
  below the 17% it replaces.
- **Compression:** general-purpose compressors barely help, because the
  float mantissas are close to random. Measured on 6 Stable Audio latent
  files (2.4 MiB as f32), sizes relative to raw f32:

  | Storage | Size | Relative L2 error |
  |---|---|---|
  | f32 + zstd -19 (xz -9e: 92%, bzip2: 95%) | 92% | none |
  | f32 byte-shuffled (blosc-style byte planes) + zstd -19 | 84% (xz: 83%) | none |
  | **f16 raw** | **50%** | 2e-4 |
  | f16 byte-shuffled + zstd -19 | 43% (xz: 41%) | 2e-4 |
  | bf16 byte-shuffled + zstd -19 | 34% (xz: 33%) | 3e-3 |

  Every row's error is far below the 17% the current WAV round trip
  introduces. For the book's seeds that's f16 ≈ 171 MB, f16+zstd ≈ 146 MB,
  bf16+zstd ≈ 116 MB. The last 25-55 MB isn't worth a codec dependency, so
  plain f16 is the recommendation; byte-shuffle + zstd is a drop-in later
  if wanted.
- **Quality:** continuations condition on the model's own latents instead
  of a lossy re-encoding of them. Output changes (not bit-identical to
  today's path).

  A blind listening A/B passed (see "Seam A/B" below): stored latents
  were judged fine, and measured as equal or slightly better at the seam.
- **Speed:** saves the WAV encode/decode/base64 hops and one SAME encode
  per seeded call. Minor next to the DiT cost of re-rendering the seed
  window, which is the "2.4x per new second" noted in `musicgen`.

### What it takes

- **audio.cpp** (another local patch alongside the Higgs ones):
  - A request option to return the final DiT latents with the audio
    (`[latent_dim × T]` plus the frame count). It's already computed at
    `session.cpp:335`.
  - A request input that supplies inpainting latents directly instead of
    `inpaint_audio`, feeding `apply_inpaint_conditioning` unchanged.
  - The C API currently returns audio only, so both need C-ABI and
    `audiocpp-go` additions.
- **Alignment:** latent frames are 4096 samples (~93 ms). The seed is the
  last 10 s of the *served* clip, which ends 4 s before the render's end
  (the loop-crossfade overrun), at an arbitrary sample offset.
  - Keep whole frames covering `[served − 10 s, served)`, rounded out.
  - Set the inpaint mask start to the stored frames' exact duration.
  - The seam moves by at most half a frame.
  - When a region's last chunk is shorter than 10 s, the seed spans two
    chunks' latents. Concatenating frames from separate generations is
    fine as conditioning.
- **Storage format:** a small header followed by the f16 array:
  - a Stable Audio checkpoint hash,
  - `latent_dim`, frame count, frame-rate constants and dtype,
  - the served-clip sample offset of frame 0.

  File name `<regionID>.seed.lat`.
- **Model changes:** a hash mismatch means the latents are unusable. Fall
  back to an unseeded ("cut") first chunk, the same path an unscored or
  missing seed takes today.
- **Migration:** existing seed WAVs can be converted once by running
  `same_->encode` on them. That's exactly the conditioning today's path
  computes (up to the encoder's 4% noise-seed dependence), so migrated
  regions behave as they do now, and new renders get the cleaner
  generator latents.
- **Backend touch points:**
  - `musicgen` seed plumbing, and `ttsproto`/`ttsworker` request and
    response fields;
  - `jobs` seed write/read;
  - `audiomaint` orphan cleanup;
  - `bookexport` import/export. Latents are tied to the checkpoint, so an
    importing library needs the same one or falls back to cuts. Today
    `excludeMusic` already has a regenerate-instead path.
- **In-region chaining** benefits even without touching disk: keep the
  previous chunk's latents in the worker between the chained calls.

### Seam A/B (go decision)

Three continuations were rendered as `musicgen` does:

- a 34 s "previous region" render, served at 30 s;
- a seed of the served clip's last 10 s, rounded out to whole latent
  frames (frames 215-322, 10.031 s);
- a 20 s continuation with an identical request, seed and mask per
  variant.

The variants differ only in the conditioning. W is today's path (the seed
WAV is re-encoded). L injects the stored generator latents (probe hook
`LECTABLE_INPAINT_LATENTS`).

| Case | Variant | Seed re-render SNR (whole / last 0.5 s) | Spectral jump at seam (× local median) |
|---|---|---|---|
| same prompt (chunk chaining) | W | 19.6 / 14.0 dB | 4.2 |
| | L | 19.5 / 16.3 dB | 5.4 |
| new mood (region to region) | W | 19.7 / 19.3 dB | 1.16 |
| | L | 21.3 / 20.8 dB | 1.14 |
| synth ambient | W | 20.2 / 17.9 dB | 1.87 |
| | L | 20.7 / 18.7 dB | 1.92 |

With latents, the model's version of the seed window lands 1-2 dB closer
to the real seed near the cut. The seam discontinuity is unchanged within
noise (one render per case). Listening verdict (blind): latents are fine.
So the storage saving stands on its own; expect no audible regression and
no dramatic gain.

A side finding, independent of latents: inpainting regenerates the "kept"
seed window rather than copying it through (~20 dB SNR vs the real seed).
`musicgen` then splices the real seed onto the re-render's continuation,
so every seeded seam is slightly imperfect, most visibly within a region
(4-5x spectral jump in the same-prompt case, both variants). A short
crossfade at the splice, or clamping the seed frames during sampling,
would address that separately.

### Implementation status

The library and bindings layer is done (2026-09-26) for Stable Audio,
Higgs and PocketTTS:

- **audio.cpp** (local patch, `lectable-latents-codec.diff`):
  - a generic `codec` task in all three families, loading only each one's
    codec/autoencoder;
  - `LATENTS` artifacts (Stable Audio, PocketTTS) and `ACOUSTIC_TOKENS`
    (Higgs);
  - `return_latents` on generation;
  - Stable Audio: latents in place of init/inpaint audio;
  - Higgs: `return_reference_codes` and `reference_codes` input (the
    reference-code cache of Use 2);
  - a fix for an out-of-bounds write in short-window SAME encodes.
- **`audiocpp-go`:** `Latents` and `Codes`, with result/request helpers.
- **Verified on HIP:**
  - For every family, decoding a generation's returned latents/codes with
    a standalone codec session reproduces its audio bit for bit.
  - Higgs, on both the single-stream and batched paths: returned
    reference codes equal a standalone encode, and cloning from them equals
    cloning from the audio.
  - PocketTTS: encoding generated audio lands at cosine 0.986 to the
    generator's latents, the same space (like Stable Audio's 0.987).
- **PocketTTS decoder speed:** 44 ms for 8.2 s of speech on the GPU,
  0.85 s on 4 CPU threads.
- **Not done yet:** the live `libaudiocpp.so` rebuild and the backend
  changes (under "What it takes" above).

### Aside: speech through the Stable Audio VAE

Three voice refs (24 kHz mono) went through a codec encode → decode and
were compared after resampling back to 24 kHz mono:

- **Band energies preserved:** within ±0.2 dB, including above 6 kHz.
- **Fine detail rebuilt:** per time-frequency cell, the log spectrum
  differs by ~6 dB on average (correlation 0.76-0.80), and waveform SNR is
  8-12 dB.
- **Dual mono out:** mono in comes out as identical stereo channels.
- **Speed:** warm encode ≈ 176 ms, decode ≈ 200 ms per ~11.5 s on the GPU.

It's intelligible but reconstructed, like any neural codec, and no storage
win for speech (~5.5 KB/s at f16, vs 0.25 KB/s for Higgs codes).

## Use 2: Higgs reference-code cache

Per clone call, Higgs looks up the reference by sample hash in an
in-memory LRU (`reference_cache_slots`). On a miss it runs
`encode_reference`, a HuBERT-style semantic model plus the acoustic
encoder:

- **encode:** 384 ms on GPU (2.5 s on CPU) for a 20 s ref, in a cold
  process.
- **prefill:** the codes plus transcript are then prefilled: 580 ms cold
  (126 ms of it the actual run). Your `reference_kv_slots` patch already
  keeps that prefix on-device for repeat voices.

Persisting the codes (~5 KB per 20 s ref, keyed by the sample hash,
transcript and codec-weights hash) would skip the encode after worker
restarts and idle unloads. It would also skip sending the samples at all.

- The codes *are* what the model conditions on, so this is exact for
  Higgs.
- It needs a request option to pass codes instead of audio, and a way to
  return them after an encode.
- It does not replace the WAV: other clone models need audio, and
  decoding codes gives a lossy rebuild (5.8 dB waveform SNR, log-spectral
  correlation 0.91 against the original ref).
- Persisting the prefix KV to disk isn't worth it: tens of MB per voice
  (estimated), tied to the exact build, to save a ~126 ms warm prefill.

## Use 3: narration master for Higgs books

Narration WAV goes from the worker straight to Opus
(`audiopath.WriteClip`), with no Go-side post-processing. So a paragraph's
codes plus the codec-weights hash are a complete recipe:

- **Size:** 2 kbps, ~66 MB for the 73.5 h book (vs ~6.3 GB FLAC, ~5.7 GB
  lac).
- **Fidelity:** bit-exact on the same GPU and build. With f32 codec
  weights, within 1 LSB on 0.2% of samples on any backend, including
  CPU-only at ~10x real time. That's five orders of magnitude below what
  any lossy re-encode discards: faithful, not bit-exact.
- **Needs:**
  - keeping those weights for as long as masters exist.
  - Codes out (`return_latents`) and a codec-only decode session now
    exist in the local patch (Implementation status above).
- **PocketTTS books (the default clone model) work the same way:**
  - Normalized 32-dim latents at 12.5 Hz are ~0.8 KB/s at f16, so ~210 MB
    for the 73.5 h book.
  - Its decoder is bit-exact on the same GPU, and within 1 LSB GPU vs CPU
    with f32 weights (104 dB).
  - Books narrated by other families would still need a PCM master.

## Open questions / not measured

- Warm (in-process) cost of `same_->encode` and Higgs `encode_reference`.
  Only cold-process timings were taken.
- Whether a future Stable Audio or Higgs checkpoint keeps the same
  autoencoder or codec. If it does, stored latents and codes survive a
  model update.

Listening samples from the merge and round-trip tests were written to this
session's scratch directory. Regenerate them with the probe patch if
needed.
