package voicerefs

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/ttsworker/ttsworkertest"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// TestEnsureFileConcurrentCallsRenderOnce stresses the exact race
// presetLocks fixes: many goroutines calling EnsureFile for the same
// not-yet-rendered presetID at once should still pay for exactly one
// Design call between them, not one per racing goroutine.
func TestEnsureFileConcurrentCallsRenderOnce(t *testing.T) {
	fake := ttsworkertest.New(t)
	tts := fake.Manager()
	dataDir := t.TempDir()

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = EnsureFile(t.Context(), tts, dataDir, "shared-preset", "speak warmly", 1, "hello world", 1.0, "")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureFile (goroutine %d): %v", i, err)
		}
	}
	want := paths[0]
	for i, p := range paths {
		if p != want {
			t.Fatalf("expected every call to return the same path, goroutine %d got %q vs %q", i, p, want)
		}
	}
	if got := len(fake.DesignCalls()); got != 1 {
		t.Fatalf("expected exactly 1 Design call across %d concurrent EnsureFile calls, got %d", n, got)
	}
}

func TestEnsureFileRendersAndCaches(t *testing.T) {
	fake := ttsworkertest.New(t)
	tts := fake.Manager()
	dataDir := t.TempDir()

	path, err := EnsureFile(t.Context(), tts, dataDir, "preset-1", "speak warmly", 1, "hello world", 1.0, "")
	if err != nil {
		t.Fatalf("EnsureFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected reference clip to exist at %s: %v", path, err)
	}
	if path != audiopath.VoicePresetRefFile(dataDir, "preset-1") {
		t.Fatalf("unexpected reference clip path: %s", path)
	}
	if len(fake.DesignCalls()) != 1 {
		t.Fatalf("expected exactly 1 Design call, got %d", len(fake.DesignCalls()))
	}

	// A second EnsureFile call should be a pure cache hit - no new render.
	if _, err := EnsureFile(t.Context(), tts, dataDir, "preset-1", "speak warmly", 1, "hello world", 1.0, ""); err != nil {
		t.Fatalf("EnsureFile (cached): %v", err)
	}
	if len(fake.DesignCalls()) != 1 {
		t.Fatalf("expected EnsureFile to reuse the cached file, got %d Design calls", len(fake.DesignCalls()))
	}
}

func TestRegenerateAlwaysReRenders(t *testing.T) {
	fake := ttsworkertest.New(t)
	tts := fake.Manager()
	dataDir := t.TempDir()

	if _, err := EnsureFile(t.Context(), tts, dataDir, "preset-1", "speak warmly", 1, "hello world", 1.0, ""); err != nil {
		t.Fatalf("EnsureFile: %v", err)
	}
	if _, err := Regenerate(t.Context(), tts, dataDir, "preset-1", "speak differently now", 1, "hello world", 1.0, ""); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if len(fake.DesignCalls()) != 2 {
		t.Fatalf("expected Regenerate to always re-render, got %d Design calls", len(fake.DesignCalls()))
	}
	last := fake.DesignCalls()[1]
	if last.Instruct != "speak differently now" {
		t.Fatalf("expected the new instruct to be sent to Design, got %q", last.Instruct)
	}
}

// TestRegenerateTruncatesAnOverlongClip is the regression test for a real
// production incident: a character's LLM-generated RefLine, combined with
// a slow instructed delivery pace, rendered a 55s reference clip that
// reproducibly OOM'd the ttsworker process on every one of her paragraphs
// (a normal preset's own clip runs ~20-30s) - see maxRefClipSeconds' own
// doc comment. Regenerate must trim any render past that cap down to it,
// rather than saving whatever Design happens to produce verbatim.
func TestRegenerateTruncatesAnOverlongClip(t *testing.T) {
	fake := ttsworkertest.New(t)
	fake.OnDesign = func(ttsproto.DesignRequest) ([]byte, error) {
		return syntheticWav(t, 55*time.Second), nil
	}
	tts := fake.Manager()
	dataDir := t.TempDir()

	path, err := Regenerate(t.Context(), tts, dataDir, "preset-1", "speak slowly", 1, "a very long generated reference line", 1.0, "")
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rendered clip: %v", err)
	}
	dur, err := wav.Duration(data)
	if err != nil {
		t.Fatalf("wav.Duration: %v", err)
	}
	if dur > time.Duration(maxRefClipSeconds*float64(time.Second)) {
		t.Fatalf("expected the clip to be truncated to at most %vs, got %v", maxRefClipSeconds, dur)
	}
}

