# llamacpp-go

A cgo wrapper around [llama.cpp](https://github.com/ggml-org/llama.cpp)'s
public C API (`llama.h`, vendored in `llamacpp/include/` along with the
ggml/gguf headers it depends on -- see below for exactly which files and
why). It embeds llama.cpp in-process: a loaded model and its context are
plain Go values that live for as long as the calling program keeps them, no
subprocess and no server in between -- so a Qwen (or any other GGUF) text
model can be hosted directly inside a Go binary.

This module lives in the `lectable` repo but is a
self-contained binding for a separate, external project, the same way
[`audiocpp-go`](../audiocpp-go) is for audio.cpp. It is currently
**standalone** -- nothing in `backend` imports it yet. It exists for a new,
non-TTS text-generation feature to build against later; the existing
audio.cpp-based TTS pipeline (`backend/internal/audioworker`,
`backend/cmd/ttsworker`) is unrelated and unaffected.

## What's vendored and what isn't

Vendored, verbatim, from a recent `ggml-org/llama.cpp` checkout:

- `llamacpp/include/llama.h` -- the public C API.
- `llamacpp/include/ggml.h`, `ggml-cpu.h`, `ggml-backend.h`, `ggml-alloc.h`,
  `ggml-opt.h`, `gguf.h` -- headers `llama.h` itself `#include`s. Unlike
  audio.cpp's `audiocpp.h` (a small, hand-written, dependency-free ABI
  purpose-built for bindings), llama.cpp's own C API is its full engine
  header and pulls in ggml's, so all of these need to be present for
  `llama.h` to parse. They're declarations only; update all of them together
  from the same llama.cpp checkout if you ever need to bump versions, rather
  than hand-editing one.

**Not** vendored, because they're large separate native build artifacts:

- llama.cpp's and ggml's own C/C++ source and CMake build (produces
  `libllama.so` and its `libggml*.so` companions).
- Any actual GGUF model weights.

So building anything that *links* against this package (not just compiles
it) needs a local llama.cpp checkout built as shared libraries.

## Build the native libraries

From a llama.cpp checkout:

```bash
cmake -S . -B build -DBUILD_SHARED_LIBS=ON
cmake --build build --target llama
```

Add `-DGGML_CUDA=ON` (NVIDIA), `-DGGML_HIP=ON` (AMD/ROCm), or
`-DGGML_VULKAN=ON` first if you want GPU offload available through
`ModelParams.NGPULayers`. This produces `libllama.so` plus `libggml.so`,
`libggml-base.so`, `libggml-cpu.so` (`.dylib` on macOS, `.dll` on Windows) in
`build/bin`.

## Use the bindings

```bash
export CGO_LDFLAGS="-L/path/to/llama.cpp/build/bin"
export LD_LIBRARY_PATH="/path/to/llama.cpp/build/bin:$LD_LIBRARY_PATH"   # Linux
export DYLD_LIBRARY_PATH="/path/to/llama.cpp/build/bin:$DYLD_LIBRARY_PATH" # macOS
```

Then, from your own Go module:

```bash
go get github.com/rhino1998/lectable/llamacpp-go
```

or, working inside this checkout, `go build ./...` / `go test ./...` pick up
`CGO_LDFLAGS`/`LD_LIBRARY_PATH` the same way any other cgo package does.

```go
package main

import (
	"fmt"
	"log"

	"github.com/rhino1998/lectable/llamacpp-go/llamacpp"
)

func main() {
	llamacpp.BackendInit()
	defer llamacpp.BackendFree()

	model, err := llamacpp.LoadModel("/path/to/qwen3-4b-instruct.gguf", llamacpp.ModelParams{
		NGPULayers: -1, // offload everything a GPU backend can take; 0 = CPU only
	})
	if err != nil {
		log.Fatal(err)
	}
	defer model.Close()

	ctx, err := model.NewContext(llamacpp.ContextParams{NCtx: 4096})
	if err != nil {
		log.Fatal(err)
	}
	defer ctx.Close()

	prompt, err := model.Vocab().ApplyChatTemplate(model.ChatTemplate(""), []llamacpp.ChatMessage{
		{Role: "user", Content: "Say hello in one short sentence."},
	}, true)
	if err != nil {
		log.Fatal(err)
	}

	sampler := llamacpp.NewSampler(llamacpp.SamplerParams{Temp: 0.7, TopK: 40, TopP: 0.9})
	defer sampler.Close()

	_, err = ctx.Generate(prompt, sampler, 128, func(piece string) bool {
		fmt.Print(piece)
		return true // keep going; return false to stop early
	})
	if err != nil {
		log.Fatal(err)
	}
}
```

`ModelParams.LoadMode` controls mmap/mlock/direct-I/O (see
`llama_load_mode` in `llama.h`); the zero value auto-detects. `SamplerParams`
with `Temp <= 0` (the zero value) builds a deterministic greedy sampler;
otherwise it chains top-k, top-p, min-p, temperature, then a seeded
distribution sample.

## What this wrapper does not do

This is a deliberately small subset of `llama.h`, not a 1:1 mirror the way
`audiocpp-go` is of `audiocpp.h` -- llama.h is llama.cpp's entire engine API
(LoRA adapters, session state save/load, embeddings extraction, model
quantization, multi-sequence batching, backend sampling, ...), most of which
a single-sequence text-generation feature has no use for. `Batch` and
`Context.Decode`/`LogitsIth` are exposed directly so a call site that needs
more of that surface can build on them without going through `Generate`.

No model download/conversion helpers -- point `LoadModel` at a `.gguf` file
you already have (see llama.cpp's `convert_hf_to_gguf.py` or download a
pre-converted GGUF).

## Testing

`llamacpp/smoke_test.go` needs libllama built and linked to run at all (even
the library-info test is a cgo call into it); the generation test
additionally needs `LLAMACPP_TEST_MODEL` pointed at a real small GGUF file,
since llama.cpp doesn't bundle a tiny test model the way audio.cpp bundles
Silero VAD:

```bash
LLAMACPP_TEST_MODEL=/path/to/small-instruct-model.gguf \
CGO_LDFLAGS="-L/path/to/llama.cpp/build/bin" \
LD_LIBRARY_PATH="/path/to/llama.cpp/build/bin" \
    go test ./... -v
```
