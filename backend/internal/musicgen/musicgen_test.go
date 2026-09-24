package musicgen

import (
	"context"
	"math"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// rampClip builds a mono clip of n samples at sampleRate, sample i valued
// float32(i) - a synthetic, easily-checked signal (never a real waveform)
// so trimToLoopableClip's own index math can be verified exactly: any
// mismatch between which samples got blended shows up as a wrong value
// rather than something only audible.
func rampClip(n, sampleRate int) *wav.Clip {
	samples := make([]float32, n)
	for i := range samples {
		samples[i] = float32(i)
	}
	return &wav.Clip{Samples: samples, SampleRate: sampleRate, Channels: 1}
}

func TestTrimToLoopableClipLength(t *testing.T) {
	clip := rampClip(100, 100) // 1s: served 0.8s + 0.2s crossfade overrun
	out := trimToLoopableClip(clip, 0.8, 0.2)
	if len(out.Samples) != 80 {
		t.Fatalf("len(out.Samples) = %d, want 80", len(out.Samples))
	}
	if out.SampleRate != 100 || out.Channels != 1 {
		t.Fatalf("out format = (%d, %d channels), want (100, 1)", out.SampleRate, out.Channels)
	}
}

func TestTrimToLoopableClipBlend(t *testing.T) {
	// served=4, crossfade=2, clip = [0..5]: the overrun [4,5] fades out
	// over the head [0,1] as it fades in; the rest is untouched.
	clip := &wav.Clip{Samples: []float32{0, 1, 2, 3, 4, 5}, SampleRate: 1, Channels: 1}
	out := trimToLoopableClip(clip, 4, 2)

	if len(out.Samples) != 4 {
		t.Fatalf("len(out.Samples) = %d, want 4", len(out.Samples))
	}
	// The wrap from the last sample lands on the overrun that followed it
	// in the render - continuous.
	if out.Samples[0] != 4 {
		t.Fatalf("out.Samples[0] = %v, want 4 (the overrun's first sample)", out.Samples[0])
	}
	tt := 0.5
	want := float32(1*math.Sin(tt*math.Pi/2) + 5*math.Cos(tt*math.Pi/2))
	if math.Abs(float64(out.Samples[1]-want)) > 1e-6 {
		t.Errorf("out.Samples[1] = %v, want %v", out.Samples[1], want)
	}
	if out.Samples[2] != 2 || out.Samples[3] != 3 {
		t.Fatalf("out.Samples[2:4] = %v, want [2 3]", out.Samples[2:4])
	}
}

func TestTrimToLoopableClipStereo(t *testing.T) {
	// Interleaved stereo: both channels blend with the same per-frame gain.
	clip := &wav.Clip{Samples: []float32{0, 10, 1, 11, 2, 12, 3, 13}, SampleRate: 1, Channels: 2}
	out := trimToLoopableClip(clip, 3, 1)
	want := []float32{3, 13, 1, 11, 2, 12}
	for i := range want {
		if out.Samples[i] != want[i] {
			t.Fatalf("out.Samples = %v, want %v", out.Samples, want)
		}
	}
}

func TestTrimToLoopableClipFallbackWhenTooShort(t *testing.T) {
	// Render came back shorter than served+crossfade - must return the
	// clip unmodified rather than slicing out of bounds.
	clip := rampClip(5, 100)
	out := trimToLoopableClip(clip, 1.0, 1.0)
	if len(out.Samples) != len(clip.Samples) {
		t.Fatalf("len(out.Samples) = %d, want %d (unmodified fallback)", len(out.Samples), len(clip.Samples))
	}
}

func TestTileLoop(t *testing.T) {
	loop, err := wav.Encode([]float32{0, 0.1, 0.2, 0.3}, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := TileLoop(loop, 2.5, 0.5) // 10 samples starting at index 2
	if err != nil {
		t.Fatal(err)
	}
	out, err := wav.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []float32{0.2, 0.3, 0, 0.1, 0.2, 0.3, 0, 0.1, 0.2, 0.3}
	if len(out.Samples) != len(want) {
		t.Fatalf("len = %d, want %d", len(out.Samples), len(want))
	}
	for i := range want {
		if math.Abs(float64(out.Samples[i]-want[i])) > 1e-3 {
			t.Fatalf("out = %v, want %v", out.Samples, want)
		}
	}
}

// fakeBackend records calls and returns silence of the requested length.
type fakeBackend struct {
	calls []ttsproto.StableAudioRequest
}

func silence(seconds float64) []byte {
	data, _ := wav.Encode(make([]float32, int(seconds*100)), 100, 1)
	return data
}

func (f *fakeBackend) StableAudioMedium(_ context.Context, req ttsproto.StableAudioRequest) ([]byte, error) {
	f.calls = append(f.calls, req)
	return silence(req.DurationSeconds), nil
}

func clipLen(t *testing.T, data []byte) float64 {
	t.Helper()
	c, err := wav.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return clipSeconds(c)
}

func TestGenerateRegionChunksAndAmbienceLoop(t *testing.T) {
	f := &fakeBackend{}
	res, err := GenerateRegion(context.Background(), f, Region{Prompt: "m", AmbiencePrompt: "a", TargetDurationSeconds: 150})
	if err != nil {
		t.Fatal(err)
	}
	// 164s render = 2 chunks (unseeded, then a continuation seeded from a
	// continuationSeedSeconds tail), then the ambience loop.
	if len(f.calls) != 3 {
		t.Fatalf("%d calls, want 3", len(f.calls))
	}
	if f.calls[0].InitAudioBase64 != "" || f.calls[1].InpaintMaskStartSeconds != continuationSeedSeconds {
		t.Errorf("music calls = %+v, want unseeded then %gs-seeded", f.calls[:2], continuationSeedSeconds)
	}
	if f.calls[2].Prompt != "a" || f.calls[2].DurationSeconds != AmbienceLoopSeconds+loopCrossfadeSeconds {
		t.Errorf("ambience call = %+v", f.calls[2])
	}
	if got := clipLen(t, res.Music); math.Abs(got-160) > 0.02 {
		t.Errorf("music = %gs, want 160", got)
	}
	if got := clipLen(t, res.AmbienceLoop); math.Abs(got-AmbienceLoopSeconds) > 0.02 {
		t.Errorf("ambience loop = %gs, want %g", got, AmbienceLoopSeconds)
	}
}

func TestGenerateRegionTrimsSeedToTail(t *testing.T) {
	f := &fakeBackend{}
	if _, err := GenerateRegion(context.Background(), f, Region{Prompt: "m", TargetDurationSeconds: 20, Seed: silence(150)}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("%d calls, want 1", len(f.calls))
	}
	if got := f.calls[0].InpaintMaskStartSeconds; got != continuationSeedSeconds {
		t.Errorf("seed = %gs, want the %gs tail, not the whole previous clip", got, continuationSeedSeconds)
	}
}

func TestGenerateRegionWithoutAmbience(t *testing.T) {
	f := &fakeBackend{}
	res, err := GenerateRegion(context.Background(), f, Region{Prompt: "m", TargetDurationSeconds: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || res.AmbienceLoop != nil {
		t.Fatalf("%d calls, loop %v; want one plain call and no loop", len(f.calls), res.AmbienceLoop != nil)
	}
}
