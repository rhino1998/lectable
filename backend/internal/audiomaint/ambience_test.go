package audiomaint

import (
	"os"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// A loop's WAV goes only when its latents are there to rebuild it from.
func TestDropRebuildableAmbienceLoops(t *testing.T) {
	data := t.TempDir()
	withLatents := audiopath.AmbienceLoopFile(data, "b", "ocean")
	wavOnly := audiopath.AmbienceLoopFile(data, "b", "forest")
	badLatents := audiopath.AmbienceLoopFile(data, "b", "cave")
	for _, loop := range []string{withLatents, wavOnly, badLatents} {
		write(t, loop, sineWAV(t, 44100, 2, 1))
	}
	lat := &ttsproto.Latents{
		Kind: "latents", Frames: 2, Dim: 2, HopSamples: 4096, SampleRate: 44100,
		Family: "stable_audio_medium", Data: make([]byte, 2*2*4),
	}
	if err := latents.WriteFile(latents.Sidecar(withLatents), lat, "f16"); err != nil {
		t.Fatal(err)
	}
	write(t, latents.Sidecar(badLatents), []byte("not latents"))

	dropped, freed := DropRebuildableAmbienceLoops(data)
	if dropped != 1 || freed == 0 {
		t.Errorf("dropped %d loops (%d bytes), want the 1 with latents", dropped, freed)
	}
	if _, err := os.Stat(withLatents); !os.IsNotExist(err) {
		t.Errorf("the loop with latents still has its WAV: %v", err)
	}
	if _, err := os.Stat(latents.Sidecar(withLatents)); err != nil {
		t.Errorf("its latents: %v", err)
	}
	for _, loop := range []string{wavOnly, badLatents} {
		if _, err := os.Stat(loop); err != nil {
			t.Errorf("a loop that can't be rebuilt lost its WAV: %v", err)
		}
	}
}
