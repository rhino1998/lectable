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
