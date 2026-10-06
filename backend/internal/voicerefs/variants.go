package voicerefs

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/loudness"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
	"github.com/rhino1998/lectable/backend/internal/voices"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// variantLocks serializes EnsureVariantFile per (preset, emotion) - the
// same check-then-render race presetLocks guards for base clips.
var variantLocks sync.Map

func lockVariant(presetID, emotion string) func() {
	muAny, _ := variantLocks.LoadOrStore(presetID+"\x00"+emotion, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// BaseClip is everything needed to (re)render presetID's base reference
// clip if it doesn't exist yet - EnsureFile's own parameters.
type BaseClip struct {
	PresetID        string
	Instruct        string
	Seed            int
	RefText         string
	SpeedMultiplier float64
	DesignModel     string
}

// VariantExists reports whether presetID's emotion variant has been
// rendered.
func VariantExists(dataDir, presetID, emotion string) bool {
	_, err := os.Stat(audiopath.VoicePresetVariantFile(dataDir, presetID, emotion))
	return err == nil
}

// VariantFailed reports whether presetID's emotion variant's render failed
// for good (see MarkVariantFailed) - generation then uses the base clip.
func VariantFailed(dataDir, presetID, emotion string) bool {
	_, err := os.Stat(audiopath.VoicePresetVariantFailedFile(dataDir, presetID, emotion))
	return err == nil
}

// MarkVariantFailed records that presetID's emotion variant can't be
// rendered, so lines in that emotion stop waiting on it and clone from the
// base clip instead. Cleared by DeleteVariant/DeleteVariants (a
// regenerate, or any base-clip change).
func MarkVariantFailed(dataDir, presetID, emotion string) error {
	if err := os.MkdirAll(audiopath.VoicePresetVariantDir(dataDir, presetID), 0o755); err != nil {
		return err
	}
	return os.WriteFile(audiopath.VoicePresetVariantFailedFile(dataDir, presetID, emotion), nil, 0o644)
}

// EnsureVariantFile returns the path to base's emotion variant, rendering
// it first if it doesn't exist yet: BreezeTTS instructed cloning
// (voices.InstructedCloneModel) of base's own reference clip, speaking
// the emotion's RefLine under its Instruction, then capped at
// maxRefClipSeconds and loudness-bounded against the base clip: a variant
// keeps its own level as long as it lands inside its emotion's band
// (variantBand) relative to the base's, and is pulled to the nearest
// bound otherwise. Only base clips are fully normalized (to TargetLUFS,
// see save).
// Renders base itself first if needed (EnsureFile).
func EnsureVariantFile(ctx context.Context, tts *ttsworker.Manager, dataDir string, base BaseClip, emotion string) (string, error) {
	e, ok := emotions.Get(emotion)
	if !ok {
		return "", fmt.Errorf("unknown emotion %q", emotion)
	}
	path := audiopath.VoicePresetVariantFile(dataDir, base.PresetID, emotion)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	unlock := lockVariant(base.PresetID, emotion)
	defer unlock()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	basePath, err := EnsureFile(ctx, tts, dataDir, base.PresetID, base.Instruct, base.Seed, base.RefText, base.SpeedMultiplier, base.DesignModel)
	if err != nil {
		return "", fmt.Errorf("ensure base reference clip: %w", err)
	}
	baseAudio, err := os.ReadFile(basePath)
	if err != nil {
		return "", fmt.Errorf("read base reference clip: %w", err)
	}
	data, err := tts.Generate(ctx, e.RefLine, voices.InstructedCloneModel, baseAudio, base.RefText, "Auto", e.Instruction, "")
	if err != nil {
		return "", fmt.Errorf("render %s variant: %w", emotion, err)
	}
	data, err = truncateToMaxDuration(data)
	if err != nil {
		return "", err
	}
	data, err = boundLoudness(data, baseAudio, emotion)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(audiopath.VoicePresetVariantDir(dataDir, base.PresetID), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	stampVersion(audiopath.VoicePresetVariantVersionFile(dataDir, base.PresetID, emotion))
	return path, nil
}

// minVariantLU/maxVariantLU bound an emotion variant's integrated
// loudness relative to its base clip's, in LU: 0.8x to 1.5x as loud
// *perceptually*, using the usual rule that +10 LU sounds twice as loud
// (so 10*log2(ratio): about -3.2 LU to +5.85 LU). The floor used to be
// half as loud (-10 LU), but Breeze renders warm/sad/weary quietly (a
// median 5-7 LU under the base) and a clone inherits its reference's
// level - paragraph audio isn't normalized - so a character whose lines
// were mostly emotional sat 4-8 LU under the narration. Shout and whisper
// get their own bands (variantBand).
var (
	minVariantLU = 10 * math.Log2(0.8)
	maxVariantLU = 10 * math.Log2(1.5)
)

// variantToleranceLU is how far outside its band a variant may sit before
// it's rescaled, so one already pulled to a bound isn't rewritten again
// over measurement error (see ReboundVariants).
const variantToleranceLU = 0.1

// maxVariantRaise caps the gain a too-quiet variant is raised by, about
// +24 dB. Wider than clampGain's +12 dB: Breeze's whispers render as low
// as -42 LUFS, and +12 dB left those 3-7 LU under their floor.
const maxVariantRaise = 16.0

// variantBand is emotion's [lo, hi] loudness band relative to the base
// clip, in LU. Shout's floor sits above the base (Breeze's shouts often
// came out no louder than the base, so the band alone never lifted
// them): about 1.3x to 1.75x as loud. Whisper shares the default floor
// but may not be louder than the base, the hushed delivery doing the
// rest.
func variantBand(emotion string) (lo, hi float64) {
	switch emotion {
	case "shout":
		return 10 * math.Log2(1.3), 10 * math.Log2(1.75)
	case "whisper":
		return minVariantLU, 0
	}
	return minVariantLU, maxVariantLU
}

// boundLoudness rescales data so its integrated loudness lands within
// emotion's variantBand of reference's, leaving it untouched if it's
// already inside that band. A raise stops short of pushing the peak past
// peakCeiling. Returns data unchanged if either clip has nothing
// that survives loudness gating.
func boundLoudness(data, reference []byte, emotion string) ([]byte, error) {
	refClip, err := wav.Decode(reference)
	if err != nil {
		return nil, fmt.Errorf("decode base clip: %w", err)
	}
	clip, err := wav.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode variant: %w", err)
	}
	if !boundSamples(clip.Samples, clip.SampleRate, loudness.Integrated(refClip.Samples, refClip.SampleRate), emotion) {
		return data, nil
	}
	return wav.Encode(clip.Samples, clip.SampleRate, clip.Channels)
}

// boundSamples is boundLoudness on decoded samples, in place, against a
// base clip's integrated loudness. Reports whether it changed them.
func boundSamples(samples []float32, sampleRate int, baseLUFS float64, emotion string) bool {
	current := loudness.Integrated(samples, sampleRate)
	if math.IsInf(baseLUFS, -1) || math.IsInf(current, -1) {
		return false
	}
	lo, hi := variantBand(emotion)
	diff := current - baseLUFS // variant's loudness relative to the base, in LU
	bounded := min(max(diff, lo), hi)
	if math.Abs(bounded-diff) <= variantToleranceLU {
		return false
	}
	gain := min(max(loudness.GainFor(current, baseLUFS+bounded), minNormalizeGain), maxVariantRaise)
	if gain > 1 {
		if peak := peakOf(samples); peak > 0 {
			gain = max(1, min(gain, peakCeiling/peak))
		}
	}
	if gain == 1 {
		return false
	}
	applyGain(samples, gain)
	return true
}

// ReboundVariants re-applies boundLoudness to presetID's existing emotion
// variants against its current base clip, rewriting those outside their
// band - how variants rendered under an older, wider band are brought to
// the current one without a BreezeTTS re-render. Returns the emotions it
// rewrote; paragraph audio already cloned from them keeps its old level
// until regenerated.
func ReboundVariants(dataDir, presetID string) ([]string, error) {
	entries, err := os.ReadDir(audiopath.VoicePresetVariantDir(dataDir, presetID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	baseData, err := os.ReadFile(audiopath.VoicePresetRefFile(dataDir, presetID))
	if err != nil {
		return nil, err
	}
	base, err := wav.Decode(baseData)
	if err != nil {
		return nil, err
	}
	baseLUFS := loudness.Integrated(base.Samples, base.SampleRate)
	var rewritten []string
	var firstErr error
	for _, e := range entries {
		emotion, ok := strings.CutSuffix(e.Name(), ".wav")
		if !ok || e.IsDir() {
			continue
		}
		changed, err := reboundVariant(dataDir, presetID, emotion, baseLUFS)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if changed {
			rewritten = append(rewritten, emotion)
		}
	}
	return rewritten, firstErr
}

func reboundVariant(dataDir, presetID, emotion string, baseLUFS float64) (bool, error) {
	unlock := lockVariant(presetID, emotion)
	defer unlock()
	path := audiopath.VoicePresetVariantFile(dataDir, presetID, emotion)
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	clip, err := wav.Decode(data)
	if err != nil {
		return false, err
	}
	if !boundSamples(clip.Samples, clip.SampleRate, baseLUFS, emotion) {
		return false, nil
	}
	out, err := wav.Encode(clip.Samples, clip.SampleRate, clip.Channels)
	if err != nil {
		return false, err
	}
	return true, writeFileAtomic(path, out)
}

// DeleteVariant removes presetID's emotion variant and any failure marker,
// so the next line in that emotion re-renders it. Best-effort on a
// missing file.
func DeleteVariant(dataDir, presetID, emotion string) error {
	for _, p := range []string{
		audiopath.VoicePresetVariantFile(dataDir, presetID, emotion),
		audiopath.VoicePresetVariantFailedFile(dataDir, presetID, emotion),
		audiopath.VoicePresetVariantVersionFile(dataDir, presetID, emotion),
	} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// DeleteVariants removes every emotion variant of presetID - called
// whenever its base clip changes, since every variant was cloned from the
// old one.
func DeleteVariants(dataDir, presetID string) error {
	return os.RemoveAll(audiopath.VoicePresetVariantDir(dataDir, presetID))
}
