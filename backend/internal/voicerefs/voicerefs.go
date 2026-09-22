// Package voicerefs makes sure a voice preset's reference clip exists on
// disk, creating it via the ttsworker process on first need. The backend is
// the sole owner of this file (both for built-in and custom presets) - the
// worker holds no per-preset state of its own, only per-clone_model loaded
// model instances (see internal/audioworker).
package voicerefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/loudness"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
	"github.com/rhino1998/lectable/backend/internal/wav"
	"github.com/rhino1998/lectable/backend/internal/wsola"
)

// presetLocks serializes EnsureFile's own check-then-render section per
// presetID, across every caller and goroutine that might race one -
// concurrent paragraph generation is the common case (jobs.Manager.generate
// calls EnsureFile from several of poolGeneration's own concurrent slots at
// once - see jobs.maxInFlight), but any two callers racing to create the
// same not-yet-rendered preset's reference clip hit the same issue.
// Without this, both can find no cached file on disk (the plain os.Stat
// check below has no atomicity of its own), both pay for a real
// VoiceDesign render, and both write to the same path with no ordering
// guarantee between them. Keyed the same LoadOrStore-a-sync.Mutex shape
// httpapi.Server.provisionLocks already uses for the analogous
// character-voice-provisioning race (two callers both deciding "no voice
// yet" and each creating a duplicate preset).
var presetLocks sync.Map

