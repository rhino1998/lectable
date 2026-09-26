# Report: a custom lossless mastering format for lectable audio

Answers `../mastering-format-experiment.md`. Everything below was measured on
this box (2026-09-26) over a fixed stratified sample; harness, sample lists,
raw results and the audio.cpp probe patch are in
`experiments/`.

## TL;DR

- **Compressing at all is the win.** Today everything is raw WAV (~11.6 GB for
  Spire's Spite's refs, seeds and ambience). Any decent lossless codec cuts
  that to ~4.3-4.5 GB. The choice of codec moves the result by only a
  further ~0.2-0.5 GB.
- **A custom codec was built and works:** `lac/` (pure Go, ~900 lines plus
  tests), 7-11% smaller than FLAC -8 on speech and seeds, 2.5% on
  ambience. It sits between Monkey's Audio and OptimFROG and is
  bit-reproducible across architectures.
- **Pure-Go FLAC is weaker than it looks.** The only maintained library
  (`mewkiz/flac`) has no LPC in its encoder, so it's 12% *worse* than
  real FLAC on speech.
- **Model-native storage is not lossless**, but it is a very good fit for
  two narrower jobs: Higgs codes as a narration "master" (~95x smaller than
  FLAC), and Stable Audio latents in place of seed WAVs (5.1 GB → ~0.2 GB).
  Every decoder is bit-exact on the same GPU and build, and never across
  backends.

## Sample and method

100 files per kind, stratified by RMS; seeds span 76 chapters and all 13
emotions appear among the variants (`experiments/*.list`, drawn by `experiments/sample.py`):

| Kind | Format | Sample |
|---|---|---|
| voice (base refs) | mono 24 kHz | 100 of 513 |
| variant (emotion refs) | mono 24 kHz | 100 of 985 |
| seed | stereo 44.1 kHz, 10 s | 100 of 3101 |
| ambience | stereo 44.1 kHz, 60 s | 100 of 588 |

The Go harness runs each codec as a process on one thread and records
size, CPU time (user+sys, so it includes process startup, which flatters
nothing on 60 s files but depresses xRT on short voice clips) and a
bit-exact PCM comparison of every file. **All 17 codecs verified 400/400.**
FLAC/ALAC/TTA/WavPack are ffmpeg's encoders (no `flac` binary here),
Monkey's Audio 13.26 was built from source, OptimFROG 5.100 is the
official Linux binary.

## Results (size as % of 16-bit PCM; lower is better)

| Codec | voice | variant | seed | ambience | Pure Go? |
|---|---|---|---|---|---|
| OptimFROG `--preset max` | **46.7** | **46.7** | **24.7** | **44.3** | no |
| OptimFROG preset 2 (default) | 47.9 | 48.0 | 25.8 | 44.9 | no |
| **lac, speech preset** | 48.6 | 50.1 | 25.8 | 45.0 | **yes** |
| **lac, music preset** | 50.0 | 51.4 | 26.2 | 45.1 | **yes** |
| Monkey's Audio extra high (`-c4000`) | 50.1 | 51.5 | 26.3 | 45.0 | no |
| Monkey's Audio insane (`-c5000`) | 51.1 | 52.5 | 25.7 | 44.8 | no |
| WavPack (ffmpeg, level 8) | 50.9 | 52.3 | 27.3 | 45.5 | no |
| TTA | 52.4 | 53.6 | 27.6 | 46.0 | no |
| FLAC -12 | 53.1 | 55.5 | 28.3 | 46.2 | no |
| **FLAC -8 (baseline)** | 53.8 | 56.1 | 28.3 | 46.3 | no |
| ALAC | 53.1 | 54.1 | 30.1 | 46.8 | no |
| `mewkiz/flac` (pure-Go FLAC) | 60.3 | 60.6 | 29.9 | 48.5 | yes |
| xz -9e | 68.3 | 67.6 | 56.3 | 61.0 | yes |
| bzip2 -9 | 65.3 | 64.5 | 74.5 | 67.0 | yes |
| zstd -19 | 73.9 | 72.6 | 76.4 | 73.5 | yes |

