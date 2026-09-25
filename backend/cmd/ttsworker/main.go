// Command ttsworker is the only binary in this repo that links against
// libaudiocpp.so (via audiocpp-go) or libllama.so (via llamacpp-go). It is
// spawned, health-checked, and restarted by cmd/server's internal/ttsworker
// client package, which never itself imports either binding - see
// backend/CLAUDE.md for why that separation exists: a confirmed native
// memory leak inside audio.cpp's own code that this repo can't fix
// upstream, contained by making the process that holds it disposable.
// llama.cpp carries none of that leak, but its model lives here too (see
// internal/llmworker) so both native model families share one process's
// VRAM budget and can be idle-unloaded against each other - see
// internal/audioworker's and internal/llmworker's own Config.
// IdleUnloadAfter doc comments.
//
// Build/run: needs a local audio.cpp checkout with the C API built AND a
// local llama.cpp checkout, with CGO_LDFLAGS/LD_LIBRARY_PATH pointed at
// both - see audiocpp-go/README.md and llamacpp-go/README.md.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audioworker"
	"github.com/rhino1998/lectable/backend/internal/llmworker"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDurationOr(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("ttsworker: %s=%q is not a valid duration, using default %s", key, v, def)
		return def
	}
	return d
}

func envIntOr(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("ttsworker: %s=%q is not a valid int, using default %d", key, v, def)
		return def
	}
	return n
}

func main() {
	port := flag.String("port", envOr("TTS_WORKER_PORT", "8091"), "port to listen on (loopback only)")
	flag.Parse()

	// idleUnloadAfter is shared between the two model families (TTS clone
	// models and the LLM) rather than two separately-tunable knobs - both
	// exist for the same reason (free this process's shared VRAM budget for
	// whichever family isn't actively in use) and there's no case observed
	// so far for wanting different thresholds on either side.
	idleUnloadAfter := envDurationOr("MODEL_IDLE_UNLOAD_AFTER", 2*time.Minute)

	defaultCloneModel := envOr("QWEN_TTS_DEFAULT_CLONE_MODEL", "audiocpp-higgs-4b")

	audioWorker, err := audioworker.New(audioworker.Config{
		DefaultCloneModel: defaultCloneModel,
		// Matches internal/jobs' own maxInFlight (4, poolGeneration's
		// concurrent-Generate-call cap) by default - see audioworker.
		// Config.ClonePoolSize's own doc comment for why a smaller pool
		// just reintroduces today's queueing, only less of it.
		ClonePoolSize:   envIntOr("QWEN_TTS_AUDIOCPP_CLONE_POOL_SIZE", 4),
		IdleUnloadAfter: idleUnloadAfter,
	})
	if err != nil {
		log.Fatalf("ttsworker: failed to start: %v", err)
	}

	// llmWorker's own model loads lazily on first use (the model file may
	// legitimately be missing - speaker attribution is an optional
	// feature, see backend/CLAUDE.md's "Speaker attribution" section, and
	// cmd/server disables it when the file isn't there), so a missing
	// model here is not a startup error the way a bad defaultCloneModel
	// above is.
	llmWorker := llmworker.New(llmworker.Config{
		ModelPath:       speakerattr.ModelPathFromEnv(),
		NGPULayers:      int32(envIntOr("SPEAKER_LLM_GPU_LAYERS", -1)),
		NCtx:            uint32(envIntOr("SPEAKER_LLM_CTX", speakerattr.SlotNCtx())),
		MaxConcurrent:   envIntOr("SPEAKER_LLM_MAX_CONCURRENT", 2),
		SystemPrompts:   speakerattr.SystemPrompts(),
		IdleUnloadAfter: idleUnloadAfter,
		// Free every resident clone model before the LLM's own (lazy,
		// first-use) load rather than letting it momentarily coexist with
		// them - see llmworker.Config.BeforeLoad's own doc comment.
		// UnloadCloneModels, not UnloadAll: leaves the shared aligner
		// resident - see audioworker.Worker.UnloadCloneModels's own doc
		// comment for why it's meant to stay one persistent instance rather
		// than being churned by an unrelated model swap.
		BeforeLoad: audioWorker.UnloadCloneModels,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /generate", audioWorker.HandleGenerate)
	mux.HandleFunc("POST /design", audioWorker.HandleDesign)
	mux.HandleFunc("POST /align", audioWorker.HandleAlign)
	mux.HandleFunc("POST /transcribe", audioWorker.HandleTranscribe)
	mux.HandleFunc("POST /music", audioWorker.HandleMusic)
	mux.HandleFunc("POST /stable-audio-music", audioWorker.HandleStableAudioMusic)
	mux.HandleFunc("POST /stable-audio-sfx", audioWorker.HandleStableAudioSFX)
	mux.HandleFunc("POST /stable-audio-medium", audioWorker.HandleStableAudioMedium)
	mux.HandleFunc("POST /unload", audioWorker.HandleUnload)
	mux.HandleFunc("POST /llm/generate", llmWorker.HandleGenerate)
	mux.HandleFunc("POST /llm/unload", llmWorker.HandleUnload)
	mux.HandleFunc("GET /health", func(rw http.ResponseWriter, r *http.Request) {
		// llmWorker.Loaded() is read fresh on every request (not captured
		// once at registration time) so /health always reflects current
		// residency, which idle-unload can change between requests.
		audioWorker.HandleHealth(llmWorker.Loaded())(rw, r)
	})

	addr := "127.0.0.1:" + *port
	log.Printf("ttsworker listening on %s (defaultCloneModel=%s, idleUnloadAfter=%s)", addr, defaultCloneModel, idleUnloadAfter)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
