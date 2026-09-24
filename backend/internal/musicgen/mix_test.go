package musicgen

import (
	"math"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/wav"
)

func tone(n int, amp float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = amp * float32(math.Sin(float64(i)*0.05))
	}
	return out
}

func TestMixAmbienceLevelMatchesAmbienceUnderMusic(t *testing.T) {
	music, _ := wav.Encode(tone(8000, 0.4), 16000, 1)
	// Ambience rendered far louder than the music - the mix must still
	// sit it at ambienceRelativeLevel of the music's own RMS.
	amb, _ := wav.Encode(tone(8100, 0.9), 16000, 1)
	out, err := MixAmbience(music, amb)
	if err != nil {
		t.Fatal(err)
	}
	clip, err := wav.Decode(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(clip.Samples) != 8000 {
		t.Fatalf("mix length = %d, want the shorter layer's 8000", len(clip.Samples))
	}
	// Both layers are the same in-phase tone, so the mix's RMS is the
	// music's times (1 + ambienceRelativeLevel).
	wantRMS := rms(tone(8000, 0.4)) * (1 + ambienceRelativeLevel)
	if got := rms(clip.Samples); math.Abs(got-wantRMS) > 0.01 {
		t.Fatalf("mix RMS = %.3f, want %.3f", got, wantRMS)
	}
}

func TestMixAmbienceRejectsFormatMismatch(t *testing.T) {
	music, _ := wav.Encode(tone(100, 0.4), 16000, 1)
	amb, _ := wav.Encode(tone(100, 0.4), 44100, 1)
	if _, err := MixAmbience(music, amb); err == nil {
		t.Fatal("want an error mixing layers with different sample rates")
	}
}
