// Package musicgen chains Stable Audio Medium generation calls into one
// continuous clip for one chapter tone region - see internal/httpapi's
// music.go (which decides each region's target duration/prompt/seed) and
// internal/speakerattr's ScoreMusic pass (which decides where tone regions
// start/end and whether each one continues the previous region's mood or
// cuts to a new one). Stable Audio's own per-call duration defaults to
// 120s (docs/models/stable_audio.md in the audio.cpp checkout) and this
// package deliberately requests less than that per call (maxChunkSeconds)
// to keep individual GPU calls short on hardware with no spare VRAM
// headroom (see backend/CLAUDE.md) - a region longer than one chunk is
// filled by several chained calls instead, each one continuation-seeded
// from the immediately preceding chunk's own tail via Stable Audio's
// inpaint_audio path (see ttsproto.StableAudioRequest's own doc comment)
// so a long region's music actually evolves across the chain rather than
// looping one short clip.
//
// Seeding specifically uses inpainting, not the more obvious-looking
// init_audio path: Stable Audio 3's own prompting guide gives inpainting
// - masking [seed's own duration, seed's own duration + new duration) of
// the requested output timeline - as its documented recipe for extending
// a clip past its current end. init_audio was tried first and confirmed
// (empirically, against this box's real GPU) not to do that at all: at a
// low init_noise_level it reproduces the seed almost verbatim for the
// seed's own duration, then trails into near-silence for the rest of a
// longer requested duration, since there's nothing left in the seed to
// condition on past its own end; a higher noise level avoids the silence
// but stops resembling the seed closely enough to call it a real
// continuation - inpainting doesn't have that tradeoff at all, since the
// model is explicitly asked to generate new masked content conditioned on
// the given unmasked audio, not to transform the whole clip. Every
// within-region chunk join is a plain concatenation (wav.Concat), never a
// crossfade: a seeded chunk is generated to flow out of its own seed's
// tail, so there's nothing to paper over at that seam the way there would
// be between two independently generated clips.
//
// Region-to-region transitions are deliberately NOT this package's
// concern: this app's chosen delivery is per-region clips, mixed live
// client-side (Web Audio - see frontend's background-music playback
// code), not one chapter-length file stitched here. A "continuation"
// region only needs its own *first* chunk seeded from the *previous
// region's* own already-generated clip (see Region.Seed) - the frontend
// then plays that first chunk immediately after the previous region's
// clip with no fade, mirroring the same hard-cut treatment this package
// already gives every within-region seeded join. A "cut" region (or a
// chapter's first region, with nothing to continue from) generates its
// first chunk unseeded (Region.Seed nil), and the frontend applies a
// short crossfade there instead.
//
// A region's own served clip also loops in the frontend (source.loop=true)
// whenever its paragraphs outlast its own generated duration - a *third*
// kind of seam, entirely internal to one region's own clip, distinct from
// both of the above. GenerateRegion renders extra margin at both ends for
// exactly this (see loopCrossfadeFraction/trimToLoopableClip) and trims it
// back off before returning, so every clip this package hands back is
// already loop-ready - no duplication or further audio processing needed
// on the frontend to make that wrap sound smooth.
package musicgen

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// Backend is the one call this package needs against the ttsworker
// process - satisfied by *internal/ttsworker.Manager. A small local
// interface rather than a concrete dependency, the same reasoning
// internal/speakerattr's own llmBackend gives: keeps this package free of
// any transitive audiocpp-go/cgo dependency of its own.
type Backend interface {
	StableAudioMedium(ctx context.Context, req ttsproto.StableAudioRequest) ([]byte, error)
}

// maxChunkSeconds bounds one Stable Audio call's own requested *new*
// duration - see the package doc comment for why this app deliberately
// requests less than audio.cpp's own 120s default per call. A seeded
// call's own total requested DurationSeconds is this plus however much
// seed context it was given (bounded by continuationSeedSeconds), so the
// real per-call ceiling is a bit higher than this alone - still well
// under audio.cpp's own default.
const maxChunkSeconds = 60.0

