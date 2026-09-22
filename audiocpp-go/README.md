# audiocpp-go

A cgo wrapper around [audio.cpp](https://github.com/0xShug0/audio.cpp)'s C ABI
(`audiocpp.h`, vendored in `audiocpp/include/` — see [`docs/c_api.md`](https://github.com/0xShug0/audio.cpp/blob/main/docs/c_api.md)
in the audio.cpp repo for the full contract). It embeds audio.cpp
in-process — no `audiocpp_cli` subprocess, no server — so a loaded model and
a warm session are reused across calls.

This module lives in the `lectable` repo but is a self-contained
binding for a separate, external project. It's the Go analogue of audio.cpp's
own `bindings/python`, and is imported by `backend/internal/audioworker`
(built only into the separate `backend/cmd/ttsworker` binary - see
`backend/CLAUDE.md`).

This is a thin wrapper: every type mirrors a C ABI handle 1:1 (`Registry` ->
`audiocpp_registry`, `Model` -> `audiocpp_model`, `Session` -> `audiocpp_session`,
...), it adds no behaviour the C API does not already have, and it inherits the
same contract: errors come back as `*audiocpp.Error` instead of a raw status
code, and anything borrowed from a `Result` is copied into plain Go values
before it's handed back.

## What's vendored and what isn't

`audiocpp/include/audiocpp.h` is vendored here (it's a single, dependency-free
header — only `<stddef.h>`/`<stdint.h>`), so this module compiles without an
audio.cpp checkout present. What is **not** vendored, because it's a large
separate native project with its own build and multi-gigabyte model assets:

- audio.cpp's own C++ source and CMake build (produces `libaudiocpp.so`).
- Its model assets (`assets/`) and downloadable model packages.

So building anything that *links* against this package (not just compiles it)
still needs a local [audio.cpp](https://github.com/0xShug0/audio.cpp)
checkout with the C API built.

## Build the native library

From an audio.cpp checkout:

```bash
cmake -S . -B build -DAUDIOCPP_BUILD_C_API=ON
cmake --build build --target audiocpp
```

This produces `build/bin/libaudiocpp.so` (`.dylib` on macOS, `.dll` on
Windows). Pass `-DENGINE_ENABLE_HIP=ON -DGPU_TARGETS=<gfxNNN>` (AMD/ROCm),
`-DENGINE_ENABLE_CUDA=ON` (NVIDIA), or `-DENGINE_ENABLE_VULKAN=ON` alongside
`-DAUDIOCPP_BUILD_C_API=ON` first if you want a GPU backend available through
`Model.Session(..., BackendConfig{Backend: "cuda"|"hip"|"vulkan"})`.

## Use the bindings

Point `cgo` and the dynamic loader at that build directory — the header is
vendored, but the compiled library is not:

```bash
export CGO_LDFLAGS="-L/path/to/audio.cpp/build/bin"
export LD_LIBRARY_PATH="/path/to/audio.cpp/build/bin:$LD_LIBRARY_PATH"   # Linux
export DYLD_LIBRARY_PATH="/path/to/audio.cpp/build/bin:$DYLD_LIBRARY_PATH" # macOS
```

Then, from your own Go module:

```bash
go get github.com/rhino1998/lectable/audiocpp-go
```

or, working inside this checkout, `go build ./...` / `go test ./...` pick up
`CGO_LDFLAGS`/`LD_LIBRARY_PATH` the same way any other cgo package does.

## Usage

```go
package main

import (
	"fmt"
	"log"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
)

func main() {
	registry, err := audiocpp.NewRegistry("")
	if err != nil {
		log.Fatal(err)
	}
	defer registry.Close()

	model, err := registry.LoadModel(
		"/path/to/audio.cpp/assets/framework/models/silero_vad",
		audiocpp.ModelConfig{FamilyHint: "silero_vad"},
		nil,
	)
	if err != nil {
		log.Fatal(err)
	}
	defer model.Close()

	session, err := model.Session("vad", "offline", audiocpp.BackendConfig{Backend: "cpu"}, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	clip, err := audiocpp.ReadWavFloat32("/path/to/audio.cpp/assets/resources/sample_16k.wav")
	if err != nil {
		log.Fatal(err)
	}

	request := audiocpp.NewRequest()
	defer request.Close()
	request.SetAudio(clip.Samples, clip.SampleRate, clip.Channels)
	if err := request.Err(); err != nil {
		log.Fatal(err)
	}

	result, err := session.Run(request)
	if err != nil {
		log.Fatal(err)
	}
	defer result.Close()

	segments, err := result.Segments()
	if err != nil {
		log.Fatal(err)
	}
	for _, seg := range segments {
		fmt.Println(seg.StartSample, seg.EndSample, seg.Confidence)
	}
}
```

`task` and `mode` take the same spellings as `--task`/`--mode`; `Backend`
takes the same spellings as `--backend` ("cpu", "cuda", "hip"/"rocm",
"vulkan", "metal", "best"). `Registry.LoadModel`'s `ModelConfig` maps onto
`--family`, `--config`, `--weight`, and `--model-spec-override`; an `*Options`
(built with `audiocpp.NewOptions(map[string]string{...})`) maps onto
`--load-option` / `--session-option` / `--request-option` depending on where
it's passed.

### Voice cloning and voice design

Families tagged `Clone` in audio.cpp's model table want a speaker reference;
families tagged `Design` want style/emotion conditioning. Both go through
`Request`:

```go
// Clone: prime the request with a reference clip before synthesizing new
// text in that voice.
request := audiocpp.NewRequest()
request.SetText("Hello from audio.cpp.", "english").
	SetVoiceAudio(reference.Samples, reference.SampleRate, reference.Channels).
	SetOption("reference_text", "Some call me nature...")

// Design: style/emotion conditioning, for families that advertise it via
// Model.SupportsStyleCondition.
request := audiocpp.NewRequest()
request.SetText("Hello from a designed voice.", "english").
	SetOption("instruct", "A warm, low-pitched adult narrator speaking calmly.")
```

`Model.SupportsSpeakerReference()` and `Model.SupportsStyleCondition()` tell
you whether a loaded family accepts these before you set them. As with the
Python wrapper: check `Model.Supports(task, mode)` for the family you loaded
rather than assuming `"clon"`/`"vdes"` apply uniformly -- Qwen3 TTS's Base
variant, for example, only advertises `Model.Supports("tts", "offline")` and
does voice cloning by conditioning a normal `"tts"` session with
`SetVoiceAudio`, while its VoiceDesign variant is a genuinely separate
`"vdes"` session.

Two things that are easy to get wrong the first time (same as the Python
wrapper -- found by actually running Qwen3 TTS end to end):

- **Language strings are per-family, not ISO codes.** `Model.Languages()` is
  the source of truth -- Qwen3 TTS wants `"english"`, not `"en"` or `"en-us"`;
  a request with a language `Model.Languages()` doesn't list returns an error
  from `Session.Run`, not from `Request.SetText`.
- **A family's own request options carry conditioning the typed `Request`
  setters don't cover.** Qwen3 TTS reads the reference transcript from the
  `"reference_text"` request option and the design instruction from
  `"instruct"`, both via `Request.SetOption` -- `Model.Options(OptionScopeRequest)`
  lists exactly what a given family accepts.

### Streaming

```go
session, _ := model.Session("vad", "streaming", audiocpp.BackendConfig{Backend: "cpu"}, nil)
policy, _ := session.StreamPolicy()
session.StreamStart(nil)

offset := 0
chunk := int(policy.ChunkSamples)
for offset+chunk <= clip.Frames() {
	block := clip.Samples[offset*clip.Channels : (offset+chunk)*clip.Channels]
	event, _ := session.StreamPush(block, clip.SampleRate, clip.Channels, int64(offset))
	if event != nil {
		activity, _ := event.VoiceActivity()
		for _, a := range activity {
			fmt.Println(a.Kind, a.Sample, a.Probability)
		}
		event.Close()
	}
	offset += chunk
}

result, _ := session.StreamFinish()
defer result.Close()
```

## What this wrapper does not do

- No model download/conversion helpers -- point `Registry.LoadModel` at a
  path you already have (see audio.cpp's model manager, `docs/gguf.md`, or
  `tools/model_manager_v2.py`).
- No audio file I/O beyond a minimal 16-bit PCM WAV reader/writer
  (`audiocpp.ReadWavFloat32` / `audiocpp.WriteWavFloat32`) used by the example
  above; bring your own decoder for other formats.

## Testing

`audiocpp/smoke_test.go` mirrors audio.cpp's own
`bindings/python/tests/test_smoke.py`: it runs Silero VAD (bundled in an
audio.cpp checkout, no model download needed) over
`assets/resources/sample_16k.wav`, offline and streaming.

Tests that need those assets skip themselves unless `AUDIOCPP_CHECKOUT`
points at a local audio.cpp checkout with the C API built; the rest (ABI
version, task vocabulary, registry listing, request-builder round-trips) run
unconditionally, needing only `libaudiocpp.so` to link against.

```bash
AUDIOCPP_CHECKOUT=/path/to/audio.cpp \
CGO_LDFLAGS="-L/path/to/audio.cpp/build/bin" \
LD_LIBRARY_PATH="/path/to/audio.cpp/build/bin" \
    go test ./... -v
```
