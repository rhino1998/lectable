// Package ttsworker spawns, health-checks, and watchdog-restarts the
// ttsworker binary (cmd/ttsworker) - the only process in this app that
// links against libaudiocpp.so. This package itself has no dependency on
// audiocpp-go and must stay that way: cmd/server (the process that imports
// this package) must never transitively link libaudiocpp.so, so that a
// confirmed native memory leak inside audio.cpp's own code (found via
// heaptrack against a real workload; not fixable from this repo - see
// backend/CLAUDE.md) can be contained by killing and relaunching just the
// worker, without ever taking down the always-up backend process.
//
// A go list -deps ./cmd/server check that audiocpp-go never appears is the
// build-time guardrail for this invariant - see backend/CLAUDE.md.
package ttsworker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// Config controls how the worker process is spawned and watched.
type Config struct {
	// BinPath is the ttsworker binary's path.
	BinPath string
	// Port is the loopback port the worker listens on.
	Port int
	// LibDir is set as LD_LIBRARY_PATH for the worker process, so it can
	// find libaudiocpp.so and its own transitive ROCm/HIP dependencies at
	// runtime (not just at link time) - explicit rather than relying on an
	// rpath baked into the binary, so a misconfiguration surfaces as a
	// loud "worker failed to start" instead of a silent dynamic-linker
	// mismatch.
	LibDir string
	// DefaultCloneModel is passed to the worker via env.
	DefaultCloneModel string
	// RSSLimitBytes is the resident-set-size threshold that triggers a
	// proactive watchdog restart. Real OOM kills of the equivalent Python
	// process were observed at ~19GB then ~26GB RSS on a 30GB box; this
	// default leaves real margin while still restarting well before any
	// actual danger.
	RSSLimitBytes int64
	// CheckInterval is how often the watchdog samples the worker's RSS (and,
	// since StuckJobTimeout below, how the worker's own busy-time).
	CheckInterval time.Duration
	// StuckJobTimeout is how long a real job (Generate/Design/Align/
	// LLMGenerate - see Manager.beginJob/endJob) can stay continuously in
	// flight before the watchdog hard-restarts the worker - see
	// watchdogLoop's own doc comment for why this needs a genuinely
	// different restart path than the RSS/process-died triggers above.
	StuckJobTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.Port == 0 {
		c.Port = 8091
	}
	if c.RSSLimitBytes == 0 {
		c.RSSLimitBytes = 11 * 1024 * 1024 * 1024 // 11 GiB
	}
	if c.CheckInterval == 0 {
		c.CheckInterval = 15 * time.Second
	}
	if c.StuckJobTimeout == 0 {
		c.StuckJobTimeout = 5 * time.Minute
	}
	return c
}

// Manager owns the worker subprocess's lifecycle: spawning it, restarting
// it (on crash or when its RSS crosses the configured threshold), and
// gating every proxied call so a restart never happens mid-request.
//
// restartGate is a sync.RWMutex, not a literal port of tts-service's single
// asyncio.Lock: every proxied call (Generate/Design/Align) takes an RLock
// for its full HTTP round trip (many concurrent "readers" = normal
// traffic), while a restart takes the exclusive Lock (one rare "writer" =
// maintenance) - which naturally blocks until every in-flight call has
// returned, and blocks new calls until the restart finishes. No separate
// queue-depth logic needed.
type Manager struct {
	cfg Config

	restartGate sync.RWMutex

	mu      sync.Mutex // guards cmd/pid below
	cmd     *exec.Cmd
	pid     int
	baseURL string

	// jobMu guards nextJobID/jobStarts below - deliberately separate from mu
	// (held for a while during an actual restart/respawn, including up to
	// spawnAndWaitLocked's own multi-minute health-check poll) so ordinary
	// per-call job bookkeeping never contends with that.
	jobMu     sync.Mutex
	nextJobID int64
	jobStarts map[int64]time.Time // one entry per currently in-flight call, keyed by beginJob's own id

	httpClient *http.Client
}

