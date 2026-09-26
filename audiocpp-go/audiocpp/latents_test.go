package audiocpp_test

import (
	"math"
	"os"
	"reflect"
	"testing"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
)

func TestLatentsArtifactRoundTrip(t *testing.T) {
	in := &audiocpp.Latents{
		Frames: 3, Dim: 2,
		Data:       []float32{0, -1.5, 2.25, float32(math.Pi), float32(math.Copysign(0, -1)), 1e-30},
		HopSamples: 4096, SampleRate: 44100, Family: "stable_audio",
		Meta: map[string]string{"decode_seed": "7", "frames": "ignored: reserved"},
	}
	a, err := in.Artifact()
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Latents()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Data, in.Data) || out.Frames != 3 || out.Dim != 2 || out.HopSamples != 4096 ||
		out.SampleRate != 44100 || out.Family != "stable_audio" || !reflect.DeepEqual(out.Meta, map[string]string{"decode_seed": "7"}) {
		t.Fatalf("round trip changed latents: %+v", out)
	}
	s, err := out.Slice(1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Data, in.Data[2:]) || s.Frames != 2 || s.Meta != nil {
		t.Fatalf("Slice(1, 3) = %+v", s)
	}
	if _, err := out.Slice(2, 4); err == nil {
		t.Error("Slice past the end succeeded")
	}
	a.Payload = a.Payload[:len(a.Payload)-4]
	if _, err := a.Latents(); err == nil {
		t.Error("short payload decoded without error")
	}
}

// TestStableAudioCodec needs a Stable Audio 3 package:
// AUDIOCPP_STABLE_AUDIO_MODEL=/path/to/Stable-Audio-3-Medium-GGUF, and
// optionally AUDIOCPP_TEST_BACKEND (default cpu; decode equality is only
// expected within one backend).
func TestStableAudioCodec(t *testing.T) {
	modelPath := os.Getenv("AUDIOCPP_STABLE_AUDIO_MODEL")
	if modelPath == "" {
		t.Skip("AUDIOCPP_STABLE_AUDIO_MODEL not set")
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
	model, err := registry.LoadModel(modelPath, audiocpp.ModelConfig{FamilyHint: "stable_audio"}, nil)
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
		defer session.Close() // results outlive their session; don't stack model copies
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

	// Generate with return_latents.
	gen := run("gen", func(r *audiocpp.Request) {
		r.SetText("soft rain on leaves, ambience", "")
		r.SetOption("duration_seconds", "3").SetOption("num_inference_steps", "2").SetOption("seed", "5")
		r.SetOption("return_latents", "true")
	})
	genAudio, err := gen.Audio()
	if err != nil {
		t.Fatal(err)
	}
	lats, err := gen.Latents()
	if err != nil || len(lats) != 1 {
		t.Fatalf("gen latents = %d, %v; want 1", len(lats), err)
	}
	lat := lats[0]
	if lat.Dim != 256 || lat.HopSamples != 4096 || lat.SampleRate != 44100 || lat.Meta["decode_seed"] != "5" {
		t.Fatalf("unexpected latents header: frames=%d dim=%d hop=%d rate=%d meta=%v", lat.Frames, lat.Dim, lat.HopSamples, lat.SampleRate, lat.Meta)
	}

	// Standalone decode reproduces the generated audio exactly.
	dec := run("codec", func(r *audiocpp.Request) {
		if err := r.AddLatents(lat); err != nil {
			t.Fatal(err)
		}
	})
	decAudio, err := dec.Audio()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decAudio.Samples, genAudio.Samples) || decAudio.Channels != genAudio.Channels {
		t.Fatalf("codec decode differs from gen output (%d vs %d samples)", len(decAudio.Samples), len(genAudio.Samples))
	}

	// Standalone encode: whole frames covering the input.
	enc := run("codec", func(r *audiocpp.Request) {
		r.SetAudio(genAudio.Samples, genAudio.SampleRate, genAudio.Channels)
	})
	encLats, err := enc.Latents()
	if err != nil || len(encLats) != 1 {
		t.Fatalf("encode latents = %d, %v; want 1", len(encLats), err)
	}
	wantFrames := (genAudio.Frames() + 4095) / 4096
	if encLats[0].Frames != wantFrames || encLats[0].Dim != 256 {
		t.Fatalf("encode gave %d x %d, want %d x 256", encLats[0].Frames, encLats[0].Dim, wantFrames)
	}

	// Inpaint from a latents slice in place of seed audio.
	seed, err := lat.Slice(0, 16)
	if err != nil {
		t.Fatal(err)
	}
	inp := run("gen", func(r *audiocpp.Request) {
		r.SetText("soft rain on leaves, ambience", "")
		r.SetOption("duration_seconds", "3").SetOption("num_inference_steps", "2").SetOption("seed", "6")
		r.SetOption("audio_input_kind", "inpaint_audio")
		r.SetOption("inpaint_mask_start_seconds", "1.5").SetOption("inpaint_mask_end_seconds", "3")
		if err := r.AddLatents(seed); err != nil {
			t.Fatal(err)
		}
	})
	inpAudio, err := inp.Audio()
	if err != nil {
		t.Fatal(err)
	}
	if inpAudio.Frames() != genAudio.Frames() {
		t.Fatalf("inpaint output %d frames, want %d", inpAudio.Frames(), genAudio.Frames())
	}
}