// continuationSeedSeconds bounds how much of the immediately preceding
// chunk's own tail is fed back in as the next chunk's own inpaint context
// - always the last continuationSeedSeconds of whatever was generated most
// recently, never a region's full accumulated audio so far, so the input
// audio length on any one Stable Audio call stays bounded regardless of
// how many chunks a long region ends up chaining through.
const continuationSeedSeconds = 30.0

// Region is one call's worth of input to GenerateRegion.
type Region struct {
	Prompt                string
	NegativePrompt        string
	TargetDurationSeconds float64
	// Seed, when non-nil, is the previous region's own finished clip (raw
	// WAV bytes) - this region's first chunk is generated continuation-
	// seeded from its tail instead of unseeded. nil means generate this
	// region's first chunk unseeded (a "cut" transition, or the chapter's
	// first region) - see the package doc comment.
	Seed []byte
}

// loopCrossfadeFraction is how much extra margin GenerateRegion renders
// at each end of a region's own clip - purely internal overhead, trimmed
// back off before the clip is ever returned (see trimToLoopableClip) - so
// the served clip is 2*loopCrossfadeFraction (here, 20%) longer to render
// than TargetDurationSeconds actually asks for. Exists because Stable
// Audio tapers its own output toward silence at the true start/end of
// anything it renders, and a region's own clip loops indefinitely
// whenever its paragraphs outlast it (frontend useBackgroundMusic's own
// source.loop=true) - so that taper used to land squarely on the loop
// seam every single wrap, a real, confirmed listening complaint
// ("substantial start/end ... periods of low music audio"). 0.1 (10%
// margin on each end) is comfortably wider than that taper.
const loopCrossfadeFraction = 0.1

// GenerateRegion renders one chapter tone region's full-duration
// background-music clip, chaining as many Stable Audio Medium calls as
// the (margin-padded - see loopCrossfadeFraction) render target needs
// (see maxChunkSeconds) and stitching them together with plain
// concatenation - see the package doc comment for why no crossfade is
// needed at any of these internal chunk-to-chunk seams (that's a
// different seam from the one loopCrossfadeFraction/
// trimToLoopableClip exists for - this one is entirely within one
// continuous render, always flowing forward). Returns raw WAV bytes,
// already trimmed to exactly TargetDurationSeconds and loop-crossfaded,
// ready to persist and play as-is.
//
// Deliberately no minimum on TargetDurationSeconds - a region covering
// only a couple of short paragraphs generates a correspondingly short
// clip rather than being padded out to some "musically coherent" floor. A
// generated clip's own length is meant to track the narration it plays
// under, not a fixed musical minimum - a short region is fine to leave
// short.
func GenerateRegion(ctx context.Context, backend Backend, in Region) ([]byte, error) {
	served := in.TargetDurationSeconds
	margin := served * loopCrossfadeFraction
	renderTarget := served + 2*margin

	numChunks := int(renderTarget / maxChunkSeconds)
	if renderTarget-float64(numChunks)*maxChunkSeconds > 0 {
		numChunks++
	}
	if numChunks < 1 {
		numChunks = 1
	}
	chunkSeconds := renderTarget / float64(numChunks)

	var out *wav.Clip
	seedAudio := in.Seed
	for i := 0; i < numChunks; i++ {
		var chunk *wav.Clip
		var err error
		if seedAudio == nil {
			chunk, err = generateChunk(ctx, backend, in.Prompt, in.NegativePrompt, chunkSeconds)
		} else {
			chunk, err = generateContinuationChunk(ctx, backend, in.Prompt, in.NegativePrompt, seedAudio, chunkSeconds)
		}
		if err != nil {
			return nil, fmt.Errorf("generate chunk %d/%d: %w", i+1, numChunks, err)
		}
		if out == nil {
			out = chunk
		} else {
			out, err = wav.Concat(out, chunk)
			if err != nil {
				return nil, fmt.Errorf("stitch chunk %d/%d: %w", i+1, numChunks, err)
			}
		}
		if i < numChunks-1 {
			// Only needed to seed the *next* chunk - skipped on the last
			// one, which has no successor to feed.
			seedAudio = tailWavBytes(chunk)
		}
	}

	out = trimToLoopableClip(out, margin, served)

	return wav.Encode(out.Samples, out.SampleRate, out.Channels)
}

