package voicerefs

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/loudness"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// TargetLUFS is the integrated loudness (internal/loudness) every base
// reference clip is normalized to when it's saved, and (via NormalizeWAV,
// internal/jobs' own call site) every freshly-cloned paragraph clip too -
// voice cloning only roughly carries a reference clip's level into what it
// generates, not exactly, so a fixed target on both ends is what actually
// keeps every voice - narrator, custom preset or auto-provisioned character
// - at one level. -20 LUFS sat just above the median of the library's clips
// when this was introduced (-21.8), in the range audiobook specs (ACX) ask
// for, and leaves room under peakCeiling for most clips without limiting.
const TargetLUFS = -20.0

// peakCeiling is the highest sample magnitude a normalized clip (or a
// raised emotion variant) may reach, about -1 dBFS - below full scale so
// wav.Encode never hard-clips it.
const peakCeiling = 0.89

// normalizeToleranceLU is how far from TargetLUFS an existing clip may sit
// before NormalizeAll rewrites it. Wider than normalizeLoudness's own
// error (a limited clip can land a few tenths short), so an
// already-normalized clip isn't rewritten on every startup.
const normalizeToleranceLU = 1.0

// limiterAttackSeconds/limiterReleaseSeconds shape limitPeaks' gain
// envelope: it starts ducking this long before a peak, and recovers over
// this long after one.
const (
	limiterAttackSeconds  = 0.005
	limiterReleaseSeconds = 0.1
)

// normalizeLoudness rescales samples in place toward TargetLUFS (the gain
// clamped by clampGain), limiting peaks to peakCeiling, and returns the
// clip's integrated loudness before and after. A clip with nothing that
// survives loudness gating (effectively silent) is left untouched and
// reported !ok. Limiting lowers loudness a little, so one correction pass
// follows the first.
func normalizeLoudness(samples []float32, sampleRate int) (before, after float64, ok bool) {
	before = loudness.Integrated(samples, sampleRate)
	if math.IsInf(before, -1) {
		return before, before, false
	}
	gain := clampGain(loudness.GainFor(before, TargetLUFS))
	target := before + 20*math.Log10(gain)
	applyGain(samples, gain)
	limitPeaks(samples, sampleRate, peakCeiling)
	after = loudness.Integrated(samples, sampleRate)
	if math.Abs(after-target) > 0.1 {
		applyGain(samples, clampGain(loudness.GainFor(after, target)))
		limitPeaks(samples, sampleRate, peakCeiling)
		after = loudness.Integrated(samples, sampleRate)
	}
	return before, after, true
}

// NormalizeWAV is normalizeLoudness over encoded WAV bytes, returning data
// unchanged if the clip is effectively silent. Exported for internal/jobs
// to level freshly-generated (cloned) paragraph clips to the same
// TargetLUFS every reference clip already sits at - see generateClone's own
// call site for why that carried-over level can't be trusted on its own.
func NormalizeWAV(data []byte) ([]byte, error) {
	clip, err := wav.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode wav: %w", err)
	}
	if _, _, ok := normalizeLoudness(clip.Samples, clip.SampleRate); !ok {
		return data, nil
	}
	out, err := wav.Encode(clip.Samples, clip.SampleRate, clip.Channels)
	if err != nil {
		return nil, fmt.Errorf("encode wav: %w", err)
	}
	return out, nil
}

// limitPeaks keeps every sample's magnitude at or below ceiling with a
// lookahead peak limiter: each sample's required gain (ceiling/|x|, at
// most 1) recovers linearly over limiterReleaseSeconds after a peak and
// ramps down linearly over limiterAttackSeconds before one, so a peak is
// ducked smoothly instead of clipped. The gain never exceeds a sample's
// own requirement, so the ceiling always holds.
func limitPeaks(samples []float32, sampleRate int, ceiling float64) {
	g := make([]float64, len(samples))
	limited := false
	for i, s := range samples {
		g[i] = 1
		if a := math.Abs(float64(s)); a > ceiling {
			g[i] = ceiling / a
			limited = true
		}
	}
	if !limited {
		return
	}
	releaseStep := 1 / max(1, limiterReleaseSeconds*float64(sampleRate))
	attackStep := 1 / max(1, limiterAttackSeconds*float64(sampleRate))
	for i := 1; i < len(g); i++ {
		g[i] = min(g[i], g[i-1]+releaseStep)
	}
	for i := len(g) - 2; i >= 0; i-- {
		g[i] = min(g[i], g[i+1]+attackStep)
	}
	for i, s := range samples {
		samples[i] = float32(float64(s) * g[i])
	}
}

