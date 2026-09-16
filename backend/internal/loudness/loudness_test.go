package loudness

import (
	"math"
	"testing"
)

func toneSamples(amp float32, n int) []float32 {
	samples := make([]float32, n)
	for i := range samples {
		if i%2 == 0 {
			samples[i] = amp
		} else {
			samples[i] = -amp
		}
	}
	return samples
}

func TestIntegratedEmptyIsNegativeInfinity(t *testing.T) {
	if got := Integrated(nil, 16000); !math.IsInf(got, -1) {
		t.Fatalf("Integrated(nil): expected -Inf, got %v", got)
	}
	if got := Integrated([]float32{}, 16000); !math.IsInf(got, -1) {
		t.Fatalf("Integrated(empty): expected -Inf, got %v", got)
	}
}

func TestIntegratedSilenceIsNegativeInfinity(t *testing.T) {
	silence := make([]float32, 16000)
	if got := Integrated(silence, 16000); !math.IsInf(got, -1) {
		t.Fatalf("Integrated(silence): expected -Inf, got %v", got)
	}
}

// TestIntegratedScalesWithAmplitude checks the property NormalizeVolume's
// own gain math leans on: doubling a signal's amplitude should raise its
// measured loudness by exactly 20*log10(2) ~= 6.02 dB, regardless of
// K-weighting's own exact frequency-response details - K-weighting is a
// linear filter, so scaling its input by k scales its output (and thus
// mean-square energy, and thus loudness) by that same k, for any two
// signals that differ only by a scalar multiplier.
func TestIntegratedScalesWithAmplitude(t *testing.T) {
	const sampleRate = 16000
	quiet := Integrated(toneSamples(0.1, sampleRate), sampleRate)
	loud := Integrated(toneSamples(0.2, sampleRate), sampleRate)
	if math.IsInf(quiet, -1) || math.IsInf(loud, -1) {
		t.Fatalf("expected finite loudness values, got quiet=%v loud=%v", quiet, loud)
	}
	want := 20 * math.Log10(2)
	if diff := math.Abs((loud - quiet) - want); diff > 0.01 {
		t.Fatalf("expected a 2x amplitude increase to raise loudness by %.4f LU, got %.4f LU (quiet=%.4f loud=%.4f)", want, loud-quiet, quiet, loud)
	}
}

// TestIntegratedGatesOutSilence is loudness's own version of the property
// internal/voicerefs.NormalizeVolume depends on: appending a lot of
// silence to a clip shouldn't meaningfully change its measured loudness,
// unlike plain whole-clip RMS (which the silence would dilute).
func TestIntegratedGatesOutSilence(t *testing.T) {
	const sampleRate = 16000
	tone := toneSamples(0.3, sampleRate/2) // 0.5s of tone
	toneOnly := Integrated(tone, sampleRate)
	if math.IsInf(toneOnly, -1) {
		t.Fatalf("expected a finite loudness for the tone alone")
	}

	padded := append(append([]float32{}, tone...), make([]float32, sampleRate*5)...) // + 5s of silence
	paddedLoudness := Integrated(padded, sampleRate)
	if math.IsInf(paddedLoudness, -1) {
		t.Fatalf("expected a finite loudness even with silence padding")
	}

	if diff := math.Abs(paddedLoudness - toneOnly); diff > 3.0 {
		t.Fatalf("expected silence padding to barely move the measured loudness (BS.1770 gating), tone-only=%.2f padded=%.2f (diff %.2f LU)", toneOnly, paddedLoudness, diff)
	}
}

// TestIntegratedShortClipFallback checks that a clip shorter than one
// 400ms gating block still produces a usable (finite) measurement instead
// of silently reporting "no content" - real reference clips are usually
// several seconds, but nothing here should assume that.
func TestIntegratedShortClipFallback(t *testing.T) {
	const sampleRate = 16000
	short := toneSamples(0.3, sampleRate/20) // 50ms, well under one 400ms block
	got := Integrated(short, sampleRate)
	if math.IsInf(got, -1) {
		t.Fatalf("expected a finite loudness for a short clip, got -Inf")
	}
}

func TestGainFor(t *testing.T) {
	if got := GainFor(-20, -20); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("GainFor(x, x): expected 1.0, got %v", got)
	}
	// +6.02 LU should be a 2x linear gain.
	if got := GainFor(-20, -20+20*math.Log10(2)); math.Abs(got-2.0) > 1e-9 {
		t.Fatalf("GainFor: expected 2.0x for a +6.02 LU increase, got %v", got)
	}
	// -6.02 LU should be a 0.5x linear gain.
	if got := GainFor(-20, -20-20*math.Log10(2)); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("GainFor: expected 0.5x for a -6.02 LU decrease, got %v", got)
	}
}
