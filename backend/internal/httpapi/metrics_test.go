package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestMetricsEndpoint checks /metrics serves the HTTP histogram labelled by
// the matched route pattern, not the raw path.
func TestMetricsEndpoint(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1")
	if resp := doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET book: %d", resp.StatusCode)
	}
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		`lectable_http_request_duration_seconds_count{code="200",method="GET",route="GET /api/books/{id}"}`,
		`lectable_db_call_duration_seconds_count{op="queryrow"}`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics is missing %s", want)
		}
	}
	if strings.Contains(text, bookID) {
		t.Errorf("/metrics leaks a raw path id into a label")
	}
}
