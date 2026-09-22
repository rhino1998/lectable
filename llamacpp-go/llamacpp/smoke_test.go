package llamacpp_test

// Smoke test for the Go bindings. Unlike audiocpp-go's smoke test (which
// runs Silero VAD, a model bundled in every audio.cpp checkout), llama.cpp
// doesn't ship a tiny test model in its own repo, so the generation test
// here needs a real GGUF file pointed at by LLAMACPP_TEST_MODEL -- any small
// instruct-tuned GGUF works (a few hundred MB quantized Qwen model is
// enough); it's skipped otherwise.
//
// Every test in this file still needs libllama (and its ggml companions)
// built and linked, since even the library-info calls are cgo calls into
// it -- see this module's README for the CMake invocation.
//
//	cd /path/to/llama.cpp
//	cmake -S . -B build -DBUILD_SHARED_LIBS=ON
//	cmake --build build --target llama
//	cd /path/to/lectable/llamacpp-go
//	LLAMACPP_TEST_MODEL=/path/to/qwen2.5-0.5b-instruct-q4_k_m.gguf \
//	CGO_LDFLAGS="-L/path/to/llama.cpp/build/bin" \
//	LD_LIBRARY_PATH="/path/to/llama.cpp/build/bin" \
//	    go test ./llamacpp/...

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rhino1998/lectable/llamacpp-go/llamacpp"
)

var backendOnce sync.Once

func initBackend() {
	backendOnce.Do(llamacpp.BackendInit)
}

func testModelPath(t *testing.T) string {
	t.Helper()
	path := os.Getenv("LLAMACPP_TEST_MODEL")
	if path == "" {
		t.Skip("LLAMACPP_TEST_MODEL not set; point it at a small local GGUF file to run this test (see smoke_test.go)")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("LLAMACPP_TEST_MODEL=%s: %v", path, err)
	}
	return path
}

func TestBackendInfo(t *testing.T) {
	initBackend()
	if llamacpp.PrintSystemInfo() == "" {
		t.Error("PrintSystemInfo() is empty")
	}
}

func TestGenerateProducesText(t *testing.T) {
	initBackend()
	modelPath := testModelPath(t)

	model, err := llamacpp.LoadModel(modelPath, llamacpp.ModelParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	if model.NCtxTrain() <= 0 {
		t.Errorf("model.NCtxTrain() = %d, want > 0", model.NCtxTrain())
	}

	ctx, err := model.NewContext(llamacpp.ContextParams{NCtx: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	prompt, err := model.Vocab().ApplyChatTemplate(model.ChatTemplate(""), []llamacpp.ChatMessage{
		{Role: "user", Content: "Reply with exactly the word: hello"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if prompt == "" {
		t.Fatal("ApplyChatTemplate returned an empty prompt")
	}

	sampler := llamacpp.NewSampler(llamacpp.SamplerParams{}) // Temp 0 => greedy, deterministic
	defer sampler.Close()

	var pieces []string
	out, err := ctx.Generate(prompt, sampler, 16, func(piece string) bool {
		pieces = append(pieces, piece)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("Generate returned empty text")
	}
	if strings.Join(pieces, "") != out {
		t.Errorf("onPiece pieces joined = %q, want equal to returned text %q", strings.Join(pieces, ""), out)
	}
}
