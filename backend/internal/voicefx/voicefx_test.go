package voicefx

import (
	"math"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/wav"
)

// voiced is a second of something speech-like: a 140 Hz buzz with
// harmonics under a slow envelope.
func voiced(rate int) []float32 {
	x := make([]float32, rate)
	for i := range x {
		t := float64(i) / float64(rate)
		env := 0.5 + 0.5*math.Sin(2*math.Pi*3*t)
		x[i] = float32(0.25 * env * (math.Sin(2*math.Pi*140*t) + 0.5*math.Sin(2*math.Pi*280*t) + 0.3*math.Sin(2*math.Pi*1400*t)))
	}
	return x
}

func rms(s []float32) float64 {
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func TestProcessKeepsLengthAndLevel(t *testing.T) {
	for _, rate := range []int{24000, 48000} {
		x := voiced(rate)
		for _, name := range Names() {
			y := Process(name, x, rate)
			if len(y) != len(x) {
				t.Fatalf("%s at %d Hz: %d samples, want %d", name, rate, len(y), len(x))
			}
			var diff, peak float64
			for i := range y {
				if math.IsNaN(float64(y[i])) || math.IsInf(float64(y[i]), 0) {
					t.Fatalf("%s at %d Hz: sample %d is %v", name, rate, i, y[i])
				}
				diff += math.Abs(float64(y[i] - x[i]))
				peak = math.Max(peak, math.Abs(float64(y[i])))
			}
			if diff/float64(len(x)) < 0.01 {
				t.Errorf("%s at %d Hz barely changes the clip", name, rate)
			}
			if got, want := rms(y), rms(x); math.Abs(got-want) > 0.02*want {
				t.Errorf("%s at %d Hz: level %.4f, want %.4f", name, rate, got, want)
			}
			if peak > 0.98 {
				t.Errorf("%s at %d Hz: peak %.3f clips", name, rate, peak)
			}
		}
	}
}

func TestUnknownEffectLeavesClipAlone(t *testing.T) {
	x := voiced(24000)
	for _, name := range []VoiceEffect{"", "sparkly"} {
		if y := Process(name, x, 24000); &y[0] != &x[0] {
			t.Errorf("Process(%q) copied the clip", name)
		}
		if Tag(name) != "" {
			t.Errorf("Tag(%q) = %q, want none", name, Tag(name))
		}
	}
	if !Valid("") || !Valid(Insectoid) || Valid("sparkly") {
		t.Error("Valid: want \"\" and insectoid valid, sparkly not")
	}
}

func TestApplyRoundTripsWAV(t *testing.T) {
	in, err := wav.Encode(voiced(24000), 24000, 1)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Apply(Underwater, in)
	if err != nil {
		t.Fatal(err)
	}
	clip, err := wav.Decode(out)
	if err != nil {
		t.Fatal(err)
	}
	if clip.SampleRate != 24000 || len(clip.Samples) != 24000 {
		t.Fatalf("got %d samples at %d Hz", len(clip.Samples), clip.SampleRate)
	}
}