// NormalizeStats summarizes a NormalizeAll run.
type NormalizeStats struct {
	Normalized, Variants, Failed int
	// Rebounded lists the "<presetID>/<emotion>" variants pulled back into
	// their loudness band (ReboundVariants).
	Rebounded []string
}

// NormalizeAll brings every existing base reference clip under dataDir to
// TargetLUFS (see normalizeLoudness), skipping clips already within
// normalizeToleranceLU of it and under peakCeiling - so it's cheap to run
// on every startup, and also levels clips written some other way (a
// restored book export, a clip saved before normalization existed). Each
// rewritten clip's emotion variants get the same gain rather than being
// deleted: their loudness is bounded relative to the base clip
// (variantBand), so the same shift keeps them in band without paying for
// a BreezeTTS re-render. Every preset's variants are then re-bounded
// against its base clip (ReboundVariants), which only rewrites ones the
// current band no longer allows. Meant to run in its own goroutine.
func NormalizeAll(ctx context.Context, dataDir string) NormalizeStats {
	var stats NormalizeStats
	paths, _ := filepath.Glob(filepath.Join(dataDir, "voice-refs", "*.wav"))
	for _, path := range paths {
		if ctx.Err() != nil {
			break
		}
		presetID := strings.TrimSuffix(filepath.Base(path), ".wav")
		shift, changed, err := normalizeExisting(dataDir, presetID)
		if err != nil {
			stats.Failed++
			continue
		}
		if changed {
			stats.Normalized++
			n, err := shiftVariants(dataDir, presetID, shift, time.Now())
			stats.Variants += n
			if err != nil {
				stats.Failed++
			}
		}
		emotions, err := ReboundVariants(dataDir, presetID)
		for _, emotion := range emotions {
			stats.Rebounded = append(stats.Rebounded, presetID+"/"+emotion)
		}
		if err != nil {
			stats.Failed++
		}
	}
	return stats
}

// normalizeExisting normalizes presetID's base clip in place if it's out
// of tolerance, returning the linear gain it applied overall (as measured
// loudness, so limiting is included) and whether it rewrote the clip.
func normalizeExisting(dataDir, presetID string) (float64, bool, error) {
	unlock := lockPreset(presetID)
	defer unlock()
	path := audiopath.VoicePresetRefFile(dataDir, presetID)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false, err
	}
	clip, err := wav.Decode(data)
	if err != nil {
		return 0, false, err
	}
	current := loudness.Integrated(clip.Samples, clip.SampleRate)
	if math.IsInf(current, -1) {
		return 0, false, nil
	}
	if math.Abs(current-TargetLUFS) <= normalizeToleranceLU && peakOf(clip.Samples) <= peakCeiling+1e-3 {
		return 0, false, nil
	}
	before, after, _ := normalizeLoudness(clip.Samples, clip.SampleRate)
	out, err := wav.Encode(clip.Samples, clip.SampleRate, clip.Channels)
	if err != nil {
		return 0, false, err
	}
	if err := writeFileAtomic(path, out); err != nil {
		return 0, false, err
	}
	return loudness.GainFor(before, after), true, nil
}

// shiftVariants applies gain (peak-limited) to each of presetID's emotion
// variants last written before cutoff - a variant rendered after the base
// clip was rewritten was already bounded against the new level. Returns
// how many it rewrote.
func shiftVariants(dataDir, presetID string, gain float64, cutoff time.Time) (int, error) {
	entries, err := os.ReadDir(audiopath.VoicePresetVariantDir(dataDir, presetID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var n int
	var firstErr error
	for _, e := range entries {
		emotion, ok := strings.CutSuffix(e.Name(), ".wav")
		if !ok || e.IsDir() {
			continue
		}
		if err := shiftVariant(dataDir, presetID, emotion, gain, cutoff); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	return n, firstErr
}

func shiftVariant(dataDir, presetID, emotion string, gain float64, cutoff time.Time) error {
	unlock := lockVariant(presetID, emotion)
	defer unlock()
	path := audiopath.VoicePresetVariantFile(dataDir, presetID, emotion)
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Before(cutoff) {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	clip, err := wav.Decode(data)
	if err != nil {
		return err
	}
	applyGain(clip.Samples, gain)
	limitPeaks(clip.Samples, clip.SampleRate, peakCeiling)
	out, err := wav.Encode(clip.Samples, clip.SampleRate, clip.Channels)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out)
}

func peakOf(samples []float32) float64 {
	var peak float64
	for _, s := range samples {
		peak = max(peak, math.Abs(float64(s)))
	}
	return peak
}

// writeFileAtomic writes data to a temp file beside path and renames it
// into place, so a concurrent reader (paragraph generation cloning from
// this clip) never sees a half-written file.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