// New creates a Manager. Call Start to actually spawn the worker.
func New(cfg Config) *Manager {
	cfg = cfg.withDefaults()
	return &Manager{
		cfg:     cfg,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", cfg.Port),
		// httpClient's own Timeout must stay comfortably above
		// cfg.StuckJobTimeout, not just "long enough" - if it ever fired
		// first, the Go-side call would simply give up and return an
		// error, which (same as any other error return) clears its own
		// share of inFlight/busySince via endJob, silently erasing
		// watchdogLoop's only signal that the *worker process itself* -
		// not just this one HTTP call - is still stuck. A client-side
		// timeout only ever stops this process from waiting; it can't
		// interrupt audiocpp_session_run on the other side of the cgo
		// boundary, so the worker would carry on grinding forever with no
		// watchdog left to notice. 4x StuckJobTimeout's own default (5m)
		// leaves real margin against CheckInterval's own polling
		// granularity and hardRestart's up-to-10s kill-wait, while still
		// bounding a genuinely wedged HTTP connection (as opposed to a
		// merely slow one) eventually.
		httpClient: &http.Client{Timeout: 4 * cfg.StuckJobTimeout},
	}
}

// Start spawns the worker, waits for it to report healthy, and launches the
// watchdog loop (stopped when ctx is done).
func (m *Manager) Start(ctx context.Context) error {
	if err := m.spawnAndWait(ctx); err != nil {
		return err
	}
	m.registerProcessCollector()
	go m.watchdogLoop(ctx)
	return nil
}

// Stop terminates the worker process (SIGTERM, then SIGKILL after a grace
// period). Call this on backend shutdown - the worker is a plain child
// process, not a process-group member, so it would otherwise be orphaned
// (left running) rather than exiting when the main process does.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.cmd.Process == nil {
		return
	}
	_ = m.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = m.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = m.cmd.Process.Kill()
		<-done
	}
}

func (m *Manager) spawnAndWait(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spawnAndWaitLocked(ctx)
}

// workerOMPThreads caps OpenMP teams in the worker (OMP_NUM_THREADS, unless
// already set). audio.cpp's CPU-side preprocessing - the aligner's and
// Parakeet's mel spectrograms, Higgs's delay pattern and reference
// resampling - runs in OpenMP parallel regions that ignore the session's
// thread count and default to one thread per core. LLVM's libomp also keeps
// a separate team per calling OS thread, and cgo calls land on whichever
// thread Go picks, so a 32-core box accumulated ~23 teams of 31 (~780
// threads), each woken from sleep for every short region: ~17 cores,
// mostly kernel time, measured while generating. Matches audioworker's
// sessionThreads.
const workerOMPThreads = 4

// spawnAndWaitLocked starts the worker process and blocks until it answers
// GET /health, or startupTimeout elapses. Caller must hold m.mu.
func (m *Manager) spawnAndWaitLocked(ctx context.Context) error {
	cmd := exec.Command(m.cfg.BinPath, "-port", strconv.Itoa(m.cfg.Port))
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+m.cfg.LibDir+":"+os.Getenv("LD_LIBRARY_PATH"))
	if m.cfg.DefaultCloneModel != "" {
		cmd.Env = append(cmd.Env, "QWEN_TTS_DEFAULT_CLONE_MODEL="+m.cfg.DefaultCloneModel)
	}
	if _, set := os.LookupEnv("OMP_NUM_THREADS"); !set {
		cmd.Env = append(cmd.Env, "OMP_NUM_THREADS="+strconv.Itoa(workerOMPThreads))
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ttsworker (%s): %w", m.cfg.BinPath, err)
	}
	m.cmd = cmd
	m.pid = cmd.Process.Pid

	// Reap the process asynchronously so it never becomes a zombie; also
	// lets us notice an unexpected exit between watchdog ticks.
	go func() {
		_ = cmd.Wait()
	}()

	// Selects on ctx.Done() every poll, not just a plain time.Sleep loop -
	// confirmed live as a real hang: ttsMgr.Start(ctx) runs synchronously
	// in main() before the SIGTERM/SIGINT-triggered shutdown goroutine
	// even exists (see cmd/server/main.go - that goroutine is only spawned
	// after Start returns), so a signal arriving while this loop is still
	// polling had nothing listening for it and the process didn't
	// actually exit until the full startupTimeout below elapsed on its
	// own - up to 3 minutes of apparent unresponsiveness to both HTTP and
	// SIGTERM. Returning promptly on ctx cancellation here fixes that
	// regardless of why a given startup's health checks are slow to
	// succeed in the first place.
	const startupTimeout = 3 * time.Minute
	deadline := time.NewTimer(startupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if m.isHealthy(ctx) {
			log.Printf("ttsworker: healthy (pid %d, port %d)", m.pid, m.cfg.Port)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ttsworker startup canceled: %w", ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("ttsworker did not become healthy within %s", startupTimeout)
		case <-ticker.C:
		}
	}
}

