package httpapi

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "lectable",
	Subsystem: "http",
	Name:      "request_duration_seconds",
	Help:      "Duration of API requests, by route pattern (e.g. \"GET /api/books/{id}\"), method, and status code.",
	Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
}, []string{"route", "method", "code"})

// withMetrics logs and times every request. The route label is the
// ServeMux pattern that matched (set on r while next routes it), so ids in
// the path don't explode the label set; an unmatched request is "unmatched".
func withMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		httpRequestDuration.WithLabelValues(route, r.Method, strconv.Itoa(sw.status)).Observe(time.Since(start).Seconds())
		logRequest(r)
	})
}

// statusWriter records the response status, passing Flush/Hijack through
// (the live WebSocket upgrade hijacks the connection).
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status, w.wroteHeader = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking not supported")
	}
	w.status, w.wroteHeader = http.StatusSwitchingProtocols, true
	return h.Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