Relative to FLAC -8:

| Codec | voice | variant | seed | ambience |
|---|---|---|---|---|
| OptimFROG max | −13.2% | −16.7% | −12.9% | −4.3% |
| lac speech | −9.7% | −10.7% | −9.0% | −2.9% |
| lac music | −7.0% | −8.5% | −7.6% | −2.5% |
| `mewkiz/flac` | +12.1% | +8.0% | +5.5% | +4.8% |

Decode speed, one core (× real time):

| Codec | voice | variant | seed | ambience |
|---|---|---|---|---|
| FLAC -8 (ffmpeg) | 230 | 142 | 162 | 399 |
| OptimFROG preset 2 | 606 | 564 | 172 | 168 |
| OptimFROG max | 72 | 78 | 20 | 19 |
| Monkey's Audio `-c4000` | 599 | 551 | 183 | 200 |
| lac speech | 116 | 115 | 34 | 31 |
| lac music | 454 | 437 | 137 | 130 |
| `mewkiz/flac` | 1078 | 1043 | 342 | 293 |

On a quiet core, the lac music preset decodes a 60 s ambience loop in
~0.4 s (150x), which meets the "well under a second" bar. The speech
preset is ~125x on mono 24 kHz, a 20 s voice ref in ~0.16 s, but only
~33x on stereo, so it's for speech only (`PresetAuto` picks by channel
count). Peak RSS is small for all codecs: OptimFROG and Monkey's Audio
3-10 MB, ffmpeg ~49 MB. lac decodes whole files in memory, ~4 MB for a
voice ref and ~52 MB for a 60 s stereo loop. The RSS column in
`results.jsonl` is contaminated by the harness's own fork and should be
ignored; these figures come from `experiments/memtime/`.

## What each idea bought