func (m *Manager) isHealthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Restart forces an immediate worker restart, the same way the watchdog's
// own RSS-threshold/process-died triggers do - exported for a caller that
// wants to trigger one manually (the Jobs dashboard's "Restart ttsworker"
// button, `POST /api/jobs/restart-worker`), e.g. to recover from a stuck or
// visibly leaking worker without waiting for RSSLimitBytes to be crossed.
func (m *Manager) Restart(ctx context.Context, reason string) error {
	restarts.WithLabelValues("manual").Inc()
	return m.restart(ctx, reason)
}

// restart kills the current worker process (SIGTERM, then SIGKILL after a
// grace period) and spawns a fresh one, blocked behind the exclusive
// restartGate so no proxied call is ever mid-flight when this runs.
func (m *Manager) restart(ctx context.Context, reason string) error {
	m.restartGate.Lock()
	defer m.restartGate.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	log.Printf("ttsworker: restarting (%s)", reason)
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			_ = m.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			log.Printf("ttsworker: did not exit after SIGTERM, sending SIGKILL")
			_ = m.cmd.Process.Kill()
			<-done
		}
	}
	return m.spawnAndWaitLocked(ctx)
}

// beginJob marks one more real (Generate/Design/Align/LLMGenerate) call as
// in flight, recording its own start time under a fresh id, and returns
// that id for the matching endJob call. Per-job, not one shared clock for
// the whole worker: an earlier version reset a single "continuously busy
// since" timestamp only when inFlight dropped all the way to 0, which
// answers "has the worker been unbroken-busy this long" rather than "is
// any single call stuck this long" - those two only coincide when the
// worker is lightly loaded. Under a large backlog with several concurrent
// slots (jobs.Manager's poolLLM, llmworker.Config.MaxConcurrent), inFlight
// can stay above 0 almost continuously for a whole multi-hour bulk run
// purely from healthy back-to-back calls, none individually slow -
// confirmed in production as a false-positive hard-restart roughly every
// StuckJobTimeout for the run's entire duration, tearing down perfectly
// healthy in-flight calls (which then just get retried) instead of only
// ever firing for a genuinely wedged one. Tracking each job's own start
// time and keying "stuck" off the oldest still-running one (stuckSince)
// answers the right question: idle time between bursts, or many fast
// overlapping calls, never accumulates against any one job.
func (m *Manager) beginJob() int64 {
	m.jobMu.Lock()
	defer m.jobMu.Unlock()
	m.nextJobID++
	id := m.nextJobID
	if m.jobStarts == nil {
		m.jobStarts = make(map[int64]time.Time)
	}
	m.jobStarts[id] = time.Now()
	return id
}

// endJob is beginJob's deferred counterpart, called exactly once per
// beginJob (with the id it returned) on every return path - including a
// job whose HTTP round trip just failed because hardRestart killed the
// worker out from under it, so a stuck call clears its own entry the
// moment it's forced to fail, the same as an ordinary error return.
func (m *Manager) endJob(id int64) {
	m.jobMu.Lock()
	defer m.jobMu.Unlock()
	delete(m.jobStarts, id)
}

// stuckSince reports how long the oldest currently in-flight job has been
// running (busyFor), and whether any job is in flight right now at all
// (anyInFlight false makes busyFor meaningless, not zero-ish). The oldest
// job is the only one that matters here: if it hasn't finished within
// watchdogLoop's own StuckJobTimeout, it's the stuck one, regardless of how
// many other, newer jobs are also currently in flight or have come and
// gone around it.
func (m *Manager) stuckSince() (busyFor time.Duration, anyInFlight bool) {
	m.jobMu.Lock()
	defer m.jobMu.Unlock()
	var oldest time.Time
	for _, t := range m.jobStarts {
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	if oldest.IsZero() {
		return 0, false
	}
	return time.Since(oldest), true
}

// hardRestart forcibly kills the worker process and respawns it, for a job
// that's been stuck in flight longer than cfg.StuckJobTimeout - a
// genuinely different path from restart() above, not just a shorter-grace-
// period variant of it. restart() takes the exclusive restartGate.Lock()
// before ever touching the process, which is fine when every in-flight
// call is going to finish on its own soon (the normal RSS/process-died
// triggers) but deadlocks outright against a call stuck long enough to
// trigger *this* path: that call is still holding an RLock that will never
// release on its own, so restartGate.Lock() would block forever right at
// the first line, before the process is ever even signaled.
//
// The fix is ordering: kill the process first, while holding only mu (not
// restartGate), so the stuck call's own HTTP round trip fails promptly (a
// broken connection, not a clean response) and its deferred RUnlock (and
// endJob, clearing busySince) fires the same as any other request error -
// only *then* is taking the exclusive Lock actually safe to wait on, the
// same way restart() itself waits on it, just no longer racing a call that
// would never have released it.
func (m *Manager) hardRestart(ctx context.Context, reason string) error {
	m.mu.Lock()
	log.Printf("ttsworker: hard-restarting (%s)", reason)
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		done := make(chan struct{})
		go func() {
			_ = m.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			// Already sent SIGKILL - this is just waiting for the OS to
			// actually reap it before rebinding the same port. Spawning
			// anyway rather than blocking indefinitely; a real never-reaps
			// case would be a kernel-level problem no amount of waiting
			// here fixes.
			log.Printf("ttsworker: hard-restart kill did not reap within 10s, spawning anyway")
		}
	}
	m.mu.Unlock()

	m.restartGate.Lock()
	defer m.restartGate.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spawnAndWaitLocked(ctx)
}

