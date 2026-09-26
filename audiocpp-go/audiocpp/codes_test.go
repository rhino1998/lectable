package audiocpp_test

import (
	"os"
	"reflect"
	"testing"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
)

func TestCodesArtifactRoundTrip(t *testing.T) {
	in := &audiocpp.Codes{
		Frames: 2, Codebooks: 3, CodebookSize: 1024,
		Data:       []int32{0, 1023, 7, 512, 3, 9},
		HopSamples: 960, SampleRate: 24000, Family: "higgs_audio_tts",
		Meta: map[string]string{"chunk_frames": "1,1"},
	}
	a, err := in.Artifact("codes")
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Codes()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

// TestHiggsCodec needs AUDIOCPP_HIGGS_MODEL (a Higgs Audio TTS GGUF) and
// optionally AUDIOCPP_TEST_BACKEND (default cpu; equality checks hold within
// one backend).
func TestHiggsCodec(t *testing.T) {
	modelPath := os.Getenv("AUDIOCPP_HIGGS_MODEL")
	if modelPath == "" {
		t.Skip("AUDIOCPP_HIGGS_MODEL not set")
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
	model, err := registry.LoadModel(modelPath, audiocpp.ModelConfig{FamilyHint: "higgs_audio_tts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	if !model.Supports("codec", "offline") {
		t.Fatal(`model doesn't advertise the "codec" task`)
	}

	run := func(task string, sessionOpts map[string]string, build func(*audiocpp.Request)) *audiocpp.Result {
		t.Helper()
		var opts *audiocpp.Options
		if sessionOpts != nil {
			if opts, err = audiocpp.NewOptions(sessionOpts); err != nil {
				t.Fatal(err)
			}
			defer opts.Close()
		}
		session, err := model.Session(task, "offline", backend, opts)
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
	audioOf := func(res *audiocpp.Result) *audiocpp.AudioBuffer {
		t.Helper()
		a, err := res.Audio()
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	decode := func(c *audiocpp.Codes) *audiocpp.AudioBuffer {
		t.Helper()
		return audioOf(run("codec", nil, func(r *audiocpp.Request) {
			if err := r.AddCodes(c, "codes"); err != nil {
				t.Fatal(err)
			}
		}))
	}

	const refText = "The lantern flickered as she stepped into the hall."
	for _, batch := range []string{"1", "2"} {
		t.Run("decode_batch_size="+batch, func(t *testing.T) {
			gen := run("tts", map[string]string{"higgs_audio_tts.decode_batch_size": batch}, func(r *audiocpp.Request) {
				r.SetText(refText, "")
				r.SetOption("seed", "3").SetOption("return_latents", "true")
			})
			codes, err := gen.Codes("codes")
			if err != nil || codes == nil {
				t.Fatalf("gen codes = %v, %v", codes, err)
			}
			if codes.Codebooks != 8 || codes.HopSamples != 960 || codes.Meta["chunk_frames"] == "" {
				t.Fatalf("unexpected codes header: %+v", codes.Meta)
			}
			if got, want := decode(codes).Samples, audioOf(gen).Samples; !reflect.DeepEqual(got, want) {
				t.Fatalf("codec decode differs from generated audio (%d vs %d samples)", len(got), len(want))
			}
		})
	}

	// A clone's returned reference codes match a standalone encode of the
	// reference, and cloning from them reproduces cloning from the audio.
	ref := audioOf(run("tts", nil, func(r *audiocpp.Request) {
		r.SetText(refText, "")
		r.SetOption("seed", "3")
	}))
	encoded, err := run("codec", nil, func(r *audiocpp.Request) {
		r.SetAudio(ref.Samples, ref.SampleRate, ref.Channels)
	}).Codes("codes")
	if err != nil || encoded == nil {
		t.Fatalf("encode codes = %v, %v", encoded, err)
	}
	const text = "Nothing answered but the wind."
	fromAudio := run("tts", nil, func(r *audiocpp.Request) {
		r.SetText(text, "")
		r.SetVoiceAudio(ref.Samples, ref.SampleRate, ref.Channels)
		r.SetOption("reference_text", refText).SetOption("seed", "9").SetOption("return_reference_codes", "true")
	})
	refCodes, err := fromAudio.Codes("reference_codes")
	if err != nil || refCodes == nil {
		t.Fatalf("reference codes = %v, %v", refCodes, err)
	}
	if !reflect.DeepEqual(refCodes.Data, encoded.Data) {
		t.Error("returned reference codes differ from a codec encode of the same audio")
	}
	fromCodes := run("tts", nil, func(r *audiocpp.Request) {
		r.SetText(text, "")
		if err := r.AddCodes(refCodes, "reference_codes"); err != nil {
			t.Fatal(err)
		}
		r.SetOption("reference_text", refText).SetOption("seed", "9")
	})
	if !reflect.DeepEqual(audioOf(fromCodes).Samples, audioOf(fromAudio).Samples) {
		t.Error("cloning from reference_codes differs from cloning from the reference audio")
	}
}