// TestRegenerateLeavesAShortClipUntouched confirms the common case (a
// normal-length render, well under the cap) is saved verbatim - no
// truncation, no re-encoding artifact from a no-op round trip changing its
// duration.
func TestRegenerateLeavesAShortClipUntouched(t *testing.T) {
	fake := ttsworkertest.New(t)
	tts := fake.Manager()
	dataDir := t.TempDir()

	path, err := Regenerate(t.Context(), tts, dataDir, "preset-1", "speak warmly", 1, "hello world", 1.0, "")
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rendered clip: %v", err)
	}
	dur, err := wav.Duration(data)
	if err != nil {
		t.Fatalf("wav.Duration: %v", err)
	}
	if dur <= 0 || dur > time.Duration(maxRefClipSeconds*float64(time.Second)) {
		t.Fatalf("expected a short, untouched clip, got duration %v", dur)
	}
}

// syntheticWav builds a minimal silent WAV of exactly dur - this
// package's own tests otherwise only ever get a clip's duration from
// ttsworkertest's own synthesize (character-count-derived, not directly
// controllable), which TestRegenerateTruncatesAnOverlongClip needs to be
// able to set precisely via OnDesign.
func syntheticWav(t *testing.T, dur time.Duration) []byte {
	t.Helper()
	const sampleRate = 16000
	n := int(dur.Seconds() * sampleRate)
	samples := make([]float32, n)
	data, err := wav.Encode(samples, sampleRate, 1)
	if err != nil {
		t.Fatalf("wav.Encode: %v", err)
	}
	return data
}

func TestApplySpeedNoOpAtUnity(t *testing.T) {
	fake := ttsworkertest.New(t)
	tts := fake.Manager()

	data, err := tts.Design(t.Context(), "some ref text of a certain length", "speak warmly", "Auto", "", nil)
	if err != nil {
		t.Fatalf("Design: %v", err)
	}
	unchanged, err := applySpeed(data, 1.0)
	if err != nil {
		t.Fatalf("applySpeed(1.0): %v", err)
	}
	origDur, err := wav.Duration(data)
	if err != nil {
		t.Fatalf("wav.Duration(orig): %v", err)
	}
	newDur, err := wav.Duration(unchanged)
	if err != nil {
		t.Fatalf("wav.Duration(unchanged): %v", err)
	}
	if origDur != newDur {
		t.Fatalf("expected applySpeed(1.0) to be a no-op, durations differ: %v vs %v", origDur, newDur)
	}

	sped, err := applySpeed(data, 2.0)
	if err != nil {
		t.Fatalf("applySpeed(2.0): %v", err)
	}
	spedDur, err := wav.Duration(sped)
	if err != nil {
		t.Fatalf("wav.Duration(sped): %v", err)
	}
	if spedDur >= origDur {
		t.Fatalf("expected a 2x speedup to shorten the clip: orig=%v sped=%v", origDur, spedDur)
	}
}