// watchdogLoop periodically samples the worker's RSS and restarts it if it
// crosses cfg.RSSLimitBytes, restarts it if it's found to have died on its
// own (crash/OOM-killed) between ticks, or hard-restarts it (see
// hardRestart's own doc comment for why that needs a different path) if
// it's had at least one real job continuously in flight for longer than
// cfg.StuckJobTimeout - the "grinds on far longer than the text warrants"
// failure mode backend/CLAUDE.md documents, which neither of the other two
// triggers catches on its own: the process is alive and its RSS may not
// have crossed the limit yet, but it's making no forward progress either.
func (m *Manager) watchdogLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			pid := m.pid
			m.mu.Unlock()

			rss, err := readRSSBytes(pid)
			if err != nil {
				log.Printf("ttsworker: worker (pid %d) appears to have died: %v", pid, err)
				restarts.WithLabelValues("process_died").Inc()
				if err := m.restart(ctx, "process died"); err != nil {
					log.Printf("ttsworker: restart failed: %v", err)
				}
				continue
			}
			if rss >= m.cfg.RSSLimitBytes {
				restarts.WithLabelValues("rss_limit").Inc()
				if err := m.restart(ctx, fmt.Sprintf("RSS %d bytes >= limit %d bytes", rss, m.cfg.RSSLimitBytes)); err != nil {
					log.Printf("ttsworker: restart failed: %v", err)
				}
				continue
			}
			if busyFor, any := m.stuckSince(); any && busyFor >= m.cfg.StuckJobTimeout {
				reason := fmt.Sprintf("job(s) in flight for %s >= stuck timeout %s", busyFor.Round(time.Second), m.cfg.StuckJobTimeout)
				restarts.WithLabelValues("stuck").Inc()
				if err := m.hardRestart(ctx, reason); err != nil {
					log.Printf("ttsworker: hard restart failed: %v", err)
				}
			}
		}
	}
}

// readRSSBytes reads a process's resident set size directly from
// /proc/<pid>/status (Linux-only, matching this app's only deployment
// target) - no third-party dependency needed for this.
func readRSSBytes(pid int) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected VmRSS line: %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse VmRSS: %w", err)
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("VmRSS not found in /proc/%d/status", pid)
}

// do sends a request to the worker and decodes its response, translating a
// non-2xx status into an error using the worker's {detail} JSON shape.
func (m *Manager) do(ctx context.Context, method, path string, reqBody, respBody any) (err error) {
	start := time.Now()
	requestsInFlight.Inc()
	defer func() {
		requestsInFlight.Dec()
		took := time.Since(start)
		requestDuration.WithLabelValues(path, outcome(err)).Observe(took.Seconds())
		status := "ok"
		if err != nil {
			status = "err: " + err.Error()
		}
		log.Printf("ttsworker: timing %s %s took %s (%s)", method, path, took.Round(time.Millisecond), status)
	}()
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	if reqBody != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := m.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e ttsproto.ErrorResponse
		if json.Unmarshal(data, &e) == nil && e.Detail != "" {
			return fmt.Errorf("ttsworker: %s", e.Detail)
		}
		return fmt.Errorf("ttsworker %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if respBody != nil {
		switch rb := respBody.(type) {
		case *[]byte:
			*rb = data
		default:
			return json.Unmarshal(data, respBody)
		}
	}
	return nil
}

