package audiocpp_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
)

// TestBreezeTTSCodec needs AUDIOCPP_BREEZE_TTS_MODEL (a BreezeTTS 2 GGUF) and
// optionally AUDIOCPP_TEST_BACKEND (default cpu; equality checks hold within
// one backend). It checks that generated codes (return_latents) decode back
// to the generated audio exactly, and that a codec encode of a clone
// reference matches the reference codes the clone returns.
func TestBreezeTTSCodec(t *testing.T) {
	modelPath := os.Getenv("AUDIOCPP_BREEZE_TTS_MODEL")
	if modelPath == "" {
		t.Skip("AUDIOCPP_BREEZE_TTS_MODEL not set")
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
	model, err := registry.LoadModel(modelPath, audiocpp.ModelConfig{FamilyHint: "breeze_tts"}, nil)
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
	audioOf := func(res *audiocpp.Result) *audiocpp.AudioBuffer {
		t.Helper()
		a, err := res.Audio()
		if err != nil || a == nil {
			t.Fatalf("audio = %v, %v", a, err)
		}
		return a
	}

	const refText = "The lantern flickered as she stepped into the hall."
	ref := audioOf(run("tts", func(r *audiocpp.Request) {
		r.SetText(refText, "")
		r.SetOption("seed", "3")
	}))

	// Several text chunks, so chunk_frames has to carry the split.
	const text = "Nobody answered, so she climbed the stairs alone. The door at the top stood open."
	gen := run("tts", func(r *audiocpp.Request) {
		r.SetText(text, "")
		r.SetVoiceAudio(ref.Samples, ref.SampleRate, ref.Channels)
		r.SetOption("reference_text", refText).SetOption("seed", "9").SetOption("text_chunk_size", "50")
		r.SetOption("return_latents", "true").SetOption("return_reference_codes", "true")
	})
	codes, err := gen.Codes("codes")
	if err != nil || codes == nil {
		t.Fatalf("gen codes = %v, %v", codes, err)
	}
	if codes.Codebooks != 16 || codes.CodebookSize != 2048 || codes.HopSamples != 1920 || codes.Family != "breeze_tts" {
		t.Fatalf("unexpected codes header: %+v", codes)
	}
	if chunks := strings.Split(codes.Meta["chunk_frames"], ","); len(chunks) < 2 {
		t.Fatalf("chunk_frames = %q, want several chunks", codes.Meta["chunk_frames"])
	}
	decoded := audioOf(run("codec", func(r *audiocpp.Request) {
		if err := r.AddCodes(codes, "codes"); err != nil {
			t.Fatal(err)
		}
	}))
	if got, want := decoded.Samples, audioOf(gen).Samples; !reflect.DeepEqual(got, want) {
		t.Fatalf("codec decode differs from generated audio (%d vs %d samples)", len(got), len(want))
	}

	refCodes, err := gen.Codes("reference_codes")
	if err != nil || refCodes == nil {
		t.Fatalf("reference codes = %v, %v", refCodes, err)
	}
	encoded, err := run("codec", func(r *audiocpp.Request) {
		r.SetAudio(ref.Samples, ref.SampleRate, ref.Channels)
	}).Codes("codes")
	if err != nil || encoded == nil {
		t.Fatalf("encode codes = %v, %v", encoded, err)
	}
	if !reflect.DeepEqual(encoded.Data, refCodes.Data) {
		t.Error("codec encode differs from the clone's returned reference codes")
	}
}
