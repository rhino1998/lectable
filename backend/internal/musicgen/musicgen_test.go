package musicgen

import (
	"math"
	"testing"

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
	// 1s @ 100Hz render target with a 0.1 margin -> renderTarget analogue
	// of served=8, margin=1 (10 total render samples, matching
	// loopCrossfadeFraction's own 10%-per-side shape at small scale).
	served := 0.08
	margin := 0.01
	sampleRate := 100
	clip := rampClip(10, sampleRate) // 0.10s of audio = served + 2*margin

	out := trimToLoopableClip(clip, margin, served)

	wantSamples := int(served * float64(sampleRate))
	if len(out.Samples) != wantSamples {
		t.Fatalf("len(out.Samples) = %d, want %d", len(out.Samples), wantSamples)
	}
	if out.SampleRate != sampleRate || out.Channels != 1 {
		t.Fatalf("out format = (%d, %d channels), want (%d, 1)", out.SampleRate, out.Channels, sampleRate)
	}
}

func TestTrimToLoopableClipBlend(t *testing.T) {
	// margin=2 samples, served=4 samples, render=8 samples: clip =
	// [0,1,2,3,4,5,6,7]. headMargin = [0,1], core = [2,3,4,5].
	// trimmed tail (last 2 of core, i.e. [4,5]) should be equal-power
	// blended with headMargin ([0,1]) - not left as [4,5] verbatim, and
	// not simply overwritten by headMargin either.
	clip := &wav.Clip{Samples: []float32{0, 1, 2, 3, 4, 5, 6, 7}, SampleRate: 1, Channels: 1}
	out := trimToLoopableClip(clip, 2, 4)

	if len(out.Samples) != 4 {
		t.Fatalf("len(out.Samples) = %d, want 4", len(out.Samples))
	}
	// First two samples (outside the crossfade region) are the core's own
	// head, untouched.
	if out.Samples[0] != 2 || out.Samples[1] != 3 {
		t.Fatalf("out.Samples[0:2] = %v, want [2 3]", out.Samples[0:2])
	}
	// Last two samples are the crossfade: core's own tail ([4,5]) fading
	// out against headMargin ([0,1]) fading in, equal-power.
	coreTail := []float32{4, 5}
	headMargin := []float32{0, 1}
	for i := 0; i < 2; i++ {
		tt := float64(i) / 2
		fadeOut := math.Cos(tt * math.Pi / 2)
		fadeIn := math.Sin(tt * math.Pi / 2)
		want := float32(float64(coreTail[i])*fadeOut + float64(headMargin[i])*fadeIn)
		got := out.Samples[2+i]
		if math.Abs(float64(got-want)) > 1e-6 {
			t.Errorf("out.Samples[%d] = %v, want %v", 2+i, got, want)
		}
	}
	// The blend must not just be the untouched core tail (verifying the
	// crossfade actually ran, not a no-op).
	if out.Samples[2] == coreTail[0] && out.Samples[3] == coreTail[1] {
		t.Fatalf("tail region unchanged from core - crossfade didn't run")
	}
}

func TestTrimToLoopableClipFallbackWhenTooShort(t *testing.T) {
	// Render came back shorter than served+2*margin (a truncated/short
	// chunk) - must return the clip unmodified rather than slicing out of
	// bounds.
	clip := rampClip(5, 100)
	out := trimToLoopableClip(clip, 1.0, 1.0) // needs 3s @ 100Hz = 300 samples
	if len(out.Samples) != len(clip.Samples) {
		t.Fatalf("len(out.Samples) = %d, want %d (unmodified fallback)", len(out.Samples), len(clip.Samples))
	}
}