// Generate clones cloneModel's voice from refAudio (WAV bytes) to narrate
// text, returning WAV bytes. instruct, when non-empty, is a clone-time
// style instruction sent alongside refAudio - only honored by a family with
// its own instructOption (breeze_tts today); "" is every existing caller's
// unchanged behavior. guidanceScale, when non-empty, overrides that
// family's own configured instruct-time guidance_scale for this one call
// (see audioworker.GenerateRequest.GuidanceScale's own doc comment) -
// ignored unless instruct is also set; "" defers to the family's own
// configured default, same as every caller but the voice/character
// editor's "test with another voice as a base" control.
func (m *Manager) Generate(ctx context.Context, text, cloneModel string, refAudio []byte, refText, language, instruct, guidanceScale string) ([]byte, error) {
	return m.generate(ctx, text, cloneModel, refAudio, refText, language, instruct, guidanceScale, nil, 0)
}

// GenerateChunked is Generate with the clone family's own long-text chunk
// budget overridden to textChunkSize codepoints (see ttsproto.
// GenerateRequest.TextChunkSize) - 0 is exactly Generate.
func (m *Manager) GenerateChunked(ctx context.Context, text, cloneModel string, refAudio []byte, refText, language, instruct string, textChunkSize int) ([]byte, error) {
	return m.generate(ctx, text, cloneModel, refAudio, refText, language, instruct, "", nil, textChunkSize)
}

// GeneratePreview is Generate for the voice editor's saved-voice test,
// with a one-off sampling-temperature override (see
// ttsproto.GenerateRequest.Temperature) - nil is exactly Generate with no
// instruction.
func (m *Manager) GeneratePreview(ctx context.Context, text, cloneModel string, refAudio []byte, refText, language string, temperature *float64) ([]byte, error) {
	return m.generate(ctx, text, cloneModel, refAudio, refText, language, "", "", temperature, 0)
}

func (m *Manager) generate(ctx context.Context, text, cloneModel string, refAudio []byte, refText, language, instruct, guidanceScale string, temperature *float64, textChunkSize int) ([]byte, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	start := time.Now()
	var out []byte
	err := m.do(ctx, http.MethodPost, "/generate", ttsproto.GenerateRequest{
		Text:           text,
		CloneModel:     cloneModel,
		RefAudioBase64: base64.StdEncoding.EncodeToString(refAudio),
		RefText:        refText,
		Language:       language,
		Instruct:       instruct,
		GuidanceScale:  guidanceScale,
		Temperature:    temperature,
		TextChunkSize:  textChunkSize,
	}, &out)
	generateDuration.WithLabelValues(cloneModel, outcome(err)).Observe(time.Since(start).Seconds())
	if err == nil {
		if d, derr := wav.Duration(out); derr == nil {
			generatedAudio.WithLabelValues(cloneModel).Add(d.Seconds())
		}
	}
	return out, err
}

// Design renders a fresh reference clip via VoiceDesign, at natural pace
// (unstretched), returning WAV bytes. designModel selects which engine -
// "" defers to the worker's own process-wide default.
func (m *Manager) Design(ctx context.Context, refText, instruct, language, designModel string, seed *int64) ([]byte, error) {
	return m.DesignPreview(ctx, refText, instruct, language, designModel, seed, nil, nil)
}

// DesignPreview is Design for the voice editor's design preview, with
// one-off guidance-scale and sampling-temperature overrides (see
// ttsproto.DesignRequest.GuidanceScale/Temperature) - nil for both is
// exactly Design.
func (m *Manager) DesignPreview(ctx context.Context, refText, instruct, language, designModel string, seed *int64, guidanceScale, temperature *float64) ([]byte, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	var out []byte
	err := m.do(ctx, http.MethodPost, "/design", ttsproto.DesignRequest{
		RefText:       refText,
		Instruct:      instruct,
		Seed:          seed,
		Language:      language,
		DesignModel:   designModel,
		GuidanceScale: guidanceScale,
		Temperature:   temperature,
	}, &out)
	return out, err
}

// Music renders an ACE-Step music clip, returning WAV bytes; see
// ttsproto.MusicRequest's own doc comment for which fields default to
// what when left zero.
func (m *Manager) Music(ctx context.Context, req ttsproto.MusicRequest) ([]byte, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	var out []byte
	err := m.do(ctx, http.MethodPost, "/music", req, &out)
	return out, err
}

