package lac

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// signal returns deterministic test audio: two sines plus LCG noise,
// with a stretch of silence and a full-scale square burst.
func signal(frames, ch int) []int16 {
	pcm := make([]int16, frames*ch)
	s := uint32(12345)
	for i := range frames {
		for c := range ch {
			s = s*1664525 + 1013904223
			v := 6000*math.Sin(float64(i)*0.031*float64(c+1)) + 2500*math.Sin(float64(i)*0.0071) + float64(int32(s>>22)-512)
			switch {
			case i > frames/2 && i < frames/2+500:
				v = 0
			case i > frames*3/4 && i < frames*3/4+300:
				v = 32767
				if (i/7)%2 == 0 {
					v = -32768
				}
			}
			pcm[i*ch+c] = int16(v)
		}
	}
	return pcm
}

func roundTrip(t *testing.T, pcm []int16, ch, rate int, p Preset) []byte {
	t.Helper()
	b, err := Encode(pcm, ch, rate, p)
	if err != nil {
		t.Fatal(err)
	}
	got, f, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if f.Channels != ch || f.SampleRate != rate || f.Frames != len(pcm)/ch {
		t.Fatalf("format %+v, want %d ch %d Hz %d frames", f, ch, rate, len(pcm)/ch)
	}
	if len(got) != len(pcm) {
		t.Fatalf("decoded %d samples, want %d", len(got), len(pcm))
	}
	for i := range got {
		if got[i] != pcm[i] {
			t.Fatalf("sample %d: %d, want %d", i, got[i], pcm[i])
		}
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	for _, p := range []Preset{PresetAuto, PresetSpeech, PresetMusic} {
		for _, ch := range []int{1, 2, 5} {
			for _, frames := range []int{0, 1, 17, 30000} {
				roundTrip(t, signal(frames, ch), ch, 24000, p)
			}
		}
	}
}

func TestExtremes(t *testing.T) {
	n := 20000
	cases := map[string]func(i int) int16{
		"silence":     func(int) int16 { return 0 },
		"dc max":      func(int) int16 { return 32767 },
		"dc min":      func(int) int16 { return -32768 },
		"nyquist":     func(i int) int16 { return int16(32767 - 65535*(i%2)) },
		"impulses":    func(i int) int16 { return map[bool]int16{true: -32768, false: 0}[i%997 == 0] },
		"white noise": func(i int) int16 { return int16(uint32(i) * 2654435761 >> 16) },
	}
	for name, f := range cases {
		pcm := make([]int16, 2*n)
		for i := range pcm {
			pcm[i] = f(i)
		}
		for _, p := range []Preset{PresetSpeech, PresetMusic} {
			t.Run(name, func(t *testing.T) { roundTrip(t, pcm, 2, 44100, p) })
		}
	}
}

// TestGolden pins the exact encoder output. If it fails, the arithmetic
// changed: existing files may no longer decode. Don't update the hashes
// without bumping the format version (and keeping the old decoder).
func TestGolden(t *testing.T) {
	want := map[Preset]string{
		PresetSpeech: goldenSpeech,
		PresetMusic:  goldenMusic,
	}
	for p, h := range want {
		b, err := Encode(signal(40000, 2), 2, 44100, p)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != h {
			t.Errorf("preset %d: encoding hash %s, want %s", p, got, h)
		}
	}
}

func TestCorrupt(t *testing.T) {
	pcm := signal(5000, 2)
	b, err := Encode(pcm, 2, 44100, PresetSpeech)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode(b[:len(b)/2]); !errors.Is(err, ErrCorrupt) {
		t.Errorf("truncated: err = %v, want ErrCorrupt", err)
	}
	// A flipped payload byte must be caught - or, in the range coder's
	// final flush bytes, not matter at all.
	for i := fixedHeader + 2*stageHeader; i < len(b); i += 97 {
		c := bytes.Clone(b)
		c[i] ^= 0x40
		got, _, err := Decode(c)
		if !errors.Is(err, ErrCorrupt) && (err != nil || !equal(got, pcm)) {
			t.Errorf("flipped byte %d: err = %v, output changed = %v", i, err, !equal(got, pcm))
		}
	}
	if _, _, err := Decode([]byte("RIFF")); !errors.Is(err, ErrCorrupt) {
		t.Errorf("garbage: err = %v", err)
	}
}

func FuzzDecode(f *testing.F) {
	for _, p := range []Preset{PresetSpeech, PresetMusic} {
		b, _ := Encode(signal(300, 2), 2, 8000, p)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if h, err := parseHeader(b); err == nil && h.format.Frames*h.format.Channels > 1<<16 {
			t.Skip() // keep iterations fast
		}
		_, _, _ = Decode(b) // must not panic
	})
}

func TestWAV(t *testing.T) {
	pcm := signal(1000, 2)
	b, err := Encode(pcm, 2, 44100, PresetMusic)
	if err != nil {
		t.Fatal(err)
	}
	wav, err := DecodeWAV(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := EncodeWAV(wav, PresetMusic)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, b2) {
		t.Fatal("WAV round trip changed the stream")
	}
}

// TestSamples round-trips real WAVs when LAC_SAMPLES points at a
// directory of them (e.g. copies of backend/data audio).
func TestSamples(t *testing.T) {
	dir := os.Getenv("LAC_SAMPLES")
	if dir == "" {
		t.Skip("LAC_SAMPLES not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.wav"))
	for _, f := range files {
		wav, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b, err := EncodeWAV(wav, PresetAuto)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		pcm, ch, _, _ := parseWAV(wav)
		got, format, err := Decode(b)
		if err != nil || format.Channels != ch || !equal(got, pcm) {
			t.Fatalf("%s: round trip failed (%v)", f, err)
		}
	}
}

func equal(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func BenchmarkDecode(b *testing.B) {
	for _, bc := range []struct {
		name string
		ch   int
		rate int
		p    Preset
	}{{"speech-mono24k", 1, 24000, PresetSpeech}, {"music-stereo44k", 2, 44100, PresetMusic}} {
		pcm := signal(bc.rate*10, bc.ch)
		enc, _ := Encode(pcm, bc.ch, bc.rate, bc.p)
		b.Run(bc.name, func(b *testing.B) {
			for b.Loop() {
				if _, _, err := Decode(enc); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(10/(b.Elapsed().Seconds()/float64(b.N)), "x_realtime")
		})
	}
}
