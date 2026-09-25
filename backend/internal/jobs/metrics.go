package jobs

import (
	"context"
	"errors"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// taskBuckets spans a job's run/queue time: sub-second lookups up to
// whole-chapter LLM passes and long background waits.
var taskBuckets = []float64{0.05, 0.25, 1, 2.5, 5, 10, 30, 60, 120, 300, 900, 1800, 3600}

var (
	taskRunSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "lectable",
		Subsystem: "jobs",
		Name:      "task_run_seconds",
		Help:      "How long a job-queue task ran once dispatched, by kind and outcome (ok, canceled, error).",
		Buckets:   taskBuckets,
	}, []string{"kind", "outcome"})

	taskWaitSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "lectable",
		Subsystem: "jobs",
		Name:      "task_wait_seconds",
		Help:      "How long a task sat queued (including time blocked on dependencies) before running, by kind and tier.",
		Buckets:   taskBuckets,
	}, []string{"kind", "tier"})

	completenessRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "lectable",
		Subsystem: "tts",
		Name:      "completeness_retries_total",
		Help:      "Clone generations re-run with a smaller text_chunk_size, by why (missing words, extra words, or both).",
	}, []string{"reason"})

	completenessResults = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "lectable",
		Subsystem: "tts",
		Name:      "completeness_results_total",
		Help:      "Completeness-checked clone generations by final result: clean (first try passed), fixed (a retry passed), unfixed (best attempt kept anyway), unchecked (alignment failed).",
	}, []string{"result"})
)

func tierName(tier int) string {
	switch tier {
	case TierUrgent:
		return "urgent"
	case TierLookahead:
		return "lookahead"
	case TierNormal:
		return "normal"
	case TierBackground:
		return "background"
	}
	return strconv.Itoa(tier)
}

func taskOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	}
	return "error"
}

// queueCollector reports the queue's current depth at scrape time rather
// than tracking it incrementally, so it can never drift from what the
// Jobs page shows.
type queueCollector struct {
	m        *Manager
	queued   *prometheus.Desc
	inFlight *prometheus.Desc
	paused   *prometheus.Desc
}

// RegisterMetrics exposes the queue's depth and paused state (the
// per-task histograms register themselves).
func (m *Manager) RegisterMetrics(reg prometheus.Registerer) error {
	return reg.Register(&queueCollector{
		m:        m,
		queued:   prometheus.NewDesc("lectable_jobs_queued", "Tasks waiting in the job queue, by kind and tier.", []string{"kind", "tier"}, nil),
		inFlight: prometheus.NewDesc("lectable_jobs_in_flight", "Tasks currently running, by kind and tier.", []string{"kind", "tier"}, nil),
		paused:   prometheus.NewDesc("lectable_jobs_paused", "1 while the job queue is paused.", nil, nil),
	})
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.queued
	ch <- c.inFlight
	ch <- c.paused
}

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	inFlight, queued := c.m.Snapshot()
	emit := func(desc *prometheus.Desc, tasks []QueueTask) {
		type key struct {
			kind Kind
			tier int
		}
		counts := map[key]int{}
		for _, t := range tasks {
			counts[key{t.Kind, t.Tier}]++
		}
		for k, n := range counts {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(n), string(k.kind), tierName(k.tier))
		}
	}
	emit(c.queued, queued)
	emit(c.inFlight, inFlight)
	paused := 0.0
	if c.m.Paused() {
		paused = 1
	}
	ch <- prometheus.MustNewConstMetric(c.paused, prometheus.GaugeValue, paused)
}
