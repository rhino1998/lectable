package audiocpp_test

// Smoke test for the Go bindings, mirroring audio.cpp's own
// tests/capi/path_test.c and bindings/python/tests/test_smoke.py: runs
// Silero VAD (bundled in an audio.cpp checkout, no model download needed)
// over assets/resources/sample_16k.wav, offline and streaming.
//
// These bindings only vendor audiocpp.h (the C ABI header, for cgo to
// compile against) -- they do not vendor audio.cpp's model assets or its
// native build. Point AUDIOCPP_CHECKOUT at a local audio.cpp checkout with
// the C API built (see README.md) to run this test; it's skipped otherwise.
//
//	cd /path/to/audio.cpp
//	cmake -S . -B build -DAUDIOCPP_BUILD_C_API=ON
//	cmake --build build --target audiocpp
//	cd /path/to/lectable/audiocpp-go
//	AUDIOCPP_CHECKOUT=/path/to/audio.cpp \
//	CGO_LDFLAGS="-L/path/to/audio.cpp/build/bin" \
//	LD_LIBRARY_PATH="/path/to/audio.cpp/build/bin" \
//	    go test ./audiocpp/...

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
)

// audioCPPCheckout returns the root of a local audio.cpp checkout (with
// assets/ and a built C API), or skips the calling test when
// AUDIOCPP_CHECKOUT isn't set to one.
func audioCPPCheckout(t *testing.T) string {
	t.Helper()
	root := os.Getenv("AUDIOCPP_CHECKOUT")
	if root == "" {
		t.Skip("AUDIOCPP_CHECKOUT not set; point it at a local audio.cpp checkout to run this test (see smoke_test.go)")
	}
	if _, err := os.Stat(filepath.Join(root, "assets", "resources", "sample_16k.wav")); err != nil {
		t.Skipf("AUDIOCPP_CHECKOUT=%s does not look like an audio.cpp checkout: %v", root, err)
	}
	return root
}

func TestABIVersionMatchesHeader(t *testing.T) {
	major, minor, _ := audiocpp.ABIVersionParts()
	if major != 0 {
		t.Fatalf("major = %d, want 0", major)
	}
	if minor < 2 {
		t.Fatalf("minor = %d, want >= 2", minor)
	}
	if audiocpp.BuildVersion() == "" {
		t.Fatal("BuildVersion() is empty")
	}
}

func TestTaskVocabularyMapsCloneAndDesign(t *testing.T) {
	names := audiocpp.TaskNames()
	has := func(want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
	}
	if !has("clon") {
		t.Error(`TaskNames() missing "clon"`)
	}
	if !has("vdes") {
		t.Error(`TaskNames() missing "vdes"`)
	}
	if !has("codec") {
		t.Error(`TaskNames() missing "codec"`)
	}
	if got, ok := audiocpp.TaskFromSpecName("clone"); !ok || got != "clon" {
		t.Errorf(`TaskFromSpecName("clone") = %q, %v; want "clon", true`, got, ok)
	}
	if got, ok := audiocpp.TaskFromSpecName("design"); !ok || got != "vdes" {
		t.Errorf(`TaskFromSpecName("design") = %q, %v; want "vdes", true`, got, ok)
	}
	if _, ok := audiocpp.TaskFromSpecName("nonsense"); ok {
		t.Error(`TaskFromSpecName("nonsense") reported ok, want false`)
	}
}

func TestRegistryListsSileroVAD(t *testing.T) {
	registry, err := audiocpp.NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	found := false
	for _, f := range registry.Families() {
		if f == "silero_vad" {
			found = true
		}
	}
	if !found {
		t.Errorf("registry.Families() = %v, missing silero_vad", registry.Families())
	}
}

