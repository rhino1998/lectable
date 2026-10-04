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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rhino1998/lectable/backend/internal/audiomaint"
	"github.com/rhino1998/lectable/backend/internal/bookexport"
	"github.com/rhino1998/lectable/backend/internal/dbbackup"
	"github.com/rhino1998/lectable/backend/internal/httpapi"
	"github.com/rhino1998/lectable/backend/internal/instanceid"
	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/live"
	"github.com/rhino1998/lectable/backend/internal/mdnsadvert"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
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
	portStr := getenv("LECTABLE_PORT", "8080")
	addr := ":" + portStr
	dataDir := getenv("LECTABLE_DATA_DIR", "./data")
	allowOrigin := getenv("LECTABLE_ALLOW_ORIGIN", "http://localhost:5173")
	// Purely a human-readable label (e.g. "Home", "Office") for a client juggling more than one
	// backend to tell them apart by - see internal/instanceid's InstanceID for the actual stable
	// identity clients scope cached data by; this is never used for that. Empty by default since
	// most setups only ever talk to one backend and don't need to name it.
	libraryName := getenv("LECTABLE_LIBRARY_NAME", "")

	// ttsworker.Manager spawns and watchdog-restarts a separate ttsworker
	// process (the only thing in this app that links against
	// libaudiocpp.so - see backend/CLAUDE.md) rather than talking to a
	// separate Python service; LECTABLE_TTS_WORKER_BIN/LECTABLE_AUDIOCPP_LIB_DIR replace the
	// old TTS_SERVICE_URL.
	ttsWorkerBin := getenv("LECTABLE_TTS_WORKER_BIN", "./ttsworker")
	audiocppLibDir := getenv("LECTABLE_AUDIOCPP_LIB_DIR", homePath("audio.cpp", "build-rocm7", "bin"))
	ttsWorkerPort := 0
	if v := os.Getenv("LECTABLE_TTS_WORKER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			ttsWorkerPort = p
		} else {
			log.Printf("LECTABLE_TTS_WORKER_PORT %q is not numeric, using default", v)
		}
	}

	for _, sub := range []string{"", "audio", "covers"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o755); err != nil {
			log.Fatalf("create data dir: %v", err)
		}
	}

	backupDir := filepath.Join(dataDir, "backups")
	// A schema migration is preceded by a copy of the library, kept (and
	// pruned) with the scheduled ones.
	lib, err := store.OpenWith(filepath.Join(dataDir, "library.sqlite"),
		store.Options{BackupBeforeMigrate: dbbackup.Path(backupDir, time.Now())})
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer lib.Close()
	// Everything reads through the cache (invalidated by lib's own write
	// notifications), sparing the hot lookups a query each.
	cached := store.NewCached(lib)
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
		RefCodesDir:       filepath.Join(dataDir, "voice-refs", "cache", "refcodes"),
	})
	if err := ttsMgr.Start(ctx); err != nil {
		log.Fatalf("start ttsworker: %v", err)
	}

	go cached.LogStatsEvery(time.Minute, ctx.Done())

	// Rolling copies of library.sqlite under LECTABLE_DATA_DIR/backups -
	// see internal/dbbackup.
	backupInterval := 24 * time.Hour
	if v := os.Getenv("LECTABLE_BACKUP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			backupInterval = d
		} else {
			log.Printf("LECTABLE_BACKUP_INTERVAL %q is not a duration, using %s", v, backupInterval)
		}
	}
	backupKeep := 7
	if v := os.Getenv("LECTABLE_BACKUP_KEEP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			backupKeep = n
		} else {
			log.Printf("LECTABLE_BACKUP_KEEP %q is not numeric, using %d", v, backupKeep)
		}
	}
	go dbbackup.Run(ctx, lib, dbbackup.Config{Dir: backupDir, Interval: backupInterval, Keep: backupKeep})

	jobManager := jobs.NewManager(st, ttsMgr, dataDir)
	if err := jobManager.RegisterMetrics(prometheus.DefaultRegisterer); err != nil {
		log.Fatalf("register job metrics: %v", err)
	}
	jobManager.Start(ctx)

	// Converts clips from before the switch to Ogg Opus and deletes audio
	// the library no longer refers to - once now, then sweeping daily.
	go audiomaint.Run(ctx, dataDir, st)
	// Levels reference clips saved before normalization (or restored from
	// an export) to voicerefs.TargetLUFS; a no-op once they all are.
	go func() {
		if s := voicerefs.NormalizeAll(ctx, dataDir); s.Normalized+s.Failed+len(s.Rebounded) > 0 {
			log.Printf("voicerefs: normalized %d reference clips to %.0f LUFS (%d variants rescaled), %d variants pulled into their loudness band, %d failed",
				s.Normalized, voicerefs.TargetLUFS, s.Variants, len(s.Rebounded), s.Failed)
		}
	}()
	go audiomaint.MigrateSeedLatents(ctx, dataDir, func(ctx context.Context, wav []byte) (*ttsproto.Latents, error) {
		return ttsMgr.CodecEncode(ctx, "stable_audio_medium", wav)
	}, ttsMgr.Idle)

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
	// unless the model file (LECTABLE_SPEAKER_LLM_MODEL_PATH, or
	// speakerattr.DefaultModelPath) exists - see internal/speakerattr. The
	// GGUF model itself is loaded and run inside the ttsworker process (see
	// internal/llmworker and backend/CLAUDE.md's "ttsworker / audioworker"
	// section) - speakerClient just proxies to it over the same loopback
	// connection ttsMgr already maintains for TTS calls.
	speakerClient := speakerattr.NewClientFromEnv(ttsMgr)
	if speakerClient == nil {
		log.Printf("speaker attribution: model %s not found (set LECTABLE_SPEAKER_LLM_MODEL_PATH), disabled", speakerattr.ModelPathFromEnv())
	} else {
		defer speakerClient.Close()
	}

	narrationResolver := narration.NewResolver(st)
	// Book exports build in the background and are kept under
	// LECTABLE_DATA_DIR/exports; build state changes reach clients via the
	// bookExports live topic.
	exports := bookexport.NewManager(bookexport.Source{Store: st, Voices: narrationResolver, DataDir: dataDir},
		func() { liveHub.Invalidate(httpapi.DepExports) })

	server := &httpapi.Server{
		Store:       st,
		TTS:         ttsMgr,
		Jobs:        jobManager,
		DataDir:     dataDir,
		AllowOrigin: allowOrigin,
		Live:        liveHub,
		Exports:     exports,
		Narration:   narrationResolver,
		Speaker:     speakerClient,
		InstanceID:  instanceID,
		LibraryName: libraryName,
	}
	handler := httpapi.NewRouter(server)

	// Only header and keep-alive idle timeouts: a ReadTimeout would cut off
	// multi-gigabyte export uploads, and a WriteTimeout long clip/export
	// downloads (the /api/events WebSocket is hijacked, so neither applies
	// to it anyway).
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	var mdnsServer *mdnsadvert.Server
	if port, err := strconv.Atoi(portStr); err != nil {
		log.Printf("mdns: LECTABLE_PORT %q is not numeric, skipping LAN advertisement: %v", portStr, err)
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
