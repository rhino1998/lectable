# audiocpp-go

Go (cgo) bindings for [audio.cpp](https://github.com/0xShug0/audio.cpp)'s C
ABI — a separate, external C++ audio-inference project, not part of this
repo. This is its own Go module (`github.com/rhino1998/lectable/audiocpp-go`,
not `.../backend`), pulled in by `backend` via a local `replace` directive
in `backend/go.mod`. **Only `backend/internal/audioworker` imports it** -
that package is built exclusively into the separate `backend/cmd/ttsworker`
binary, never into `backend/cmd/server` itself (see `backend/CLAUDE.md`'s
"ttsworker / audioworker / llmworker" section for why: a confirmed native memory leak
in audio.cpp, isolated into a disposable process by keeping this binding
out of the main, always-up backend binary entirely).

## Layout

- `audiocpp/` — the package. One file per concern: `registry.go`, `model.go`,
  `session.go`, `request.go`, `result.go`, `stream.go`, `options.go` wrap the
  ABI's handle types; `wav.go` is a small dependency-free 16-bit PCM WAV
  reader/writer, not part of the ABI itself.
- `audiocpp/include/audiocpp.h` — a vendored copy of audio.cpp's C ABI
  header, so this package compiles without an audio.cpp checkout present.
  It's a single dependency-free header (only `<stddef.h>`/`<stdint.h>`); if
  it ever needs updating, copy the new one from an audio.cpp checkout's
  `include/audiocpp.h` verbatim — don't hand-edit it here.
- `audiocpp/smoke_test.go` — exercises the bindings against a real Silero
  VAD run. Tests needing audio.cpp's model assets skip themselves unless
  `AUDIOCPP_CHECKOUT` points at a local audio.cpp checkout with the C API
  built; see the package doc comment and README.md for the exact steps.

## Latents, codes and the codec task

These are thin typed layers over the artifact API for this repo's local
audio.cpp latents/codec patch (`backend/CLAUDE.md`, "Latents/codec patch"):
- **`audiocpp/latents.go`:** `Latents` (continuous: Stable Audio,
  PocketTTS; f32, time-major, `Slice`), `Result.Latents()`,
  `Request.AddLatents()`.
- **`audiocpp/codes.go`:** `Codes` (discrete: Higgs; int32, time-major),
  `Result.Codes(id)`, `Request.AddCodes(c, id)`. The id is `"codes"` or
  `"reference_codes"`.

A `"codec"` session encodes audio to latents/codes, or decodes them to
audio, loading only the family's codec/autoencoder. The request option
`return_latents=true` makes generation return its decoder input (latents or
codes, with `chunk_frames` meta where text is chunked). Decoding that input
in a codec session reproduces the generated audio exactly on the same
backend.

Family extras:
- **Stable Audio:** accepts latents in place of `init_audio`/`inpaint_audio`.
- **Higgs:** `return_reference_codes=true` returns a clone's reference
  codes, and `AddCodes(c, "reference_codes")` reuses them instead of
  reference audio.

Model-gated tests (each skips unless its env var is set):
- `TestStableAudioCodec`: `AUDIOCPP_STABLE_AUDIO_MODEL`.
- `TestHiggsCodec`: `AUDIOCPP_HIGGS_MODEL` (the .gguf).
- `TestPocketTTSCodec`: `AUDIOCPP_POCKET_TTS_MODEL` (package dir).

They take an optional `AUDIOCPP_TEST_BACKEND` (default `cpu`; use `hip`
here, since Stable Audio and Higgs generation on CPU need `-timeout 30m`).
The vendored header comes from the *patched* local checkout, so re-copying
it from an unpatched one would drop `AUDIOCPP_ARTIFACT_LATENTS`.

## What's vendored vs. external

Only the header is vendored. audio.cpp's C++ source, its CMake build (which
produces `libaudiocpp.so`), and its model assets are **not** vendored here —
they're a large separate native project. Anything that *links* against this
package (not just compiles it) needs `CGO_LDFLAGS`/`LD_LIBRARY_PATH` pointed
at a local audio.cpp build; see README.md.

## Version sensitivity

`libaudiocpp.so` is a native shared library, not vendored - check
`audiocpp.ABIVersionMajor` against whatever audio.cpp checkout's build
you're linking if something seems off after updating either side.
