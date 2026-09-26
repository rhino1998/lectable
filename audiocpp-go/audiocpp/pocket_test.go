package audiocpp_test

import (
	"math"
	"os"
	"reflect"
	"testing"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
)

// TestPocketTTSCodec needs AUDIOCPP_POCKET_TTS_MODEL (a PocketTTS package dir)
// and optionally AUDIOCPP_TEST_BACKEND (default cpu).
func TestPocketTTSCodec(t *testing.T) {
	modelPath := os.Getenv("AUDIOCPP_POCKET_TTS_MODEL")
	if modelPath == "" {
		t.Skip("AUDIOCPP_POCKET_TTS_MODEL not set")
	}
	backend := audiocpp.BackendConfig{Backend: os.Getenv("AUDIOCPP_TEST_BACKEND"), Threads: 8}
	if backend.Backend == "" {
		backend.Backend = "cpu"
	}
	registry, err := audiocpp.NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	model, err := registry.LoadModel(modelPath, audiocpp.ModelConfig{FamilyHint: "pocket_tts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	if !model.Supports("codec", "offline") {
		t.Fatal(`model doesn't advertise the "codec" task`)
	}
	run := func(task string, build func(*audiocpp.Request)) *audiocpp.Result {
		t.Helper()
		session, err := model.Session(task, "offline", backend, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		req := audiocpp.NewRequest()
		defer req.Close()
		build(req)
		if err := req.Err(); err != nil {
			t.Fatal(err)
		}
		if err := session.Prepare(req); err != nil {
			t.Fatal(err)
		}
		res, err := session.Run(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(res.Close)
		return res
	}
	audioOf := func(res *audiocpp.Result) *audiocpp.AudioBuffer {
		t.Helper()
		a, err := res.Audio()
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	latentsOf := func(res *audiocpp.Result) *audiocpp.Latents {
		t.Helper()
		l, err := res.Latents()
		if err != nil || len(l) != 1 {
			t.Fatalf("latents = %d, %v; want 1", len(l), err)
		}
		return l[0]
	}

	gen := run("tts", func(r *audiocpp.Request) {
		r.SetText("The lantern flickered as she stepped into the hall, listening for footsteps.", "")
		r.SetVoiceID("alba") // the package's built-in voice
		r.SetOption("return_latents", "true")
	})
	genAudio, lat := audioOf(gen), latentsOf(gen)
	if lat.Dim != 32 || lat.HopSamples != 1920 || lat.Meta["normalized"] != "true" {
		t.Fatalf("unexpected latents header: dim=%d hop=%d meta=%v", lat.Dim, lat.HopSamples, lat.Meta)
	}
	decoded := audioOf(run("codec", func(r *audiocpp.Request) {
		if err := r.AddLatents(lat); err != nil {
			t.Fatal(err)
		}
	}))
	if !reflect.DeepEqual(decoded.Samples, genAudio.Samples) {
		t.Fatalf("codec decode differs from generated audio (%d vs %d samples)", len(decoded.Samples), len(genAudio.Samples))
	}

	// Encoding the generated audio should land near the generator's own
	// latents - it's the same (normalized) space.
	enc := latentsOf(run("codec", func(r *audiocpp.Request) {
		r.SetAudio(genAudio.Samples, genAudio.SampleRate, genAudio.Channels)
	}))
	n := min(enc.Frames, lat.Frames)
	if n < lat.Frames-1 {
		t.Fatalf("encode gave %d frames for %d generated", enc.Frames, lat.Frames)
	}
	var dot, na, nb, diff float64
	for i := 0; i < n*lat.Dim; i++ {
		a, b := float64(lat.Data[i]), float64(enc.Data[i])
		dot, na, nb, diff = dot+a*b, na+a*a, nb+b*b, diff+(a-b)*(a-b)
	}
	cos := dot / math.Sqrt(na*nb)
	t.Logf("encode(decode(z)) vs z over %d frames: cosine %.4f, relative L2 %.3f", n, cos, math.Sqrt(diff/na))
	if cos < 0.5 {
		t.Errorf("encoded latents don't resemble the generated ones (cosine %.3f): encoder space mismatch?", cos)
	}
	back := audioOf(run("codec", func(r *audiocpp.Request) {
		if err := r.AddLatents(enc); err != nil {
			t.Fatal(err)
		}
	}))
	if back.Frames() != genAudio.Frames() {
		t.Errorf("encode+decode gave %d frames, want %d", back.Frames(), genAudio.Frames())
	}
}
