package latents

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

func floats(vs ...float32) []byte {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

func ints(vs ...int32) []byte {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.LittleEndian.PutUint32(b[4*i:], uint32(v))
	}
	return b
}

func TestRoundTripExact(t *testing.T) {
	for _, tc := range []struct {
		l     *ttsproto.Latents
		dtype string
	}{
		{&ttsproto.Latents{Kind: KindLatents, Frames: 2, Dim: 2, HopSamples: 1920, SampleRate: 24000, Family: "pocket_tts",
			Data: floats(1.5, -2, float32(math.Pi), 1e-30), Meta: map[string]string{"normalized": "true"}}, ""},
		{&ttsproto.Latents{Kind: KindCodes, Frames: 2, Dim: 3, CodebookSize: 1024, HopSamples: 960, SampleRate: 24000, Family: "higgs_audio_tts",
			Data: ints(0, 1023, 5, 7, 512, 9), Meta: map[string]string{"chunk_frames": "1,1"}}, ""},
		{&ttsproto.Latents{Kind: KindCodes, Frames: 1, Dim: 2, Data: ints(70000, -3)}, ""},                                    // i32 (no codebook size)
		{&ttsproto.Latents{Kind: KindCodes, Frames: 2, Dim: 3, CodebookSize: 1024, Data: ints(0, 1023, 5, 7, 512, 9)}, "i16"}, // pre-packing sidecars still read
	} {
		b, err := Encode(tc.l, tc.dtype)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.l) {
			t.Fatalf("round trip = %+v, want %+v", got, tc.l)
		}
	}
}

func TestFloat16(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 100000; i++ {
		f := float32(r.NormFloat64() * 3)
		got := fromFloat16(toFloat16(f))
		if math.Abs(float64(got-f)) > math.Abs(float64(f))/2048+1e-7 {
			t.Fatalf("f16(%g) = %g", f, got)
		}
	}
	for _, f := range []float32{0, 1, -1, 65504, 0.5, 6.1035156e-05, 5.9604645e-08} {
		if got := fromFloat16(toFloat16(f)); got != f {
			t.Errorf("f16 exact value %g -> %g", f, got)
		}
	}
	if got := fromFloat16(toFloat16(1e6)); !math.IsInf(float64(got), 1) {
		t.Errorf("overflow -> %g, want +Inf", got)
	}
}

func TestSliceConcatFile(t *testing.T) {
	a := &ttsproto.Latents{Kind: KindCodes, Frames: 3, Dim: 1, CodebookSize: 1024, Family: "higgs_audio_tts", Data: ints(1, 2, 3), Meta: map[string]string{"chunk_frames": "2,1"}}
	b := &ttsproto.Latents{Kind: KindCodes, Frames: 2, Dim: 1, CodebookSize: 1024, Family: "higgs_audio_tts", Data: ints(4, 5)}
	c, err := Concat(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Frames != 5 || c.Meta["chunk_frames"] != "2,1,2" || !reflect.DeepEqual(c.Data, ints(1, 2, 3, 4, 5)) {
		t.Fatalf("Concat = %+v", c)
	}
	if _, err := Concat(a, &ttsproto.Latents{Kind: KindCodes, Frames: 1, Dim: 1, Data: ints(1), Meta: map[string]string{"decode_seed": "3"}}); err == nil {
		t.Error("Concat accepted a piece with window-specific meta")
	}
	s, err := Slice(c, 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Data, ints(2, 3, 4)) || s.Meta != nil {
		t.Fatalf("Slice = %+v", s)
	}
	path := filepath.Join(t.TempDir(), "x.lat")
	if err := WriteFile(path, c, ""); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("ReadFile = %+v, %v", got, err)
	}
	if Sidecar("/a/b/00012.opus") != "/a/b/00012.lat" {
		t.Error(Sidecar("/a/b/00012.opus"))
	}
}

func TestPackedCodes(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for _, size := range []int{2, 1024, 2048, 1000, 1 << 20} {
		vals := make([]int32, 999)
		for i := range vals {
			vals[i] = int32(r.IntN(size))
		}
		l := &ttsproto.Latents{Kind: KindCodes, Frames: 333, Dim: 3, CodebookSize: size, Data: ints(vals...)}
		b, err := Encode(l, "")
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil || !reflect.DeepEqual(got, l) {
			t.Fatalf("codebook %d: round trip failed (%v)", size, err)
		}
	}
	// 10-bit Higgs codes: 999 codes -> 1249 payload bytes (vs 1998 as i16).
	l := &ttsproto.Latents{Kind: KindCodes, Frames: 333, Dim: 3, CodebookSize: 1024, Data: ints(make([]int32, 999)...)}
	packed, _ := Encode(l, "")
	i16, _ := Encode(l, "i16")
	if len(i16)-len(packed) != 1998-1249 {
		t.Errorf("packed %d B vs i16 %d B", len(packed), len(i16))
	}
	if _, err := Encode(&ttsproto.Latents{Kind: KindCodes, Frames: 1, Dim: 1, CodebookSize: 1024, Data: ints(1024)}, ""); err == nil {
		t.Error("out-of-range code packed without error")
	}
}
