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
	"sync/atomic"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audioworker"
	"github.com/rhino1998/lectable/backend/internal/llmworker"
	"github.com/rhino1998/lectable/backend/internal/semantic"
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
	port := flag.String("port", envOr("LECTABLE_TTS_WORKER_PORT", "8091"), "port to listen on (loopback only)")
	flag.Parse()

	// idleUnloadAfter is shared between the two model families (TTS clone
	// models and the LLM) rather than two separately-tunable knobs - both
	// exist for the same reason (free this process's shared VRAM budget for
	// whichever family isn't actively in use) and there's no case observed
	// so far for wanting different thresholds on either side.
	idleUnloadAfter := envDurationOr("LECTABLE_MODEL_IDLE_UNLOAD_AFTER", 2*time.Minute)

	defaultCloneModel := envOr("LECTABLE_DEFAULT_CLONE_MODEL", "audiocpp-higgs-4b")

	// A speaker LLM too big to share VRAM with an audio model (Qwen3.6-
	// 35B-A3B: 16.5GB of a 24GB card) takes turns with them instead: an
	// audio model load unloads it first, and its own load unloads every
	// audio model - see llmExclusive. internal/jobs' pool switching already
	// keeps the two families' work apart; this makes the swap synchronous
	// here, so one never loads on top of the other.
	llmModelPath := speakerattr.ModelPathFromEnv()
	exclusive := llmExclusive(llmModelPath)
	var llmRef atomic.Pointer[llmworker.Worker]
	var gpuEmbedderRef atomic.Pointer[llmworker.Embedder]
	var audioBeforeLoad func()
	if exclusive {
		log.Printf("ttsworker: speaker LLM %s is exclusive - it and the audio models unload each other", llmModelPath)
		audioBeforeLoad = func() {
			if lw := llmRef.Load(); lw != nil {
				lw.Unload()
			}
			if e := gpuEmbedderRef.Load(); e != nil {
				e.Unload()
			}
		}
	}

	audioWorker, err := audioworker.New(audioworker.Config{
		DefaultCloneModel: defaultCloneModel,
		// Half of internal/jobs' own maxInFlight (8, poolGeneration's
		// concurrent-Generate-call cap, sized for Higgs batching, which
		// shares one session): calls beyond the pool wait for a session
		// - see audioworker.Config.ClonePoolSize's own doc comment. Most
		// families override it anyway (cloneFamily.poolSizeOverride).
		ClonePoolSize:   envIntOr("LECTABLE_AUDIOCPP_CLONE_POOL_SIZE", 4),
		IdleUnloadAfter: idleUnloadAfter,
		BeforeLoad:      audioBeforeLoad,
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
	llmBeforeLoad := audioWorker.UnloadCloneModels
	if exclusive {
		llmBeforeLoad = audioWorker.UnloadAll
	}
	llmWorker := llmworker.New(llmworker.Config{
		ModelPath:     llmModelPath,
		NGPULayers:    int32(envIntOr("LECTABLE_SPEAKER_LLM_GPU_LAYERS", -1)),
		NCtx:          uint32(envIntOr("LECTABLE_SPEAKER_LLM_CTX", speakerattr.SlotNCtx())),
		MaxConcurrent: envIntOr("LECTABLE_SPEAKER_LLM_MAX_CONCURRENT", 2),
		// Opt-in: see llmworker.Config.PrimeAsState/NUBatch.
		PrimeAsState: envOr("LECTABLE_SPEAKER_LLM_PRIME_STATE", "") == "true",
		// 2048 reads prompts ~35% faster than libllama's 512 (llama-bench,
		// Qwen3.6-35B-A3B on the RX 7900 XTX, 2026-09-28); 4096 adds ~1%.
		NUBatch: uint32(envIntOr("LECTABLE_SPEAKER_LLM_UBATCH", 2048)),
		// Draft length for MTP speculative decoding; 0 turns it off.
		MTPDraft:        envIntOr("LECTABLE_SPEAKER_LLM_MTP", 3),
		SystemPrompts:   speakerattr.SystemPrompts(),
		IdleUnloadAfter: idleUnloadAfter,
		// Free every resident clone model before the LLM's own (lazy,
		// first-use) load rather than letting it momentarily coexist with
		// them - see llmworker.Config.BeforeLoad's own doc comment.
		// UnloadCloneModels, not UnloadAll: leaves the shared aligner
		// resident - see audioworker.Worker.UnloadCloneModels's own doc
		// comment for why it's meant to stay one persistent instance rather
		// than being churned by an unrelated model swap.
		// An exclusive LLM frees everything instead, aligner and Stable
		// Audio included.
		BeforeLoad: llmBeforeLoad,
	})
	llmRef.Store(llmWorker)

	// The embedding model (internal/semantic), loaded twice from one file,
	// each on first use: on the GPU for the chapters a jobs.KindEmbedding
	// task embeds - LLM-pool work, so it leaves with the speaker LLM - and
	// on the CPU for a search phrase at any moment.
	embedModelPath := semantic.ModelPathFromEnv()
	gpuEmbedder := llmworker.NewEmbedder(llmworker.EmbedderConfig{
		Name:            "gpu",
		ModelPath:       embedModelPath,
		NoGPU:           envOr("LECTABLE_EMBED_GPU", "true") != "true",
		IdleUnloadAfter: idleUnloadAfter,
	})
	gpuEmbedderRef.Store(gpuEmbedder)
	cpuEmbedder := llmworker.NewEmbedder(llmworker.EmbedderConfig{
		Name:      "cpu",
		ModelPath: embedModelPath,
		NoGPU:     true,
		Threads:   int32(envIntOr("LECTABLE_EMBED_QUERY_THREADS", 4)),
		// A phrase at a time.
		MaxSeqs:         2,
		IdleUnloadAfter: 15 * time.Minute,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /generate", audioWorker.HandleGenerate)
	mux.HandleFunc("POST /design", audioWorker.HandleDesign)
	mux.HandleFunc("POST /align", audioWorker.HandleAlign)
	mux.HandleFunc("POST /transcribe", audioWorker.HandleTranscribe)
	mux.HandleFunc("POST /listen", audioWorker.HandleListen)
	mux.HandleFunc("POST /music", audioWorker.HandleMusic)
	mux.HandleFunc("POST /yue2", audioWorker.HandleYuE2)
	mux.HandleFunc("POST /stable-audio-music", audioWorker.HandleStableAudioMusic)
	mux.HandleFunc("POST /stable-audio-sfx", audioWorker.HandleStableAudioSFX)
	mux.HandleFunc("POST /stable-audio-medium", audioWorker.HandleStableAudioMedium)
	mux.HandleFunc("POST /codec-encode", audioWorker.HandleCodecEncode)
	mux.HandleFunc("POST /codec-decode", audioWorker.HandleCodecDecode)
	mux.HandleFunc("POST /unload", audioWorker.HandleUnload)
	mux.HandleFunc("POST /llm/generate", llmWorker.HandleGenerate)
	mux.HandleFunc("POST /llm/decide", llmWorker.HandleDecide)
	mux.HandleFunc("POST /llm/unload", func(rw http.ResponseWriter, r *http.Request) {
		gpuEmbedder.Unload()
		llmWorker.HandleUnload(rw, r)
	})
	mux.HandleFunc("POST /embed", llmworker.HandleEmbed(gpuEmbedder, cpuEmbedder))
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

// llmExclusiveAutoBytes is the model file size above which the speaker LLM
// is exclusive by default - see llmExclusive. Qwen3-4B (2.5GB) shares VRAM
// with the audio models fine; Qwen3.6-35B-A3B (16.5GB) can't.
const llmExclusiveAutoBytes = 8 << 30

// llmExclusive reports whether the speaker LLM at path takes turns with
// the audio models rather than sharing VRAM with them:
// LECTABLE_SPEAKER_LLM_EXCLUSIVE=true/false, or by default whether the
// model file is bigger than llmExclusiveAutoBytes.
func llmExclusive(path string) bool {
	switch envOr("LECTABLE_SPEAKER_LLM_EXCLUSIVE", "auto") {
	case "true":
		return true
	case "false":
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Size() > llmExclusiveAutoBytes
}
