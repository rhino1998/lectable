package voicerefs

import (
	"math"
	"os"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/loudness"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

func clipLUFS(t *testing.T, path string) float64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	clip, err := wav.Decode(data)
	if err != nil {
		t.Fatalf("wav.Decode: %v", err)
	}
	return loudness.Integrated(clip.Samples, clip.SampleRate)
}

// sineClip returns WAV bytes for one second of a 1kHz sine at amp - near
// K-weighting's flat region, unlike constantAmplitudeClip's Nyquist-rate
// square, which measures far louder than its amplitude suggests.
func sineClip(t *testing.T, amp float64) []byte {
	t.Helper()
	const sampleRate = 16000
	samples := make([]float32, sampleRate)
	for i := range samples {
		samples[i] = float32(amp * math.Sin(2*math.Pi*1000*float64(i)/sampleRate))
	}
	data, err := wav.Encode(samples, sampleRate, 1)
	if err != nil {
		t.Fatalf("wav.Encode: %v", err)
	}
	return data
}

func TestSaveNormalizesToTarget(t *testing.T) {
	for _, amp := range []float64{0.05, 0.1, 0.25} {
		dataDir := t.TempDir()
		path, err := save(dataDir, "p", sineClip(t, amp))
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if got := clipLUFS(t, path); math.Abs(got-TargetLUFS) > 0.2 {
			t.Fatalf("amp %.2f: saved clip at %.2f LUFS, want %.1f", amp, got, TargetLUFS)
		}
		if p := peakAmplitude(t, path); p > peakCeiling+1e-3 {
			t.Fatalf("amp %.2f: peak %.4f over ceiling", amp, p)
		}
	}
}

func TestSaveClampsExtremeGain(t *testing.T) {
	dataDir := t.TempDir()
	path, err := save(dataDir, "p", constantAmplitudeClip(t, 0.001, 16000, 16000))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := peakAmplitude(t, path); math.Abs(got/0.001-maxNormalizeGain) > 0.2 {
		t.Fatalf("peak %.5f, want gain clamped to %.1fx", got, maxNormalizeGain)
	}
}

func TestSaveLeavesSilentClip(t *testing.T) {
	dataDir := t.TempDir()
	silent := constantAmplitudeClip(t, 0, 1000, 16000)
	path, err := save(dataDir, "p", silent)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(silent) {
		t.Fatalf("expected a silent clip to be written unchanged")
	}
}

// A clip whose voice sits at the target but is mostly silence needs no
// gain: gated loudness ignores the padding that plain RMS would count.
func TestNormalizeIgnoresSilencePadding(t *testing.T) {
	const sampleRate = 16000
	toneClip, _ := wav.Decode(constantAmplitudeClip(t, 0.3, sampleRate/2, sampleRate))
	tone := loudness.Integrated(toneClip.Samples, sampleRate)
	padded := append(append([]float32{}, toneClip.Samples...), make([]float32, sampleRate*5)...)
	before, after, ok := normalizeLoudness(padded, sampleRate)
	if !ok {
		t.Fatalf("expected a measurable clip")
	}
	if math.Abs(before-tone) > 2 { // plain RMS would read ~10 LU lower
		t.Fatalf("padding changed measured loudness: %.2f vs tone alone %.2f", before, tone)
	}
	if math.Abs(after-TargetLUFS) > 0.2 {
		t.Fatalf("after = %.2f, want %.1f", after, TargetLUFS)
	}
}

func TestLimitPeaksHoldsCeiling(t *testing.T) {
	const sampleRate = 16000
	samples := make([]float32, sampleRate)
	for i := range samples {
		samples[i] = float32(0.3 * math.Sin(float64(i)*0.1))
	}
	samples[8000] = 1.5 // an isolated spike
	limitPeaks(samples, sampleRate, peakCeiling)
	var peak float64
	for _, s := range samples {
		peak = max(peak, math.Abs(float64(s)))
	}
	if peak > peakCeiling+1e-6 {
		t.Fatalf("peak %.4f over ceiling %.2f", peak, peakCeiling)
	}
	// Far from the spike, the signal is untouched.
	if want := float32(0.3 * math.Sin(100*0.1)); samples[100] != want {
		t.Fatalf("sample far from the spike changed: %v vs %v", samples[100], want)
	}
}

