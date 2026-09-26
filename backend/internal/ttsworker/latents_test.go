package ttsworker_test

import (
	"path/filepath"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
	"github.com/rhino1998/lectable/backend/internal/ttsworker/ttsworkertest"
)

func codes(frames int, codecModel string, fill int32) *ttsproto.Latents {
	data := make([]byte, 4*frames)
	for i := range frames {
		data[4*i] = byte(fill)
	}
	return &ttsproto.Latents{Kind: latents.KindCodes, Frames: frames, Dim: 1, CodebookSize: 1024, Family: "higgs_audio_tts",
		Data: data, Meta: map[string]string{"codec_model": codecModel}}
}

func TestGenerateChunkedAudioCachesReferenceCodes(t *testing.T) {
	fake := ttsworkertest.New(t)
	refCodes := codes(3, "m1", 7)
	fake.OnGenerateLatents = func(req ttsproto.GenerateRequest) (*ttsproto.Latents, *ttsproto.Latents) {
		// A worker only encodes (and returns) the reference when the
		// cached codes it was sent don't match its model.
		if req.ReferenceCodes != nil && req.ReferenceCodes.Meta["codec_model"] == refCodes.Meta["codec_model"] {
			return codes(5, "m1", 1), nil
		}
		return codes(5, "m1", 1), refCodes
	}
	dir := t.TempDir()
	mgr := fake.ManagerWith(ttsworker.Config{RefCodesDir: dir})
	ref := []byte("reference wav bytes")

	a, err := mgr.GenerateChunkedAudio(t.Context(), "hello", "audiocpp-higgs", ref, "ref text", "en", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Latents == nil || a.Latents.Frames != 5 || len(a.WAV) == 0 {
		t.Fatalf("audio = %d bytes, latents %+v", len(a.WAV), a.Latents)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"+latents.Ext))
	if len(files) != 1 {
		t.Fatalf("%d cache files, want 1", len(files))
	}
	if _, err := mgr.GenerateChunkedAudio(t.Context(), "again", "audiocpp-higgs", ref, "ref text", "en", "", 0); err != nil {
		t.Fatal(err)
	}
	calls := fake.GenerateCalls()
	if calls[0].ReferenceCodes != nil || calls[1].ReferenceCodes == nil || calls[1].ReferenceCodes.Frames != 3 {
		t.Fatalf("reference codes sent: first %v, second %v", calls[0].ReferenceCodes, calls[1].ReferenceCodes)
	}
	if calls[1].RefAudioBase64 == "" {
		t.Error("reference audio must still be sent as the fallback")
	}

	// A model change: the worker re-encodes and the entry is replaced.
	refCodes = codes(4, "m2", 9)
	if _, err := mgr.GenerateChunkedAudio(t.Context(), "third", "audiocpp-higgs", ref, "ref text", "en", "", 0); err != nil {
		t.Fatal(err)
	}
	got, err := latents.ReadFile(files[0])
	if err != nil || got.Frames != 4 || got.Meta["codec_model"] != "m2" {
		t.Fatalf("cache after model change = %+v, %v", got, err)
	}
	// A different reference is a different entry.
	if _, err := mgr.GenerateChunkedAudio(t.Context(), "x", "audiocpp-higgs", []byte("other"), "ref text", "en", "", 0); err != nil {
		t.Fatal(err)
	}
	if files, _ = filepath.Glob(filepath.Join(dir, "*"+latents.Ext)); len(files) != 2 {
		t.Errorf("%d cache files after a second reference, want 2", len(files))
	}
}