func TestDeriveFromExistingAndCached(t *testing.T) {
	fake := ttsworkertest.New(t)
	tts := fake.Manager()
	dataDir := t.TempDir()

	if _, err := EnsureFile(t.Context(), tts, dataDir, "source-preset", "speak warmly", 1, "a somewhat longer reference line", 1.0, ""); err != nil {
		t.Fatalf("EnsureFile (source): %v", err)
	}
	if len(fake.DesignCalls()) != 1 {
		t.Fatalf("expected exactly 1 Design call for the source preset, got %d", len(fake.DesignCalls()))
	}

	derivedPath, err := DeriveFromExisting(t.Context(), dataDir, "derived-preset", "source-preset", 2.0)
	if err != nil {
		t.Fatalf("DeriveFromExisting: %v", err)
	}
	if _, err := os.Stat(derivedPath); err != nil {
		t.Fatalf("expected derived clip to exist: %v", err)
	}
	// Deriving is a pure re-speed of the existing bytes - no extra worker
	// call at all, real or otherwise.
	if len(fake.DesignCalls()) != 1 {
		t.Fatalf("DeriveFromExisting should not call Design, got %d calls", len(fake.DesignCalls()))
	}

	// DesignConfigHash + CacheDesignRender/LookupCachedDesign round trip.
	hash := DesignConfigHash("speak warmly", 1, "some text", "Auto", "")
	if _, ok := LookupCachedDesign(dataDir, hash); ok {
		t.Fatalf("expected no cached design render before any CacheDesignRender call")
	}
	rendered, err := tts.Design(t.Context(), "some text", "speak warmly", "Auto", "", nil)
	if err != nil {
		t.Fatalf("Design: %v", err)
	}
	CacheDesignRender(dataDir, hash, rendered)
	cached, ok := LookupCachedDesign(dataDir, hash)
	if !ok {
		t.Fatalf("expected a cached design render after CacheDesignRender")
	}
	if len(cached) != len(rendered) {
		t.Fatalf("cached render doesn't match what was cached")
	}

	derivedFromCache, err := DeriveFromCached(dataDir, "cached-derived-preset", cached, 1.5)
	if err != nil {
		t.Fatalf("DeriveFromCached: %v", err)
	}
	if _, err := os.Stat(derivedFromCache); err != nil {
		t.Fatalf("expected DeriveFromCached's file to exist: %v", err)
	}
}

// constantAmplitudeClip returns WAV bytes for a clip alternating +amp/-amp
// samples for n samples, at sampleRate - a convenient known-loudness
// fixture for the loudness tests: any two clips built from this same
// waveform shape differ only by a linear scale factor, so BS.1770's
// K-weighting filter (itself linear) scales their measured loudness by
// exactly that same factor regardless of its own exact frequency-response
// details, which is what makes peakAmplitude below a reliable way to
// check an applied gain without depending on
// internal/loudness's internals from this test.
func constantAmplitudeClip(t *testing.T, amp float32, n int, sampleRate int) []byte {
	t.Helper()
	samples := make([]float32, n)
	for i := range samples {
		if i%2 == 0 {
			samples[i] = amp
		} else {
			samples[i] = -amp
		}
	}
	data, err := wav.Encode(samples, sampleRate, 1)
	if err != nil {
		t.Fatalf("wav.Encode: %v", err)
	}
	return data
}

