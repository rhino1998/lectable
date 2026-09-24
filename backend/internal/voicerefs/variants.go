package voicerefs

import (
	"context"
	"fmt"
	"math"
	"os"
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
// maxRefClipSeconds and loudness-matched to the base clip so an emotional
// line doesn't land louder or quieter than the rest of that voice's lines
// (shout/whisper still differ in delivery, just not in overall level).
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
	data, err = matchLoudness(data, baseAudio)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(audiopath.VoicePresetVariantDir(dataDir, base.PresetID), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// matchLoudness rescales data's overall loudness to reference's - the
// in-memory counterpart of NormalizeVolume. Returns data unchanged if
// either clip has nothing that survives loudness gating.
func matchLoudness(data, reference []byte) ([]byte, error) {
	refClip, err := wav.Decode(reference)
	if err != nil {
		return nil, fmt.Errorf("decode base clip: %w", err)
	}
	target := loudness.Integrated(refClip.Samples, refClip.SampleRate)
	clip, err := wav.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode variant: %w", err)
	}
	current := loudness.Integrated(clip.Samples, clip.SampleRate)
	if math.IsInf(target, -1) || math.IsInf(current, -1) {
		return data, nil
	}
	applyGain(clip.Samples, clampGain(loudness.GainFor(current, target)))
	return wav.Encode(clip.Samples, clip.SampleRate, clip.Channels)
}

// DeleteVariant removes presetID's emotion variant and any failure marker,
// so the next line in that emotion re-renders it. Best-effort on a
// missing file.
func DeleteVariant(dataDir, presetID, emotion string) error {
	for _, p := range []string{
		audiopath.VoicePresetVariantFile(dataDir, presetID, emotion),
		audiopath.VoicePresetVariantFailedFile(dataDir, presetID, emotion),
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
