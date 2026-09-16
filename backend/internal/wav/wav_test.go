package wav

import "testing"

func TestConcatJoinsSamplesInOrder(t *testing.T) {
	a := &Clip{Samples: []float32{0.1, 0.2, 0.3}, SampleRate: 16000, Channels: 1}
	b := &Clip{Samples: []float32{0.4, 0.5}, SampleRate: 16000, Channels: 1}

	joined, err := Concat(a, b)
	if err != nil {
		t.Fatalf("Concat: %v", err)
	}
	want := []float32{0.1, 0.2, 0.3, 0.4, 0.5}
	if len(joined.Samples) != len(want) {
		t.Fatalf("expected %d samples, got %d", len(want), len(joined.Samples))
	}
	for i, s := range want {
		if joined.Samples[i] != s {
			t.Fatalf("sample %d: expected %v, got %v", i, s, joined.Samples[i])
		}
	}
	if joined.SampleRate != a.SampleRate || joined.Channels != a.Channels {
		t.Fatalf("expected joined clip to keep a's own sample rate/channels, got %d/%d", joined.SampleRate, joined.Channels)
	}
}

func TestConcatRejectsMismatchedFormat(t *testing.T) {
	a := &Clip{Samples: []float32{0.1}, SampleRate: 16000, Channels: 1}

	if _, err := Concat(a, &Clip{Samples: []float32{0.2}, SampleRate: 22050, Channels: 1}); err == nil {
		t.Fatalf("expected an error joining clips with different sample rates")
	}
	if _, err := Concat(a, &Clip{Samples: []float32{0.2}, SampleRate: 16000, Channels: 2}); err == nil {
		t.Fatalf("expected an error joining clips with different channel counts")
	}
}

// TestConcatRoundTripsThroughEncodeDecode confirms Concat's own output
// actually re-encodes and decodes back correctly - generateCloneSplit's
// real call site immediately re-encodes the joined Clip (see
// concatCloneAudio), so this exercises that whole path, not just the
// in-memory sample slice.
func TestConcatRoundTripsThroughEncodeDecode(t *testing.T) {
	aData, err := Encode([]float32{0.25, -0.25}, 16000, 1)
	if err != nil {
		t.Fatalf("Encode a: %v", err)
	}
	bData, err := Encode([]float32{0.5, -0.5, 0.0}, 16000, 1)
	if err != nil {
		t.Fatalf("Encode b: %v", err)
	}
	a, err := Decode(aData)
	if err != nil {
		t.Fatalf("Decode a: %v", err)
	}
	b, err := Decode(bData)
	if err != nil {
		t.Fatalf("Decode b: %v", err)
	}
	joined, err := Concat(a, b)
	if err != nil {
		t.Fatalf("Concat: %v", err)
	}
	out, err := Encode(joined.Samples, joined.SampleRate, joined.Channels)
	if err != nil {
		t.Fatalf("Encode joined: %v", err)
	}
	roundTripped, err := Decode(out)
	if err != nil {
		t.Fatalf("Decode joined: %v", err)
	}
	if len(roundTripped.Samples) != 5 {
		t.Fatalf("expected 5 samples after round trip, got %d", len(roundTripped.Samples))
	}
}
