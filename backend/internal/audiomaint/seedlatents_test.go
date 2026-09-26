package audiomaint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

func TestMigrateSeedLatents(t *testing.T) {
	dir := t.TempDir()
	music := filepath.Join(dir, "audio", "b", "c", "music")
	if err := os.MkdirAll(music, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"r1.seed.wav", "r2.seed.wav", "r2.seed.lat", "r3.opus"} {
		if err := os.WriteFile(filepath.Join(music, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	encode := func(_ context.Context, wav []byte) (*ttsproto.Latents, error) {
		calls++
		return &ttsproto.Latents{Kind: latents.KindLatents, Frames: 1, Dim: 1, Data: make([]byte, 4)}, nil
	}
	stats := MigrateSeedLatents(t.Context(), dir, encode, func() bool { return true })
	if stats.Converted != 1 || calls != 1 {
		t.Fatalf("stats %+v, %d encodes; want 1 conversion", stats, calls)
	}
	for name, want := range map[string]bool{"r1.seed.wav": false, "r1.seed.lat": true, "r2.seed.wav": false, "r2.seed.lat": true, "r3.opus": true} {
		_, err := os.Stat(filepath.Join(music, name))
		if (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", name, err == nil, want)
		}
	}
	if _, err := latents.ReadFile(filepath.Join(music, "r1.seed.lat")); err != nil {
		t.Errorf("converted seed unreadable: %v", err)
	}
}