func TestOfflineVADFindsSpeech(t *testing.T) {
	root := audioCPPCheckout(t)
	modelPath := filepath.Join(root, "assets", "framework", "models", "silero_vad")
	audioPath := filepath.Join(root, "assets", "resources", "sample_16k.wav")

	clip, err := audiocpp.ReadWavFloat32(audioPath)
	if err != nil {
		t.Fatal(err)
	}

	registry, err := audiocpp.NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	model, err := registry.LoadModel(modelPath, audiocpp.ModelConfig{FamilyHint: "silero_vad"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	if got := model.Family(); got != "silero_vad" {
		t.Errorf("model.Family() = %q, want silero_vad", got)
	}
	if !model.Supports("vad", "offline") {
		t.Error("model.Supports(vad, offline) = false, want true")
	}
	if model.Supports("tts", "offline") {
		t.Error("model.Supports(tts, offline) = true, want false")
	}

	session, err := model.Session("vad", "offline", audiocpp.BackendConfig{Backend: "cpu", Threads: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	request := audiocpp.NewRequest()
	defer request.Close()
	request.SetAudio(clip.Samples, clip.SampleRate, clip.Channels)
	if err := request.Err(); err != nil {
		t.Fatal(err)
	}

	result, err := session.Run(request)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()

	segments, err := result.Segments()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) == 0 {
		t.Fatal("result.Segments() is empty, want at least one speech segment")
	}

	prevEnd := int64(-1)
	for _, seg := range segments {
		if seg.StartSample < 0 {
			t.Errorf("segment.StartSample = %d, want >= 0", seg.StartSample)
		}
		if seg.EndSample < seg.StartSample {
			t.Errorf("segment.EndSample %d < StartSample %d", seg.EndSample, seg.StartSample)
		}
		if seg.EndSample > int64(clip.Frames()) {
			t.Errorf("segment.EndSample %d > clip frames %d", seg.EndSample, clip.Frames())
		}
		if seg.StartSample < prevEnd {
			t.Errorf("segment.StartSample %d < previous EndSample %d (out of order)", seg.StartSample, prevEnd)
		}
		prevEnd = seg.EndSample
	}

	audio, err := result.Audio()
	if err != nil {
		t.Fatal(err)
	}
	if audio != nil {
		t.Error("result.Audio() is non-nil, VAD should produce no audio output")
	}
	text, err := result.Text()
	if err != nil {
		t.Fatal(err)
	}
	if text != nil {
		t.Error("result.Text() is non-nil, VAD should produce no text output")
	}

	// Reusing the session must reproduce the same result.
	request2 := audiocpp.NewRequest()
	defer request2.Close()
	request2.SetAudio(clip.Samples, clip.SampleRate, clip.Channels)
	result2, err := session.Run(request2)
	if err != nil {
		t.Fatal(err)
	}
	defer result2.Close()
	segments2, err := result2.Segments()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments2) != len(segments) {
		t.Fatalf("repeat run produced %d segments, want %d", len(segments2), len(segments))
	}
	for i := range segments {
		if segments[i].StartSample != segments2[i].StartSample || segments[i].EndSample != segments2[i].EndSample {
			t.Errorf("segment %d differs between runs: %+v vs %+v", i, segments[i], segments2[i])
		}
	}
}

func TestStreamingVADProducesActivityOrSegments(t *testing.T) {
	root := audioCPPCheckout(t)
	modelPath := filepath.Join(root, "assets", "framework", "models", "silero_vad")
	audioPath := filepath.Join(root, "assets", "resources", "sample_16k.wav")

	clip, err := audiocpp.ReadWavFloat32(audioPath)
	if err != nil {
		t.Fatal(err)
	}

	registry, err := audiocpp.NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	model, err := registry.LoadModel(modelPath, audiocpp.ModelConfig{FamilyHint: "silero_vad"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	if !model.Supports("vad", "streaming") {
		t.Skip("silero_vad build does not advertise vad/streaming")
	}

	session, err := model.Session("vad", "streaming", audiocpp.BackendConfig{Backend: "cpu", Threads: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	policy, err := session.StreamPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.InputKind != audiocpp.StreamInputAudioChunks {
		t.Errorf("policy.InputKind = %v, want StreamInputAudioChunks", policy.InputKind)
	}
	if policy.ChunkSamples <= 0 {
		t.Fatalf("policy.ChunkSamples = %d, want > 0", policy.ChunkSamples)
	}

	if err := session.StreamStart(nil); err != nil {
		t.Fatal(err)
	}

	offset := 0
	activityEvents := 0
	chunk := int(policy.ChunkSamples)
	frames := clip.Frames()
	for offset+chunk <= frames {
		block := clip.Samples[offset*clip.Channels : (offset+chunk)*clip.Channels]
		event, err := session.StreamPush(block, clip.SampleRate, clip.Channels, int64(offset))
		if err != nil {
			t.Fatal(err)
		}
		if event != nil {
			activity, err := event.VoiceActivity()
			if err != nil {
				t.Fatal(err)
			}
			activityEvents += len(activity)
			for _, a := range activity {
				if a.Probability < 0.0 || a.Probability > 1.0 {
					t.Errorf("activity.Probability = %v, want in [0, 1]", a.Probability)
				}
			}
			event.Close()
		}
		offset += chunk
	}

	result, err := session.StreamFinish()
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	segments, err := result.Segments()
	if err != nil {
		t.Fatal(err)
	}
	if activityEvents == 0 && len(segments) == 0 {
		t.Fatal("streaming run produced neither voice-activity events nor segments")
	}
}

func TestModelOptionIntrospectionIsWellFormed(t *testing.T) {
	root := audioCPPCheckout(t)
	modelPath := filepath.Join(root, "assets", "framework", "models", "silero_vad")

	registry, err := audiocpp.NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	model, err := registry.LoadModel(modelPath, audiocpp.ModelConfig{FamilyHint: "silero_vad"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	for _, scope := range []audiocpp.OptionScope{audiocpp.OptionScopeRequest, audiocpp.OptionScopeSession, audiocpp.OptionScopeLoad} {
		opts, err := model.Options(scope)
		if err != nil {
			t.Fatal(err)
		}
		for _, opt := range opts {
			if opt.Name == "" {
				t.Errorf("scope %v: option has empty Name", scope)
			}
		}
	}
}

func TestCloneAndDesignRequestSurface(t *testing.T) {
	root := audioCPPCheckout(t)
	audioPath := filepath.Join(root, "assets", "resources", "sample_16k.wav")
	clip, err := audiocpp.ReadWavFloat32(audioPath)
	if err != nil {
		t.Fatal(err)
	}

	clone := audiocpp.NewRequest()
	defer clone.Close()
	clone.SetText("Hello from audio.cpp.", "en-us").
		SetVoiceAudio(clip.Samples, clip.SampleRate, clip.Channels).
		SetVoiceID("cached-voice-id")
	if err := clone.Err(); err != nil {
		t.Fatal(err)
	}

	design := audiocpp.NewRequest()
	defer design.Close()
	design.SetText("Hello from audio.cpp.", "en-us").
		SetEmotion("cheerful").
		SetSpeakingRate(1.1).
		SetPitchShift(0.0).
		SetEnergyScale(1.0).
		SetStyleLanguage("en-us").
		SetStyleTag("voice-description", "a warm, low-pitched narrator")
	if err := design.Err(); err != nil {
		t.Fatal(err)
	}
}
