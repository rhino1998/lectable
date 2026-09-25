package ttsworker

import (
	"context"
	"errors"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Worker metrics, measured on the cmd/server side of every proxied call -
// the worker process itself exports nothing (its CPU/memory come from
// processCollector below, reading /proc for its pid).

// modelBuckets spans the worker's call latencies: ~50ms aligns up to
// minute-long generations and LLM batches.
var modelBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 15, 30, 60, 120, 300}

var (
	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "lectable",
		Subsystem: "worker",
		Name:      "request_duration_seconds",
		Help:      "Duration of calls from the server to the ttsworker, by endpoint and outcome.",
		Buckets:   modelBuckets,
	}, []string{"endpoint", "outcome"})

	requestsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "lectable",
		Subsystem: "worker",
		Name:      "requests_in_flight",
		Help:      "Calls to the ttsworker currently in flight.",
	})

	restarts = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "lectable",
		Subsystem: "worker",
		Name:      "restarts_total",
		Help:      "ttsworker process restarts, by reason (process_died, rss_limit, stuck, manual).",
	}, []string{"reason"})

	generateDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "lectable",
		Subsystem: "tts",
		Name:      "generate_duration_seconds",
		Help:      "Duration of TTS clone generations, by clone model and outcome (ok, decode_budget, canceled, error).",
		Buckets:   modelBuckets,
	}, []string{"clone_model", "outcome"})

	generatedAudio = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "lectable",
		Subsystem: "tts",
		Name:      "generated_audio_seconds_total",
		Help:      "Seconds of audio produced by successful clone generations; rate(generate_duration_seconds_sum) / rate(this) is the real-time factor.",
	}, []string{"clone_model"})
)

// workerPID is read by processCollector at scrape time; 0 while no worker
// is running (the collector then reports nothing).
var workerPID = func() (int, error) { return 0, errors.New("ttsworker not started") }

// registerProcessCollector exposes the worker process's CPU seconds,
// memory, and open files as lectable_ttsworker_process_*.
func (m *Manager) registerProcessCollector() {
	workerPID = func() (int, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.pid == 0 {
			return 0, errors.New("ttsworker not running")
		}
		return m.pid, nil
	}
	// A second Manager (tests) would register a duplicate; the first one wins.
	_ = prometheus.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{
		PidFn:     func() (int, error) { return workerPID() },
		Namespace: "lectable_ttsworker",
	}))
}

// outcome classifies a worker call's error for metric labels.
func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case strings.Contains(err.Error(), "before EOC for this text chunk"):
		// Higgs's decode budget/max_tokens: a generation that never
		// reached its end-of-clip token (see audioworker's
		// isMaxTokensOverflow).
		return "decode_budget"
	default:
		return "error"
	}
}