// lockPreset returns presetID's own mutex, locked - call the returned func
// (typically via defer) to unlock once done. Growth is bounded by the
// number of distinct preset ids ever rendered through this process, the
// same negligible-in-practice growth httpapi.Server.provisionLocks accepts
// for characters.
func lockPreset(presetID string) func() {
	muAny, _ := presetLocks.LoadOrStore(presetID, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// EnsureFile returns the local path to presetID's reference clip, creating
// it (via the worker's VoiceDesign task, then re-speeding in-process) and
// caching it to disk first if it doesn't exist yet. Safe to call on every
// request that needs the clip (playback, paragraph generation) - the
// common case is just an os.Stat. The clip is independent of whichever
// clone model it's later cloned through (a book's own choice, not the
// preset's).
//
// The render itself is serialized per presetID (see presetLocks) with a
// second os.Stat once the lock is held, so a caller that waited out
// another one's render picks up the file it just finished instead of
// paying for a second one - Regenerate itself (the explicit, deliberate
// "always re-render" path for a preset edit) stays unlocked, since it's
// never called concurrently with itself for the same presetID the way
// EnsureFile's own check-then-render is.
func EnsureFile(ctx context.Context, tts *ttsworker.Manager, dataDir, presetID, instruct string, seed int, refText string, speedMultiplier float64, designModel string) (string, error) {
	path := audiopath.VoicePresetRefFile(dataDir, presetID)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	unlock := lockPreset(presetID)
	defer unlock()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return Regenerate(ctx, tts, dataDir, presetID, instruct, seed, refText, speedMultiplier, designModel)
}

// Regenerate always (re)creates presetID's reference clip and overwrites
// the cached file, regardless of whether one already exists. Call this
// when instruct/seed/refText/speedMultiplier/designModel change (a preset
// edit) - EnsureFile would otherwise keep serving the stale clip forever.
func Regenerate(ctx context.Context, tts *ttsworker.Manager, dataDir, presetID, instruct string, seed int, refText string, speedMultiplier float64, designModel string) (string, error) {
	seed64 := int64(seed)
	data, err := tts.Design(ctx, refText, instruct, "Auto", designModel, &seed64)
	if err != nil {
		return "", fmt.Errorf("design: %w", err)
	}
	data, err = applySpeed(data, speedMultiplier)
	if err != nil {
		return "", err
	}
	data, err = truncateToMaxDuration(data)
	if err != nil {
		return "", err
	}
	return save(dataDir, presetID, data)
}

// DeriveFromExisting builds presetID's reference clip from sourcePresetID's
// already-rendered one, re-stretched by relativeSpeed, instead of paying
// for a full VoiceDesign re-render. Only call this when the two presets'
// instruct/seed/refText are known to match exactly and only speed differs
// (see httpapi's derivation-source lookup, which decides when that's true)
// - relativeSpeed should be the new preset's speedMultiplier divided by
// sourcePresetID's. Pure re-speed math, no worker call at all: VoiceDesign
// rendering never depends on which clone_model a preset uses, only on
// instruct/seed/refText/language/designModel.
func DeriveFromExisting(ctx context.Context, dataDir, presetID, sourcePresetID string, relativeSpeed float64) (string, error) {
	sourceAudio, err := os.ReadFile(audiopath.VoicePresetRefFile(dataDir, sourcePresetID))
	if err != nil {
		return "", err
	}
	return deriveAndSave(dataDir, presetID, sourceAudio, relativeSpeed)
}

// DeriveFromCached is DeriveFromExisting's counterpart for a cached design
// render (see DesignConfigHash/LookupCachedDesign) rather than another
// preset's own file: sourceAudio is always at natural (1.0) pace, so
// speedMultiplier itself is the full relative-speed factor, not a ratio
// against some other preset's own speed.
func DeriveFromCached(dataDir, presetID string, sourceAudio []byte, speedMultiplier float64) (string, error) {
	return deriveAndSave(dataDir, presetID, sourceAudio, speedMultiplier)
}

func deriveAndSave(dataDir, presetID string, sourceAudio []byte, relativeSpeed float64) (string, error) {
	data, err := applySpeed(sourceAudio, relativeSpeed)
	if err != nil {
		return "", err
	}
	data, err = truncateToMaxDuration(data)
	if err != nil {
		return "", err
	}
	return save(dataDir, presetID, data)
}

// applySpeed re-stretches WAV bytes by multiplier (pitch preserved) via
// internal/wsola - a no-op copy when multiplier is 1.0 (wsola.TimeStretch's
// own fast path).
func applySpeed(data []byte, multiplier float64) ([]byte, error) {
	clip, err := wav.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode wav: %w", err)
	}
	stretched := wsola.TimeStretch(clip.Samples, clip.SampleRate, multiplier)
	out, err := wav.Encode(stretched, clip.SampleRate, clip.Channels)
	if err != nil {
		return nil, fmt.Errorf("encode wav: %w", err)
	}
	return out, nil
}

// maxRefClipSeconds caps a reference clip's own rendered duration.
// audio.cpp's zero-shot voice cloning feeds the whole reference clip in as
// conditioning context for every paragraph cloned from it, so an
// unusually long one inflates the KV-cache/prefill-graph allocation Higgs
// needs well past what a normal preset's ~15-20s clip needs - up to the
// point of failing to allocate at all on hardware that handles every
// other preset fine. Real, observed case: a character whose
// LLM-generated RefLine (speakerattr.Client.CharacterizeVoice) came out
// longer than that package's own word target, spoken at a slow/deliberate
// instructed pace, rendered clips reproducibly OOMing the ttsworker
// process - even freshly restarted, ruling out a leak/stuck-state
// explanation - on every one of that character's paragraphs, while every
// other character's shorter clip generated normally throughout. Checked
// here, against the actual rendered audio, rather than relying solely on
// bounding RefLine's own word count in speakerattr (that package now also
// trims its own output - see its maxRefLineWords - as a first line of
// defense): a slow Instruct alone can inflate duration independent of
// word count, so only a check on the real rendered output catches every
// cause at once. 30s was the original cap here but still wasn't tight
// enough headroom on this box's GPU (a 30s clip reproducibly OOM'd too) -
// lowered to 20s, which sits comfortably above every normal preset's own
// natural length while leaving real margin below where either failure was
// observed.
const maxRefClipSeconds = 20.0

// truncateToMaxDuration trims data to at most maxRefClipSeconds of audio,
// returned unchanged if it's already shorter - applySpeed's own sibling,
// called right after it everywhere a reference clip is produced or
// re-derived, so a speed-down (which lengthens a clip) can't push an
// already-at-the-cap clip back over it either. A hard cut with no
// fade-out: this only ever fires for a pathologically long render, not a
// clip a reader listens to end to end, so a rare abrupt ending here is an
// acceptable trade for the simplicity of not computing a fade window.
func truncateToMaxDuration(data []byte) ([]byte, error) {
	clip, err := wav.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode wav: %w", err)
	}
	maxSamples := int(maxRefClipSeconds*float64(clip.SampleRate)) * clip.Channels
	if maxSamples <= 0 || len(clip.Samples) <= maxSamples {
		return data, nil
	}
	out, err := wav.Encode(clip.Samples[:maxSamples], clip.SampleRate, clip.Channels)
	if err != nil {
		return nil, fmt.Errorf("encode wav: %w", err)
	}
	return out, nil
}

// minNormalizeGain/maxNormalizeGain bound NormalizeVolume's computed gain
// factor to roughly -12dB/+12dB - a defensive clamp against a nearly
// silent (but nonzero) reference clip producing a wildly large factor that
// would mostly amplify quantization noise/background hiss rather than
// produce a usable voice.
const (
	minNormalizeGain = 0.25
	maxNormalizeGain = 4.0
)

