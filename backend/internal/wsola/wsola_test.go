package wsola

import (
	"math"
	"testing"
)

const testRate = 24000

func sine(freq float64, seconds float64) []float32 {
	n := int(seconds * testRate)
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/testRate))
	}
	return out
}

// zeroCrossingFreq estimates a pure tone's frequency from its zero
// crossings over the middle of the clip (skipping the edges, where the
// stretch's first/last windows blend with silence or an unstretched tail).
func zeroCrossingFreq(s []float32) float64 {
	lo, hi := len(s)/4, len(s)*3/4
	crossings := 0
	for i := lo + 1; i < hi; i++ {
		if (s[i-1] < 0) != (s[i] < 0) {
			crossings++
		}
	}
	return float64(crossings) / 2 / (float64(hi-lo) / testRate)
}

func TestTimeStretchIdentityCopies(t *testing.T) {
	in := sine(220, 0.5)
	out := TimeStretch(in, testRate, 1.0)
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d", len(out), len(in))
	}
	out[0] = 42
	if in[0] == 42 {
		t.Fatal("rate 1.0 returned the input slice itself, want a copy")
	}
}

func TestTimeStretchEmpty(t *testing.T) {
	if out := TimeStretch(nil, testRate, 1.5); len(out) != 0 {
		t.Fatalf("len = %d, want 0", len(out))
	}
}

func TestTimeStretchLengthAndPitch(t *testing.T) {
	const freq = 220.0
	in := sine(freq, 3)
	for _, rate := range []float64{0.75, 1.25, 1.5, 2.0} {
		out := TimeStretch(in, testRate, rate)
		want := float64(len(in)) / rate
		// The tail past the last full window is appended unstretched, so
		// allow a couple of windows' slack.
		if tol := 0.05 * want; math.Abs(float64(len(out))-want) > tol {
			t.Errorf("rate %.2f: len = %d, want ~%.0f (±%.0f)", rate, len(out), want, tol)
		}
		if got := zeroCrossingFreq(out); math.Abs(got-freq) > 0.03*freq {
			t.Errorf("rate %.2f: pitch %.1f Hz, want ~%.0f Hz (pitch should be preserved)", rate, got, freq)
		}
	}
}

func TestTimeStretchOutOfRangeIsSilence(t *testing.T) {
	in := sine(220, 1)
	rate := MaxPlaybackRate * 2
	out := TimeStretch(in, testRate, rate)
	if want := int(float64(len(in)) / rate); len(out) != want {
		t.Fatalf("len = %d, want %d", len(out), want)
	}
	for i, v := range out {
		if v != 0 {
			t.Fatalf("out[%d] = %v, want silence outside the supported range", i, v)
		}
	}
}