// trimToLoopableClip is GenerateRegion's own last step: given clip
// (rendered 2*marginSeconds longer than servedSeconds - see
// loopCrossfadeFraction), trims it down to exactly servedSeconds while
// crossfading the loop seam with real, already-rendered margin audio
// rather than leaving the clip's own literal true edges - the ones
// Stable Audio actually tapers - as what plays right at the wrap.
//
// The "core" servedSeconds in the middle of clip, offset marginSeconds in
// from both true edges (so neither of its own two ends sits inside
// Stable Audio's own start/end taper any more), becomes the served
// clip's own body, unmodified except at its own tail: that tail is
// crossfaded into the head margin - the audio that, in the original
// continuous render, played immediately *before* the core began - rather
// than left as the core's own unmodified tail. When frontend playback
// loops the served clip (source.loop), what plays right before wrapping
// back to the core's own start is genuinely the *same* audio that led
// into that exact content the first time through, in one continuous
// render - not a synthetic blend of unrelated material, just replayed at
// the loop point instead of only once. The tail margin (audio that
// continued past the core, on the far side) is rendered purely so the
// core's own tail also sits safely clear of the true end's own taper -
// its actual samples are never used; there's only one crossfade point
// (the loop seam) per clip, and the head margin is what naturally leads
// into the core's own start.
//
// Falls back to clip unmodified if it came back shorter than
// 2*marginSeconds+servedSeconds (a short/degenerate render) - better a
// clip a bit longer or shorter than requested than an out-of-bounds slice.
func trimToLoopableClip(clip *wav.Clip, marginSeconds, servedSeconds float64) *wav.Clip {
	if clip.Channels <= 0 || clip.SampleRate <= 0 {
		return clip
	}
	marginSamples := int(marginSeconds*float64(clip.SampleRate)) * clip.Channels
	servedSamples := int(servedSeconds*float64(clip.SampleRate)) * clip.Channels
	if marginSamples < 0 || servedSamples <= 0 || 2*marginSamples+servedSamples > len(clip.Samples) {
		return clip
	}

	headMargin := clip.Samples[0:marginSamples]
	core := clip.Samples[marginSamples : marginSamples+servedSamples]

	out := make([]float32, servedSamples)
	copy(out, core)

	if marginSamples > 0 {
		tailStart := servedSamples - marginSamples
		if tailStart < 0 {
			tailStart = 0
		}
		n := servedSamples - tailStart
		for i := 0; i < n; i++ {
			// Equal-power crossfade so the blended tail's own loudness
			// stays roughly constant across it rather than dipping toward
			// silence partway through, the way a plain linear fade would.
			t := float64(i) / float64(n)
			fadeOut := float32(math.Cos(t * math.Pi / 2))
			fadeIn := float32(math.Sin(t * math.Pi / 2))
			out[tailStart+i] = core[tailStart+i]*fadeOut + headMargin[i]*fadeIn
		}
	}

	return &wav.Clip{Samples: out, SampleRate: clip.SampleRate, Channels: clip.Channels}
}

// generateChunk renders seconds of plain, unseeded audio - GenerateRegion's
// own path for a chunk with no seed to continue from (the very first chunk
// of an unseeded region).
func generateChunk(ctx context.Context, backend Backend, prompt, negativePrompt string, seconds float64) (*wav.Clip, error) {
	wavBytes, err := backend.StableAudioMedium(ctx, ttsproto.StableAudioRequest{
		Prompt:          prompt,
		NegativePrompt:  negativePrompt,
		DurationSeconds: seconds,
	})
	if err != nil {
		return nil, err
	}
	return wav.Decode(wavBytes)
}