// StableAudioMusic renders a Stable Audio 3 clip via the Small/Music
// checkpoint, returning WAV bytes - Music's own exact counterpart for a
// different engine; see ttsproto.StableAudioRequest's own doc comment.
func (m *Manager) StableAudioMusic(ctx context.Context, req ttsproto.StableAudioRequest) ([]byte, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	var out []byte
	err := m.do(ctx, http.MethodPost, "/stable-audio-music", req, &out)
	return out, err
}

// StableAudioSFX renders a Stable Audio 3 clip via the Small/SFX
// checkpoint, returning WAV bytes - StableAudioMusic's own counterpart.
func (m *Manager) StableAudioSFX(ctx context.Context, req ttsproto.StableAudioRequest) ([]byte, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	var out []byte
	err := m.do(ctx, http.MethodPost, "/stable-audio-sfx", req, &out)
	return out, err
}

// StableAudioMedium renders a Stable Audio 3 clip via the larger Medium
// checkpoint, returning WAV bytes - StableAudioMusic's own counterpart.
func (m *Manager) StableAudioMedium(ctx context.Context, req ttsproto.StableAudioRequest) ([]byte, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	var out []byte
	err := m.do(ctx, http.MethodPost, "/stable-audio-medium", req, &out)
	return out, err
}

// Align runs forced word-level alignment of text against audio (a WAV
// clip, e.g. one already generated by Generate).
func (m *Manager) Align(ctx context.Context, text string, audio []byte, language string) ([]ttsproto.Word, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	var out ttsproto.AlignResponse
	err := m.do(ctx, http.MethodPost, "/align", ttsproto.AlignRequest{
		Text:        text,
		AudioBase64: base64.StdEncoding.EncodeToString(audio),
		Language:    language,
	}, &out)
	return out.Words, err
}

// Transcribe runs free ASR over audio (a WAV clip), returning what's
// actually spoken in it - see audioworker.Worker.Transcribe.
func (m *Manager) Transcribe(ctx context.Context, audio []byte) (string, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	var out ttsproto.TranscribeResponse
	err := m.do(ctx, http.MethodPost, "/transcribe", ttsproto.TranscribeRequest{
		AudioBase64: base64.StdEncoding.EncodeToString(audio),
	}, &out)
	return out.Text, err
}

// LLMGenerate runs one independent chat completion (system + user turn in,
// reply text out) against ttsworker's own embedded GGUF model
// (internal/llmworker, hosted in the same worker process as the TTS clone
// models - see backend/CLAUDE.md's "ttsworker / audioworker" section).
// Unlike Generate/Design/Align, many concurrent LLMGenerate calls are
// expected and safe: RLock here only guards against a restart landing
// mid-call, the same as every other proxied call - the actual concurrency
// limit is llmworker.Config.MaxConcurrent, enforced worker-side via
// multi-sequence batching (llamacpp.Scheduler), not by this method.
func (m *Manager) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	var out ttsproto.LLMGenerateResponse
	err := m.do(ctx, http.MethodPost, "/llm/generate", ttsproto.LLMGenerateRequest{
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		Temp:         temp,
		MaxTokens:    maxTokens,
	}, &out)
	return out.Text, err
}

// UnloadTTS asks the worker to free every currently-loaded TTS clone
// model right now (audioworker.Worker.UnloadAll, via POST /unload) rather
// than waiting for its own idle timer (MODEL_IDLE_UNLOAD_AFTER) - called
// by internal/jobs on a genuine pool-family switch away from TTS work
// (toward the LLM) and when the whole queue is paused, so a model that
// won't be touched again for a while actually frees its VRAM promptly.
// Each clone model reloads lazily on its next actual use, same as any
// other cache miss - this never fails a caller's own request, it just
// changes when the next one after a gap pays a reload cost.
func (m *Manager) UnloadTTS(ctx context.Context) error {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	return m.do(ctx, http.MethodPost, "/unload", nil, nil)
}

// UnloadLLM is UnloadTTS's LLM-side sibling (llmworker.Worker.Unload, via
// POST /llm/unload) - see UnloadTTS's own doc comment.
func (m *Manager) UnloadLLM(ctx context.Context) error {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	return m.do(ctx, http.MethodPost, "/llm/unload", nil, nil)
}

// Health reports the worker's current status.
func (m *Manager) Health(ctx context.Context) (ttsproto.HealthResponse, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()

	var out ttsproto.HealthResponse
	err := m.do(ctx, http.MethodGet, "/health", nil, &out)
	return out, err
}
