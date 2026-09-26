# Experiment: a custom lossless mastering format for lectable audio

## Goal

Find out whether a custom lossless format can store lectable's
uncompressed audio meaningfully smaller than FLAC, and whether that's
worth building. The output is a findings report and a throwaway
benchmark harness, not a production change.

## Context

lectable stores three kinds of uncompressed audio (all under
`backend/data/`):

| Kind | Where | Format | Current total (Spire's Spite) |
|---|---|---|---|
| Voice reference clips (TTS speech) | `voice-refs/*.wav`, `voice-refs/variants/*/*.wav` | mono, 24 kHz, 16-bit | ~450 MB, library-wide |
| Music seeds | `audio/<book>/<chapter>/music/*.seed.wav` | stereo, 44.1 kHz, 16-bit, 10 s each | 5.1 GB |
| Ambience loops | `audio/<book>/ambience/*.wav` | stereo, 44.1 kHz, 16-bit, 60 s seamless loops | 5.8 GB |

Narration itself is served as 48 kbps Opus, but a lossless narration
master (same format as the voice refs) is on the table: about 6.3 GB as
FLAC for the book's 73.5 h. The voice refs are the best stand-in for
narration.

Measured so far (60 voice refs, per-file, size as % of raw PCM):

| Method | Size |
|---|---|
| WavPack (max, ffmpeg) | 49.9% |
| TTA | 51.4% |
| ALAC | 52.2% |
| FLAC -8 | 52.9% |
| bzip2 -9 | 64.5% |
| xz -9e | 67.6% |
| zstd -19 | 73.2% |

FLAC on a 40-file sample: ambience 47%, seeds 29%.

## Constraints

- **Exactly lossless**: decoding must give back the original PCM bit
  for bit. Verify every file in every run.
- **Pure Go** for anything that would run in `backend/cmd/server` (see
  `backend/CLAUDE.md` - no native libraries there). Prototyping in
  another language is fine if the result could be ported.
- **Per-file random access**: each clip must stay independently
  decodable (clips are regenerated, deleted and served one at a time). A
  shared, versioned side resource (dictionary, codebook, model weights)
  is acceptable if you measure its size too.
- Decode must be fast enough to read a clip on demand (a 60 s ambience
  loop in well under a second on one core).
- Don't modify anything under `backend/data/`. Copy samples into a
  scratch directory.

## Ideas to try (pick the promising ones; add your own)

1. **Tune the classic pipeline.** Higher-order or adaptive linear
   prediction (per-block LPC order search, cascaded LMS/NLMS like
   OptimFROG/Monkey's Audio), with residuals entropy-coded by adaptive
   arithmetic/rANS coding instead of FLAC's Rice codes.
2. **Exploit stereo** (seeds, ambience): mid/side or adaptive
   inter-channel prediction.
3. **Exploit speech structure** (voice refs/narration): long-term
   (pitch) prediction on top of short-term LPC; silence/low-energy
   segment handling.
4. **Exploit the loops**: ambience loops are seamless and texture-like;
   check for long-range self-similarity a predictor could use.
5. **Cross-file context**: a shared trained context or predictor per
   voice or per kind of audio, and whether it beats per-file coding
   once its size is counted.
6. **Model-native representation** (investigate first, it may dominate
   everything else): most of lectable's TTS and music models produce
   discrete audio-codec tokens that a codec decoder turns into waveform
   (see `backend/internal/audioworker` and the audio.cpp checkout at
   `/home/rhino/audio.cpp`). Storing tokens could be orders of magnitude
   smaller. Find out what's actually available at the generation
   boundary, whether the decoder is deterministic across runs and
   hardware (GPU float non-determinism matters - "lossless" here would
   mean "reproduces the model's output"), and what a model or codec
   version change would cost. Note the backend `CLAUDE.md` already says
   seeded generation isn't bit-reproducible across code paths, so check
   the decoder alone, not end-to-end generation.

## Method

- Sample about 100 files of each kind (stratified: different voices,
  quiet and busy music, several ambience settings). Record the sample
  list so runs are comparable.
- Write a small benchmark harness (Go preferred) that runs each codec
  over the sample and reports, per kind: size as % of PCM, encode and
  decode speed (x real-time, one core), peak memory, and round-trip
  verification.
- Always include FLAC -8 and the best available general-purpose
  compressor as baselines. If you can build the reference encoders, add
  OptimFROG and Monkey's Audio as upper bounds for "classic" lossless.

## Deliverable

A short report (markdown) with:
- The results table per audio kind, including baselines.
- What each idea bought over FLAC, and what didn't work.
- A recommendation: stay with FLAC, adopt an existing codec, or build a
  custom one, with the projected savings on the Spire's Spite numbers
  above and an honest estimate of the code to maintain.
- For the model-native idea specifically: feasible or not, and why.