1. **Tuning the classic pipeline** was the whole gain. It came from:
   - Backward-adaptive NLMS/sign-sign-LMS cascades instead of FLAC's
     per-block LPC plus Rice codes, worth ~7-10% on speech and seeds by
     itself.
   - An adaptive binary range coder for the residual, which beats Rice
     coding. Three coder variants tried were all within ±0.1% of each
     other, so the coder is saturated.
   - An adaptive mix of stage predictions: +0.1-0.15%.

   Longer filters stop paying quickly: 512-1024 taps were worse on
   speech (Monkey's `-c5000` is worse than `-c4000` here too), and cost
   linear decode time.
2. **Stereo.** Both channels share an interleaved history, so each
   channel's predictor sees the other's already-coded samples. It's built
   in and not separately ablated. Ambience is wide and noise-like (L/R
   correlation -0.14 to 0.77), and even OptimFROG max only manages −4%.
3. **Speech structure.** A 256-tap stage at 24 kHz already spans pitch
   lags down to ~94 Hz. Longer (pitch-range) filters made things worse.
   No explicit long-term predictor was built.
4. **Loops.** No exploitable repetition: ambience loops are Stable Audio
   texture with a 4 s equal-power crossfade at the head, not a repeated
   segment. Not pursued further.
5. **Cross-file context.** Priming the adaptive state with another clip
   of the *same* voice saved 0.25%; an unrelated voice saved 0.21%. Not
   worth a shared resource.
6. **Model-native.** See below. It's the only idea with order-of-magnitude
   gains, but it isn't lossless.

Still unexplained: a ~1.5-2 point gap to OptimFROG on speech. OptimFROG is
closed-source, and further cascade and step-size sweeps plateaued.

## Model-native representation

(Expanded, with later measurements, in `latent-storage-report.md`.)

What each generator emits before its decoder:

| Audio | Generator | State before decode | Rate | % of PCM |
|---|---|---|---|---|
| voice refs, variants | BreezeTTS 2 (Mimi-style codec) | 16 × 11-bit codes @ 12.5 Hz | 2.2 kbps | 0.57% |
| narration (Higgs books) | Higgs Audio v3 | 8 × 10-bit codes @ 25 Hz | 2.0 kbps | 0.52% |
| seeds, ambience | Stable Audio 3 SAME | 256-d float latents @ 10.8 Hz | 88 kbps f32 / 44 f16 | 6.3% / 3.1% |

Decoder determinism, measured by patching audio.cpp to dump and reload
codes/latents and decode them repeatedly
(`experiments/det-probe/decoder-probe.patch`):

| Decoder | same GPU, same/new process | GPU vs CPU, shipped q8_0 weights | GPU vs CPU, f32 weights |
|---|---|---|---|
| Breeze | bit-identical | 42 dB SNR, 78% of samples differ, max 470 LSB | 86 dB, 8% differ, max 4 LSB |
| Higgs | bit-identical (also CPU 4 vs 16 threads) | 61 dB, 60% differ, max 48 LSB | 116 dB, 0.2% differ, max 1 LSB |
| Stable Audio | bit-identical | 33 dB, 90% differ, max 396 LSB | 41 dB, 68% differ, max 167 LSB |

For scale: today's served 48 kbps Opus narration is at 18.7 dB against its
source. All of these differences are inaudible and far below Opus's error.
They matter only because "lossless" means bit-exact. The cause, verified by
switching weights to f32:

- **Quantized weights (the main cause).** ggml quantizes activations
  differently per backend, so q8_0 weights make GPU and CPU diverge.
- **Float reduction order and approximations.** The rest is ordinary
  float differences, which Stable Audio's decoder amplifies on its own.
- **Ruled out:** Stable Audio's decoder noise is host-generated with the
  same RNG policy on both backends, and Breeze's bf16 mode doesn't touch
  its decoder.

Decode speed from codes/latents:

| Decoder | GPU | CPU, 4 threads | Decode-path weights |
|---|---|---|---|
| Breeze | 45x | 1.9x | — |
| Higgs | ~300x | ~8x | ~43 MB at f16, of a 4.85 GB file (compute estimate) |
| Stable Audio | 45x | 1.1x | — |

audio.cpp has no decode-only entry point, so today decoding loads the full
model.

**Verdict on "lossless via model state": not feasible.** Nothing is
bit-exact across backends, and a driver or ggml update could silently
change every file on the same GPU. A correction residual to force
exactness would cost 5.5-6 bits/sample, about as much as coding the whole
file losslessly, and would itself be decoder-specific. Existing files can't
be converted either, since no codes or latents were ever kept. Voice refs
also go through WSOLA, truncation and loudness gain after decode.

It *is* a good fit for two non-lossless jobs:

- **Narration master for Higgs books.** The backend writes the worker's
  WAV straight to Opus, so codes plus the codec-weights hash reproduce a
  paragraph to within 1 LSB on any backend with f32 codec weights. That's
  ~66 MB for the 73.5 h book versus ~6.3 GB as FLAC.

  It needs audio.cpp patches: return codes, plus a decode-only path. It
  doesn't cover PocketTTS (the default clone model, continuous latents,
  not examined) or other families.
- **Seeds as Stable Audio latents.** A seed WAV exists only to be
  re-encoded by `same_->encode` for inpainting. Storing the generator's
  own latents (sliced at a 4096-sample frame boundary) would cut 5.1 GB to
  ~170-340 MB and remove a lossy round trip.

  This also needs audio.cpp patches (return latents, accept latents for
  inpainting). The latents are tied to the checkpoint, so store a model
  hash and fall back to an unseeded "cut" on a mismatch.

Related findings:

- **Higgs reference codes can be cached.** Higgs' reference encoder emits
  the same code format its decoder takes (verified by round trip), about
  5 KB per 20 s ref. Persisting them would skip the ~0.4 s cold
  `encode_reference` after worker restarts.