// generateContinuationChunk extends seedAudio by newSeconds of genuinely
// new content, via Stable Audio's inpaint_audio path - see the package
// doc comment for why inpainting, not init_audio, is what actually
// achieves this. Requests DurationSeconds equal to the seed's own
// duration plus newSeconds, with the mask covering exactly
// [seed's own duration, that total) - Stable Audio 3's own documented
// continuation recipe - then returns only the newly-inpainted tail (the
// combined output, sliced at the seed's own duration), discarding the
// leading portion that's just a rendition of audio the caller already has
// accumulated elsewhere (GenerateRegion's own out).
func generateContinuationChunk(ctx context.Context, backend Backend, prompt, negativePrompt string, seedAudio []byte, newSeconds float64) (*wav.Clip, error) {
	seed, err := wav.Decode(seedAudio)
	if err != nil {
		return nil, fmt.Errorf("decode seed: %w", err)
	}
	seedSeconds := clipSeconds(seed)

	wavBytes, err := backend.StableAudioMedium(ctx, ttsproto.StableAudioRequest{
		Prompt:                  prompt,
		NegativePrompt:          negativePrompt,
		DurationSeconds:         seedSeconds + newSeconds,
		InitAudioBase64:         base64.StdEncoding.EncodeToString(seedAudio),
		InpaintMaskStartSeconds: seedSeconds,
		InpaintMaskEndSeconds:   seedSeconds + newSeconds,
	})
	if err != nil {
		return nil, err
	}
	combined, err := wav.Decode(wavBytes)
	if err != nil {
		return nil, err
	}
	return sliceFrom(combined, seedSeconds)
}

// clipSeconds is clip's own real duration - the per-channel frame count
// (len(Samples)/Channels) over SampleRate, the same math wav.Duration
// does from raw WAV bytes, just against an already-decoded Clip.
func clipSeconds(clip *wav.Clip) float64 {
	if clip.Channels <= 0 || clip.SampleRate <= 0 {
		return 0
	}
	frames := len(clip.Samples) / clip.Channels
	return float64(frames) / float64(clip.SampleRate)
}

// sliceFrom returns clip's own samples starting at startSeconds onward -
// generateContinuationChunk's own way of keeping only the newly-inpainted
// tail of a combined (seed+new) output.
func sliceFrom(clip *wav.Clip, startSeconds float64) (*wav.Clip, error) {
	if clip.Channels <= 0 {
		return nil, fmt.Errorf("clip has no channels")
	}
	startSample := int(startSeconds*float64(clip.SampleRate)) * clip.Channels
	if startSample > len(clip.Samples) {
		startSample = len(clip.Samples)
	}
	return &wav.Clip{Samples: clip.Samples[startSample:], SampleRate: clip.SampleRate, Channels: clip.Channels}, nil
}

// tailWavBytes returns clip's own last continuationSeedSeconds, re-encoded
// as a standalone WAV - the seed handed to the next chunk's own inpaint
// call. Always a bounded tail, never the whole clip generated so far - see
// continuationSeedSeconds' own doc comment for why. Returns nil (silently
// skipping seeding for whatever comes next) on a re-encode failure or a
// degenerate zero-channel clip - a seed is a quality nicety, not something
// worth failing an otherwise-successful chunk over.
func tailWavBytes(clip *wav.Clip) []byte {
	if clip.Channels <= 0 {
		return nil
	}
	maxSamples := int(continuationSeedSeconds*float64(clip.SampleRate)) * clip.Channels
	samples := clip.Samples
	if len(samples) > maxSamples {
		samples = samples[len(samples)-maxSamples:]
	}
	data, err := wav.Encode(samples, clip.SampleRate, clip.Channels)
	if err != nil {
		return nil
	}
	return data
}
