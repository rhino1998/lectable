// Command server runs the lectable backend: library management, epub
// parsing, TTS orchestration, and the HTTP API consumed by the frontend.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/rhino1998/lectable/backend/internal/httpapi"
	"github.com/rhino1998/lectable/backend/internal/instanceid"
	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/live"
	"github.com/rhino1998/lectable/backend/internal/mdnsadvert"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// homePath joins elem onto the current user's home directory, falling
// back to a path relative to the working directory if it can't be found.
func homePath(elem ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(elem...)
	}
	return filepath.Join(append([]string{home}, elem...)...)
}

func main() {
	portStr := getenv("PORT", "8080")
	addr := ":" + portStr
	dataDir := getenv("DATA_DIR", "./data")
	allowOrigin := getenv("ALLOW_ORIGIN", "http://localhost:5173")
	// Purely a human-readable label (e.g. "Home", "Office") for a client juggling more than one
	// backend to tell them apart by - see internal/instanceid's InstanceID for the actual stable
	// identity clients scope cached data by; this is never used for that. Empty by default since
	// most setups only ever talk to one backend and don't need to name it.
	libraryName := getenv("LIBRARY_NAME", "")

	// ttsworker.Manager spawns and watchdog-restarts a separate ttsworker
	// process (the only thing in this app that links against
	// libaudiocpp.so - see backend/CLAUDE.md) rather than talking to a
	// separate Python service; TTS_WORKER_BIN/AUDIOCPP_LIB_DIR replace the
	// old TTS_SERVICE_URL.
	ttsWorkerBin := getenv("TTS_WORKER_BIN", "./ttsworker")
	audiocppLibDir := getenv("AUDIOCPP_LIB_DIR", homePath("audio.cpp", "build", "bin"))
	ttsWorkerPort := 0
	if v := os.Getenv("TTS_WORKER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			ttsWorkerPort = p
		} else {
			log.Printf("TTS_WORKER_PORT %q is not numeric, using default", v)
		}
	}

	for _, sub := range []string{"", "audio", "covers"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o755); err != nil {
			log.Fatalf("create data dir: %v", err)
		}
	}

	duck, err := store.Open(filepath.Join(dataDir, "library.duckdb"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer duck.Close()
	// Everything reads through the cache (invalidated by duck's own write
	// notifications) - the single DuckDB connection is the bottleneck.
	cached := store.NewCached(duck)
	var st store.Store = cached

	instanceID, err := instanceid.LoadOrCreate(dataDir)
	if err != nil {
		log.Fatalf("load/create instance id: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ttsMgr := ttsworker.New(ttsworker.Config{
		BinPath:           ttsWorkerBin,
		Port:              ttsWorkerPort,
		LibDir:            audiocppLibDir,
		DefaultCloneModel: voices.DefaultCloneModel,
	})
	if err := ttsMgr.Start(ctx); err != nil {
		log.Fatalf("start ttsworker: %v", err)
	}

	go cached.LogStatsEvery(time.Minute, ctx.Done())

	jobManager := jobs.NewManager(st, ttsMgr, dataDir)
	jobManager.Start(ctx)

	// Live state push (GET /api/events): every committed store write and
	// every job-queue change invalidates the topics depending on it - see
	// package live and httpapi.registerLiveTopics. The periodic resync is a
	// safety net for anything that changes without either signal.
	liveHub := live.New(live.Config{Resync: 30 * time.Second})
	st.OnChange(func(tables []string) {
		deps := make([]string, len(tables))
		for i, t := range tables {
			deps[i] = httpapi.DBDep(t)
		}
		liveHub.Invalidate(deps...)
	})
	jobChanges := jobManager.SubscribeChanges()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-jobChanges:
				liveHub.Invalidate(httpapi.DepJobs)
			}
		}
	}()
	go liveHub.Run(ctx)

	// speakerClient is nil (and speaker attribution's endpoint reports 503)
	// unless the model file (SPEAKER_LLM_MODEL_PATH, or
	// speakerattr.DefaultModelPath) exists - see internal/speakerattr. The
	// GGUF model itself is loaded and run inside the ttsworker process (see
	// internal/llmworker and backend/CLAUDE.md's "ttsworker / audioworker"
	// section) - speakerClient just proxies to it over the same loopback
	// connection ttsMgr already maintains for TTS calls.
	speakerClient := speakerattr.NewClientFromEnv(ttsMgr)
	if speakerClient == nil {
		log.Printf("speaker attribution: model %s not found (set SPEAKER_LLM_MODEL_PATH), disabled", speakerattr.ModelPathFromEnv())
	} else {
		defer speakerClient.Close()
	}

	server := &httpapi.Server{
		Store:       st,
		TTS:         ttsMgr,
		Jobs:        jobManager,
		DataDir:     dataDir,
		AllowOrigin: allowOrigin,
		Live:        liveHub,
		Narration:   narration.NewResolver(st),
		Speaker:     speakerClient,
		InstanceID:  instanceID,
		LibraryName: libraryName,
	}
	handler := httpapi.NewRouter(server)

	httpServer := &http.Server{Addr: addr, Handler: handler}

	var mdnsServer *mdnsadvert.Server
	if port, err := strconv.Atoi(portStr); err != nil {
		log.Printf("mdns: PORT %q is not numeric, skipping LAN advertisement: %v", portStr, err)
	} else if mdnsServer, err = mdnsadvert.Start(port); err != nil {
		log.Printf("mdns: failed to advertise on LAN, falling back to manual discovery: %v", err)
	} else {
		log.Printf("mdns: advertising as %s on port %d", mdnsadvert.ServiceType, port)
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		log.Println("shutting down...")
		if mdnsServer != nil {
			_ = mdnsServer.Shutdown()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		ttsMgr.Stop()
	}()

	log.Printf("lectable backend listening on %s (data dir: %s, ttsworker: %s, instance: %s)", addr, dataDir, ttsWorkerBin, instanceID)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
	// ListenAndServe unblocks as soon as httpServer.Shutdown is called, which
	// happens partway through the goroutine above - without waiting here,
	// main returns and the process exits before ttsMgr.Stop() runs, orphaning
	// the ttsworker child instead of terminating it.
	<-shutdownDone
}