func writePresetClip(t *testing.T, dataDir, presetID string, data []byte) {
	t.Helper()
	if err := audiopath.EnsureVoiceRefDir(dataDir); err != nil {
		t.Fatalf("EnsureVoiceRefDir: %v", err)
	}
	if err := os.WriteFile(audiopath.VoicePresetRefFile(dataDir, presetID), data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func peakAmplitude(t *testing.T, path string) float64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	clip, err := wav.Decode(data)
	if err != nil {
		t.Fatalf("wav.Decode: %v", err)
	}
	var peak float64
	for _, s := range clip.Samples {
		if v := math.Abs(float64(s)); v > peak {
			peak = v
		}
	}
	return peak
}

func TestDeleteIsSafeOnMissingFile(t *testing.T) {
	dataDir := t.TempDir()
	if err := Delete(dataDir, "never-created"); err != nil {
		t.Fatalf("Delete on a never-created preset should be a no-op, got: %v", err)
	}
}

func TestDesignFailurePropagates(t *testing.T) {
	fake := ttsworkertest.New(t)
	fake.OnDesign = func(req ttsproto.DesignRequest) ([]byte, error) {
		return nil, errors.New("simulated worker failure")
	}
	tts := fake.Manager()
	dataDir := t.TempDir()

	if _, err := EnsureFile(t.Context(), tts, dataDir, "preset-1", "speak warmly", 1, "hello world", 1.0, ""); err == nil {
		t.Fatalf("expected EnsureFile to propagate the worker's Design failure")
	}
	if _, err := os.Stat(audiopath.VoicePresetRefFile(dataDir, "preset-1")); err == nil {
		t.Fatalf("expected no reference clip to have been written after a failed render")
	}
}

func TestBoundLoudness(t *testing.T) {
	base := constantAmplitudeClip(t, 0.2, 1000, 16000)
	peak := func(data []byte) float64 {
		t.Helper()
		clip, err := wav.Decode(data)
		if err != nil {
			t.Fatalf("wav.Decode: %v", err)
		}
		var p float64
		for _, s := range clip.Samples {
			p = max(p, math.Abs(float64(s)))
		}
		return p
	}
	for _, tc := range []struct {
		name      string
		emotion   string
		amplitude float32
		want      float64
	}{
		// Default band is -3.2 LU..+5.85 LU around 0.2: amplitudes ~0.138..~0.3927.
		{"within band louder", "sad", 0.35, 0.35},
		{"within band quieter", "sad", 0.15, 0.15},
		{"too loud", "sad", 0.8, 0.2 * math.Pow(10, maxVariantLU/20)},
		{"too quiet", "sad", 0.03, 0.2 * math.Pow(10, minVariantLU/20)},
		// Shout's floor is above the base: a shout as loud as the base is raised.
		{"shout raised", "shout", 0.2, 0.2 * math.Pow(10, 10*math.Log2(1.3)/20)},
		{"shout too loud", "shout", 0.8, 0.2 * math.Pow(10, 10*math.Log2(1.75)/20)},
		// Whisper's floor is just under the base: a -10 LU whisper is raised.
		{"whisper raised", "whisper", 0.0632, 0.2 * math.Pow(10, 10*math.Log2(0.8)/20)},
		// A raise may exceed clampGain's +12 dB (here ~+23 dB).
		{"whisper raised past 12 dB", "whisper", 0.01, 0.2 * math.Pow(10, 10*math.Log2(0.8)/20)},
		{"whisper too loud", "whisper", 0.3, 0.2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			variant := constantAmplitudeClip(t, tc.amplitude, 1000, 16000)
			out, err := boundLoudness(variant, base, tc.emotion)
			if err != nil {
				t.Fatalf("boundLoudness: %v", err)
			}
			if got := peak(out); math.Abs(got-tc.want) > 0.005 {
				t.Fatalf("peak = %.4f, want %.4f", got, tc.want)
			}
		})
	}
}

func TestBoundLoudnessPeakCeiling(t *testing.T) {
	// A shout matching a loud base would need ~0.95 peak to reach its
	// floor; the raise stops at peakCeiling instead of clipping.
	base := constantAmplitudeClip(t, 0.7, 1000, 16000)
	variant := constantAmplitudeClip(t, 0.7, 1000, 16000)
	out, err := boundLoudness(variant, base, "shout")
	if err != nil {
		t.Fatalf("boundLoudness: %v", err)
	}
	clip, err := wav.Decode(out)
	if err != nil {
		t.Fatalf("wav.Decode: %v", err)
	}
	var p float64
	for _, s := range clip.Samples {
		p = max(p, math.Abs(float64(s)))
	}
	if math.Abs(p-peakCeiling) > 0.005 {
		t.Fatalf("peak = %.4f, want %.4f", p, peakCeiling)
	}
}

// TestDeleteTakesDerivedVoices checks that removing a voice's clip removes
// its numbered siblings' and age stages' clips and variants with it, and
// nobody else's.
func TestDeleteTakesDerivedVoices(t *testing.T) {
	dataDir := t.TempDir()
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gone := []string{
		audiopath.VoicePresetRefFile(dataDir, "abc"),
		audiopath.VoicePresetVariantFile(dataDir, "abc", "angry"),
		audiopath.VoicePresetRefFile(dataDir, "abc~2-1234"),
		audiopath.VoicePresetRefVersionFile(dataDir, "abc~2-1234"),
		audiopath.VoicePresetVariantFile(dataDir, "abc~a13-5678", "sad"),
	}
	kept := []string{
		audiopath.VoicePresetRefFile(dataDir, "abcd"),
		audiopath.VoicePresetRefFile(dataDir, "abcd~2-1234"),
		audiopath.VoicePresetVariantFile(dataDir, "abcd~2-1234", "sad"),
	}
	for _, p := range append(gone, kept...) {
		write(p)
	}
	if err := Delete(dataDir, "abc"); err != nil {
		t.Fatal(err)
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there", p)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}
