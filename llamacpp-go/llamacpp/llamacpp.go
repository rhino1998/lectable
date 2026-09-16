// Package llamacpp is a cgo wrapper around llama.cpp's public C API
// (llama.h, vendored together with the ggml/gguf headers it depends on in
// this package's include/ directory -- see this module's README for exactly
// which upstream files and why). It embeds llama.cpp in-process: a loaded
// model and its context live for as long as the calling Go program keeps
// them, with no subprocess and no server in between.
//
// Unlike audio.cpp's audiocpp.h (a small, hand-written C ABI purpose-built
// for bindings), llama.h is llama.cpp's own full engine API, so this
// wrapper is deliberately not 1:1: it exposes a small, idiomatic subset --
// load a GGUF model, build a prompt (optionally through a chat template),
// and run a token-by-token generation loop with a configurable sampler
// chain -- rather than every function llama.h declares (no LoRA adapters,
// no session/state save-load, no embeddings extraction, no quantization).
// Add to it as real call sites need more of the underlying API.
//
// Quick start:
//
//	llamacpp.BackendInit()
//	defer llamacpp.BackendFree()
//
//	model, err := llamacpp.LoadModel("/path/to/qwen3-4b-instruct.gguf", llamacpp.ModelParams{NGPULayers: -1})
//	if err != nil { ... }
//	defer model.Close()
//
//	ctx, err := model.NewContext(llamacpp.ContextParams{NCtx: 4096})
//	if err != nil { ... }
//	defer ctx.Close()
//
//	sampler := llamacpp.NewSampler(llamacpp.SamplerParams{Temp: 0.7, TopK: 40, TopP: 0.9, Seed: 42})
//	defer sampler.Close()
//
//	prompt := model.Vocab().ApplyChatTemplate("", []llamacpp.ChatMessage{
//		{Role: "user", Content: "Say hello in one short sentence."},
//	}, true)
//
//	out, err := ctx.Generate(prompt, sampler, 128, func(piece string) bool {
//		fmt.Print(piece)
//		return true // keep going; return false to stop early
//	})
//
// # Building and linking
//
// llama.cpp is not vendored here beyond its headers -- build it separately
// (CMake, shared libs) and point cgo/the dynamic loader at that build's lib
// directory before building or running anything that imports this package;
// see README.md for the exact CMake invocation and the CGO_LDFLAGS /
// LD_LIBRARY_PATH exports it needs.
package llamacpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -lllama -lggml -lggml-base -lggml-cpu
#include <stdlib.h>
#include <llama.h>
*/
import "C"

// BackendInit initializes llama.cpp's ggml backend. Call it once near the
// start of the program, before loading any model.
func BackendInit() {
	C.llama_backend_init()
}

// BackendFree releases the ggml backend. Call it once, after every Model and
// Context has been closed.
func BackendFree() {
	C.llama_backend_free()
}

// Version returns llama.cpp's own build version string (e.g. a git describe
// or release tag, as baked in at compile time by the linked libllama).
func Version() string {
	return C.GoString(C.llama_version())
}

// PrintSystemInfo returns llama.cpp's one-line summary of the compiled
// backends and CPU features in use (the same line llama.cpp's own CLI tools
// print at startup) -- useful for confirming GPU offload actually took.
func PrintSystemInfo() string {
	return C.GoString(C.llama_print_system_info())
}

// SupportsGPUOffload reports whether this build of libllama was compiled
// with a GPU backend (CUDA, HIP, Vulkan, Metal, ...).
func SupportsGPUOffload() bool {
	return bool(C.llama_supports_gpu_offload())
}
