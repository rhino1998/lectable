# lac

**Status: rejected experiment (2026-09-26).** Kept for reference, not for
use - don't import it from `backend` or build on it. It beats FLAC -8 by
only ~3-11% (~0.2 GB for a whole book), which doesn't pay for a private
format with frozen arithmetic, no browser/Android playback, and ~900 lines
of numeric code to maintain. Use FLAC for lossless audio instead.
`latent-storage-report.md` here is unaffected and still current.

A pure-Go lossless codec for 16-bit PCM (`github.com/rhino1998/lectable/lac`,
its own module, no dependencies). Built by the mastering-format experiment
(`mastering-format-report.md` here has the benchmarks and the
recommendation; `latent-storage-report.md` covers storing model
latents/codes instead of audio); not imported by `backend`.

Monkey's Audio/OptimFROG-style design: pre-emphasis, a cascade of
backward-adaptive predictors (float NLMS + integer sign-sign LMS, sharing a
channel-interleaved history), an optional adaptive mix of stage
predictions, and an adaptive binary range coder. 5-11% smaller than
FLAC -8 on lectable's speech and music. The package doc comment (`doc.go`)
has the byte format.

## Files

- `codec.go` — API (`Encode`/`Decode`/`ReadFormat`), presets, header,
  validation, the shared encode/decode cascade.
- `predict.go` — the stages (`nlms`, `sslms`, `mixer`) and prediction clamping.
- `residual.go` / `rangecoder.go` — residual model and range coder.
- `wav.go` — `EncodeWAV`/`DecodeWAV` for 16-bit PCM WAV files.

## Invariants

- **The arithmetic is the format.** A decoder must reproduce the encoder's
  predictions bit for bit, on every platform, forever:
  - Wrap every float product in an explicit `float64(...)`, which stops
    the compiler fusing it into an FMA (it does on arm64).
  - Keep sums in a fixed order.
  - Convert floats to ints only through `clampRound`.
- **`TestGolden` pins exact encoder output.** If it fails, don't update
  the hashes. Bump the version byte and keep decoding the old version.
- **Check other float implementations** after touching `predict.go`:
  `GOARCH=386 GO386=softfloat go test ./...` must pass. It's pure software
  IEEE, so it catches accidental FMA- or x87-style dependence. Also,
  `GOARCH=arm64 go build -gcflags=-S . 2>&1 | grep -c FMADD` should print 0.
- **Predictions are clamped to ±2^20**, which keeps every residual under
  the coder's 2^25 limit for any input, corrupt streams included.
- **`Decode` must never panic.** Run the fuzz target
  (`go test -fuzz FuzzDecode`) after changing parsing or the residual coder.
- **`TestSamples`** round-trips real WAVs when `LAC_SAMPLES` points at a
  directory of `<kind>/*.wav`, e.g. `experiments/`'s sample
  set (recreate it with `sample.py`).
