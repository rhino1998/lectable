package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A clip re-rendered in place keeps its path, so only the version tag
// tells a client to refetch it.
func TestWithClipVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.wav")
	if got := withClipVersion("/a/audio", path); got != "/a/audio" {
		t.Fatalf("missing file: got %q", got)
	}
	if got := withClipVersion("", path); got != "" {
		t.Fatalf("empty url: got %q", got)
	}
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := withClipVersion("/a/audio", path)
	if !strings.HasPrefix(first, "/a/audio?v=") {
		t.Fatalf("got %q", first)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if second := withClipVersion("/a/audio", path); second == first {
		t.Fatalf("rewritten clip kept its URL %q", second)
	}
}
