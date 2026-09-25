package store

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// dbCallDuration times every call on the store's single DuckDB connection.
// With one connection a call's time includes waiting for it, so a slow
// begin_wait means "queued behind other DB work", and tx_commit/tx_rollback
// is how long a transaction held the connection.
var dbCallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "lectable",
	Subsystem: "db",
	Name:      "call_duration_seconds",
	Help:      "Duration of calls on the store's DuckDB connection, by op (exec, query, queryrow, begin_wait, tx_commit, tx_rollback).",
	Buckets:   []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
}, []string{"op"})

// metricOp maps logCallTiming's op descriptions to short label values.
func metricOp(op string) string {
	switch op {
	case "begin (connection wait)":
		return "begin_wait"
	case "transaction held (commit)":
		return "tx_commit"
	case "transaction held (rollback)":
		return "tx_rollback"
	}
	return op
}
