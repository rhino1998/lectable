package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithBodyLimit(t *testing.T) {
	var read int64
	var readErr error
	h := withBodyLimit(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		read, readErr = io.Copy(io.Discard, r.Body)
	}))
	big := strings.Repeat("x", maxRequestBody+1)

	for _, tc := range []struct {
		contentType string
		wantErr     bool
	}{
		{"application/json", true},
		{"", true},
		{"multipart/form-data; boundary=x", false},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/books", strings.NewReader(big))
		if tc.contentType != "" {
			r.Header.Set("Content-Type", tc.contentType)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if gotErr := readErr != nil; gotErr != tc.wantErr {
			t.Errorf("Content-Type %q: read %d bytes, err %v; want error %v", tc.contentType, read, readErr, tc.wantErr)
		}
	}
}