// NormalizeVolume rescales presetID's already-rendered reference clip by a
// single uniform gain factor (clamped to [minNormalizeGain,
// maxNormalizeGain]) so its overall loudness matches referencePresetID's
// own already-rendered clip - one multiply applied across the whole clip,
// preserving its internal relative dynamics rather than compressing/
// limiting it. Loudness is measured via internal/loudness.Integrated
// (ITU-R BS.1770/"EBU R128" perceptually-weighted, silence-gated
// loudness) rather than plain whole-clip RMS specifically because RMS is
// diluted by however much silence/pause a clip contains - a character
// preset's reference line and the book's own can differ quite a bit in
// that respect, and matching plain RMS between them would chase that
// difference instead of the two voices' actual speaking volume, inflating
// gain (and whatever background noise floor rides along with the speech)
// well past what the voice itself needs. Voice cloning carries a
// reference clip's own loudness into every paragraph generated from it
// (see internal/audioworker), so an auto-provisioned character voice's
// independent VoiceDesign render can otherwise land at a very different
// overall level than the book's own narrator voice, making that
// character's dialogue jarringly louder or quieter than the narration
// around it.
//
// A no-op (nil error) if referencePresetID has no rendered clip yet (the
// book's own voice hasn't been generated against yet) or either clip has
// no content that survives loudness gating at all (effectively silent,
// nothing sensible to scale toward/from) - a caller normalizing a
// freshly-rendered character voice shouldn't fail the whole provisioning
// attempt over this.
func NormalizeVolume(dataDir, presetID, referencePresetID string) error {
	refData, err := os.ReadFile(audiopath.VoicePresetRefFile(dataDir, referencePresetID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	refClip, err := wav.Decode(refData)
	if err != nil {
		return fmt.Errorf("decode reference voice's clip: %w", err)
	}
	targetLUFS := loudness.Integrated(refClip.Samples, refClip.SampleRate)
	if math.IsInf(targetLUFS, -1) {
		return nil
	}

	path := audiopath.VoicePresetRefFile(dataDir, presetID)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	clip, err := wav.Decode(data)
	if err != nil {
		return fmt.Errorf("decode wav: %w", err)
	}
	currentLUFS := loudness.Integrated(clip.Samples, clip.SampleRate)
	if math.IsInf(currentLUFS, -1) {
		return nil
	}

	applyGain(clip.Samples, clampGain(loudness.GainFor(currentLUFS, targetLUFS)))
	out, err := wav.Encode(clip.Samples, clip.SampleRate, clip.Channels)
	if err != nil {
		return fmt.Errorf("encode wav: %w", err)
	}
	return os.WriteFile(path, out, 0o644)
}

// applyGain scales every sample by factor in place. wav.Encode clips
// anything left outside [-1, 1] afterward, so an aggressive factor
// distorts rather than wrapping around.
func applyGain(samples []float32, factor float64) {
	for i, s := range samples {
		samples[i] = float32(float64(s) * factor)
	}
}

func clampGain(factor float64) float64 {
	if factor < minNormalizeGain {
		return minNormalizeGain
	}
	if factor > maxNormalizeGain {
		return maxNormalizeGain
	}
	return factor
}

func save(dataDir, presetID string, data []byte) (string, error) {
	if err := audiopath.EnsureVoiceRefDir(dataDir); err != nil {
		return "", err
	}
	path := audiopath.VoicePresetRefFile(dataDir, presetID)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// DesignConfigHash derives a stable, config-defined key for a raw,
// natural-pace VoiceDesign render's exact recipe: instruct/seed/text/
// language/designModel. Design (what both a "test this design" preview and
// the very first step of building any preset's reference clip actually
// call) is deterministic on just this tuple - speedMultiplier is a separate
// WSOLA pass applied afterward (applySpeed above), and the clone model a
// book later clones through never affects the design render itself. So a
// cached render at this key is reusable for any preset sharing just
// instruct/seed/refText/language/designModel, whatever its own
// speedMultiplier ends up being. designModel IS part of the key - two presets with identical instruct/seed/refText but
// different design engines render audibly different clips and must not be
// treated as the same cached recipe.
func DesignConfigHash(instruct string, seed int, text, language, designModel string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%s\x00%s\x00%s", instruct, seed, text, language, designModel)
	return hex.EncodeToString(h.Sum(nil))
}

// CacheDesignRender saves data to hash's cache slot for later reuse -
// best-effort: a failure here just means a later save won't find it and
// falls back to a full render, not a hard error worth surfacing.
func CacheDesignRender(dataDir, hash string, data []byte) {
	if err := audiopath.EnsureVoiceDesignCacheDir(dataDir); err != nil {
		return
	}
	_ = os.WriteFile(audiopath.VoiceDesignCacheFile(dataDir, hash), data, 0o644)
}

// LookupCachedDesign returns a previously-cached design render's bytes for
// hash, if any.
func LookupCachedDesign(dataDir, hash string) ([]byte, bool) {
	data, err := os.ReadFile(audiopath.VoiceDesignCacheFile(dataDir, hash))
	if err != nil {
		return nil, false
	}
	return data, true
}

// Delete removes presetID's cached reference clip, if any. Best-effort -
// safe to call on one that was never created.
func Delete(dataDir, presetID string) error {
	err := os.Remove(audiopath.VoicePresetRefFile(dataDir, presetID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