- **Prefix KV to disk isn't worth it.** It runs to tens of MB per voice,
  and the warm prefill it would save is only ~126 ms.

## Projected savings (Spire's Spite)

Storage (GB; raw sizes are today's WAV):

| Audio | raw | `mewkiz/flac` | FLAC -8 | lac | OptimFROG max |
|---|---|---|---|---|---|
| voice refs (676 MB now) | 0.68 | 0.41 | 0.37 | 0.33 | 0.32 |
| seeds | 5.1 | 1.52 | 1.44 | 1.33 (music) | 1.26 |
| ambience | 5.8 | 2.81 | 2.68 | 2.62 (music) | 2.57 |
| **total** | **11.6** | **4.74** | **4.49** | **4.28** | **4.15** |
| lossless narration master (hypothetical) | 12.7 | ~7.1 | 6.3 | ~5.7 | ~5.4 |

The model-native alternatives:

- Seed latents: ~0.17-0.34 GB instead of the seeds row.
- Higgs narration codes: ~0.07 GB instead of the master row.

## Recommendation

> **Decision (2026-09-26): lac rejected.** Its gain over FLAC -8 (~0.2 GB
> for Spire's Spite) was judged too small for the costs in point 2 below.
> Point 1 still stands; use FLAC rather than lac. The rest of this section
> is the original recommendation, kept for the record.

1. **Stop storing WAV for seeds and ambience.** That saves ~7 GB of 11.6
   GB with any codec. It's the bulk of the book-export size too (the
   export notes put these two at 10.2 GB of 16 GB). Ambience loops and
   seeds are read on generation paths, not playback, so decode speed is
   not critical.
2. **Use lac (`lac/`) for that.** Under the pure-Go rule the real
   alternative isn't "FLAC -8" but `mewkiz/flac`, which is 5-12% larger
   than FLAC -8. Writing a FLAC LPC encoder to close that gap is
   comparable work to maintaining lac, and still ends 3-10% behind it.
   lac vs pure-Go FLAC saves ~0.45 GB today and ~1.4 GB per book if
   lossless narration masters are kept.

   The cost: a private format. It needs a server-side decode for any
   browser preview, and its arithmetic is frozen forever (guarded by
   `TestGolden`; a change means a new version byte plus keeping the old
   decoder). Maintenance is ~900 lines of dense numeric code with a fuzz
   target.
3. **Voice refs:** leave them as WAV, or use FLAC if you want the
   preview endpoints to serve files browsers and Android play directly.
   lac saves only ~35 MB over FLAC there and adds ~160 ms per read on
   every clone call.
4. **Model-native:**
   - Worth prototyping for seeds (latents): the largest remaining
     reduction for one moderate audio.cpp patch.
   - Worth it for Higgs narration masters if lossless masters are ever
     wanted for Higgs books.
   - Not viable as a lossless archive format.
5. **Don't build further toward OptimFROG.** The remaining 3-4% is
   ~0.15 GB here and would need a more complex, slower predictor.

## The library

`lac/` is its own module (`github.com/rhino1998/lectable/lac`), added to
`make lint-go`. It isn't imported by the backend (rejected; see Recommendation).

- **API:** `Encode(pcm, channels, rate, preset)`, `Decode`, `ReadFormat`,
  `EncodeWAV`/`DecodeWAV`; presets `PresetSpeech`/`PresetMusic`/`PresetAuto`.
- **Robustness:** the header carries a CRC-32 of the PCM, and every
  stream records its own predictor cascade, so presets can change without
  breaking old files. Fuzzed for 7.5M executions with no panics.
- **Determinism:** golden hashes are identical on amd64 v1 and v3 (FMA),
  386 SSE2 and 386 softfloat. The arm64 build contains no fused
  multiply-add instructions, while an unguarded control loop does. arm64
  itself was not *run* (no emulator here).
- **Scope:** 16-bit PCM only, up to 8 channels. The WAV helpers keep audio
  data, not extra RIFF chunks.