func TestNormalizeAll(t *testing.T) {
	dataDir := t.TempDir()
	writePresetClip(t, dataDir, "quiet", sineClip(t, 0.05))
	variantDir := audiopath.VoicePresetVariantDir(dataDir, "quiet")
	if err := os.MkdirAll(variantDir, 0o755); err != nil {
		t.Fatal(err)
	}
	variantPath := audiopath.VoicePresetVariantFile(dataDir, "quiet", "sad")
	if err := os.WriteFile(variantPath, sineClip(t, 0.04), 0o644); err != nil {
		t.Fatal(err)
	}
	quietBefore := clipLUFS(t, audiopath.VoicePresetRefFile(dataDir, "quiet"))
	variantBefore := clipLUFS(t, variantPath)

	path, err := save(dataDir, "done", sineClip(t, 0.2))
	if err != nil {
		t.Fatal(err)
	}
	doneBytes, _ := os.ReadFile(path)

	s := NormalizeAll(t.Context(), dataDir)
	if s.Normalized != 1 || s.Variants != 1 || s.Failed != 0 {
		t.Fatalf("stats = %+v, want 1 normalized, 1 variant", s)
	}
	quietAfter := clipLUFS(t, audiopath.VoicePresetRefFile(dataDir, "quiet"))
	if math.Abs(quietAfter-TargetLUFS) > 0.2 {
		t.Fatalf("quiet clip at %.2f LUFS, want %.1f", quietAfter, TargetLUFS)
	}
	// The variant keeps its level relative to the base.
	if shift, vshift := quietAfter-quietBefore, clipLUFS(t, variantPath)-variantBefore; math.Abs(shift-vshift) > 0.1 {
		t.Fatalf("variant shifted %.2f LU, base %.2f LU", vshift, shift)
	}
	if data, _ := os.ReadFile(path); string(data) != string(doneBytes) {
		t.Fatalf("an already-normalized clip was rewritten")
	}
	if s := NormalizeAll(t.Context(), dataDir); s.Normalized != 0 {
		t.Fatalf("second run normalized %d clips, want 0", s.Normalized)
	}
}

// TestNormalizeAllReboundsVariants: a variant under its band's floor is
// raised to it once; a second run leaves it alone.
func TestNormalizeAllReboundsVariants(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := save(dataDir, "voice", sineClip(t, 0.2)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(audiopath.VoicePresetVariantDir(dataDir, "voice"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := clipLUFS(t, audiopath.VoicePresetRefFile(dataDir, "voice"))
	quiet := audiopath.VoicePresetVariantFile(dataDir, "voice", "warm")
	inBand := audiopath.VoicePresetVariantFile(dataDir, "voice", "sad")
	for path, amplitude := range map[string]float64{quiet: 0.02, inBand: 0.08} {
		if err := os.WriteFile(path, sineClip(t, amplitude), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Place the in-band one a known 1 LU under the base.
	inBandBytes := gainedClip(t, inBand, base-1)

	s := NormalizeAll(t.Context(), dataDir)
	if len(s.Rebounded) != 1 || s.Rebounded[0] != "voice/warm" || s.Failed != 0 {
		t.Fatalf("stats = %+v, want only voice/warm rebounded", s)
	}
	if got := clipLUFS(t, quiet) - base; math.Abs(got-minVariantLU) > 0.1 {
		t.Fatalf("raised variant sits %.2f LU from the base, want %.2f", got, minVariantLU)
	}
	if data, _ := os.ReadFile(inBand); string(data) != string(inBandBytes) {
		t.Fatalf("an in-band variant was rewritten")
	}
	if s := NormalizeAll(t.Context(), dataDir); len(s.Rebounded) != 0 {
		t.Fatalf("second run rebounded %v, want none", s.Rebounded)
	}
}

// gainedClip rewrites the clip at path to targetLUFS and returns its bytes.
func gainedClip(t *testing.T, path string, targetLUFS float64) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	clip, err := wav.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	applyGain(clip.Samples, loudness.GainFor(loudness.Integrated(clip.Samples, clip.SampleRate), targetLUFS))
	out, err := wav.Encode(clip.Samples, clip.SampleRate, clip.Channels)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}
