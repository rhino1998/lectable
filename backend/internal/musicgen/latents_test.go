package musicgen

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// latentsFake returns silence of the requested length and latents of dim 1
// whose every frame is labelled: a seeded window's first frames copy the
// seed's labels, and each new frame gets the next label - so the region's
// timeline is labels 1, 2, 3, ... when seeding is exact.
type latentsFake struct {
	next  float32
	calls []ttsproto.StableAudioRequest
}

func (f *latentsFake) StableAudioMedium(context.Context, ttsproto.StableAudioRequest) ([]byte, error) {
	panic("latents path must not call StableAudioMedium for music")
}

func (f *latentsFake) StableAudioMediumLatents(_ context.Context, req ttsproto.StableAudioRequest) ([]byte, *ttsproto.Latents, error) {
	f.calls = append(f.calls, req)
	samples := int(req.DurationSeconds * latentRate)
	frames := (samples + latentHop - 1) / latentHop
	data := make([]byte, 4*frames)
	seedFrames := 0
	if req.InitLatents != nil {
		seedFrames = req.InitLatents.Frames
		copy(data, req.InitLatents.Data)
	}
	// Frames past the whole-frame duration are the window's padding: -1.
	whole := samples / latentHop
	for i := seedFrames; i < frames; i++ {
		v := float32(-1)
		if i < whole {
			f.next++
			v = f.next
		}
		binary.LittleEndian.PutUint32(data[4*i:], math.Float32bits(v))
	}
	w, _ := wav.Encode(make([]float32, samples*2), latentRate, 2)
	return w, &ttsproto.Latents{Kind: latents.KindLatents, Frames: frames, Dim: 1, HopSamples: latentHop, SampleRate: latentRate, Data: data}, nil
}

func labels(l *ttsproto.Latents) []float32 {
	out := make([]float32, l.Frames)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(l.Data[4*i:]))
	}
	return out
}

func TestGenerateRegionLatentsSeedsExactly(t *testing.T) {
	f := &latentsFake{}
	// Long enough for three chunks.
	res, err := GenerateRegion(t.Context(), f, Region{Prompt: "p", TargetDurationSeconds: 3*maxChunkSeconds - 30})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("%d calls, want 3", len(f.calls))
	}
	n := seedFramesFor()
	for i, c := range f.calls[1:] {
		got := labels(c.InitLatents)
		for j := 1; j < len(got); j++ {
			if got[j] != got[j-1]+1 {
				t.Fatalf("chunk %d seed labels not contiguous: %v", i+2, got)
			}
		}
		if len(got) != n || c.InpaintMaskStartSeconds != frameSeconds(n) {
			t.Fatalf("chunk %d seed %d frames, mask start %v", i+2, len(got), c.InpaintMaskStartSeconds)
		}
	}
	// Labels are assigned in timeline order, so contiguous seeds mean each
	// chunk continued exactly from the previous one's end - and the next
	// label handed out after chunk k's seed is chunk k+1's first new frame.
	music, err := wav.Decode(res.Music)
	if err != nil {
		t.Fatal(err)
	}
	served := 3*maxChunkSeconds - 30 + crossfadePaddingSeconds
	if got, want := len(music.Samples)/2, int(served*latentRate); got != want {
		t.Fatalf("music %d frames, want %d", got, want)
	}
	seed := labels(res.SeedLatents)
	end := int(math.Round(served * latentRate / latentHop))
	if len(seed) != n || seed[0] != float32(end-n+1) || seed[len(seed)-1] != float32(end) {
		t.Fatalf("SeedLatents labels %v..%v (%d), want %d..%d", seed[0], seed[len(seed)-1], len(seed), end-n+1, end)
	}

	// The next region continues from those latents.
	f.calls = nil
	if _, err := GenerateRegion(t.Context(), f, Region{Prompt: "p", TargetDurationSeconds: 20, SeedLatents: res.SeedLatents}); err != nil {
		t.Fatal(err)
	}
	if got := labels(f.calls[0].InitLatents); got[0] != seed[0] || len(got) != n {
		t.Fatalf("next region seeded from %v", got)
	}
}
