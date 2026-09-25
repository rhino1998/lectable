package main

import (
	"bytes"
	"os"
	"testing"
)

// TestGeneratedUpToDate fails when a DTO in internal/httpapi changed
// without regenerating the frontend's and Android client's copies.
func TestGeneratedUpToDate(t *testing.T) {
	outs, err := generate("../..")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range outs {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale; run `go generate ./internal/httpapi` from backend/", path)
		}
	}
}
