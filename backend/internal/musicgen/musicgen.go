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
// region's* own already-generated clip (see Region.Seed) so its mood
// evolves out of it; a "cut" region (or a chapter's first region, with
// nothing to continue from) generates its first chunk unseeded (Region.Seed
// nil). Playback treats both the same - every region switch is a short
// equal-power crossfade centred on the paragraph boundary.
//
// A region's own served clip also loops in the clients (source.loop=true)
// whenever its paragraphs outlast its own generated duration - a *third*
// kind of seam, entirely internal to one region's own clip, distinct from
// both of the above. GenerateRegion renders loopCrossfadeSeconds past the
// served length and folds that overrun back over the clip's head (see
// trimToLoopableClip), so every clip this package hands back is already
// loop-ready - no duplication or further audio processing needed on the
// frontend to make that wrap sound smooth.
//
// Ambience is not rendered per region: a setting's soundscape is steady
// texture, so GenerateRegion renders one AmbienceLoopSeconds loop per
// ambience prompt (Region.AmbiencePrompt, left empty once the caller has
// that setting's loop cached) and TileLoop lays it under each region.
// (Batching the loop with a region's first music chunk into one Stable
// Audio call was measured and dropped: the GPU is already saturated at
// batch 1, and a batch renders every item at its longest duration, so it
// was 18-25% slower whenever the two lengths differed.)
package musicgen

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// Backend is the one call this package needs against the ttsworker process -
// satisfied by *internal/ttsworker.Manager. A small local interface rather
// than a concrete dependency, the same reasoning internal/speakerattr's own
// llmBackend gives: keeps this package free of any transitive
// audiocpp-go/cgo dependency of its own.
type Backend interface {
	StableAudioMedium(ctx context.Context, req ttsproto.StableAudioRequest) ([]byte, error)
}

// maxChunkSeconds bounds one Stable Audio call's own requested *new*
// duration; a region longer than this chains several calls. A seeded
// call's total is this plus up to continuationSeedSeconds of seed, well
// under Medium's own ~380s limit. Measured on this box, a seeded call
// costs ~2.4x an unseeded one per new second (the seed is re-rendered), so
// fewer, longer chunks are cheaper.
const maxChunkSeconds = 110.0

// continuationSeedSeconds bounds how much of the preceding audio's tail is
// fed back in as a continuation's inpaint context - always a bounded tail,
// never a region's whole accumulated audio, so the seed's re-render cost
// stays fixed however long the region or its predecessor is.
const continuationSeedSeconds = 10.0

// loopCrossfadeSeconds is how far past its served length every clip is
// rendered: the overrun is crossfaded over the clip's head so its loop
// seam is continuous (see trimToLoopableClip). Stable Audio's own
// duration_padding_seconds (6s, left at its default) already renders past
// the requested end and trims, so the tail this reads sits clear of the
// model's end-of-clip fade.
const loopCrossfadeSeconds = 4.0

// crossfadePaddingSeconds is added on top of every region's own
// TargetDurationSeconds (the summed narration it plays under) - the
// clients crossfade every region switch centred on its paragraph boundary
// (frontend useBackgroundMusic's own CROSSFADE_HALF_SECONDS, 5s), so a
// region's clip actually plays for that long past both of its own ends:
// starting early under the previous region's tail, and running on under
// the next region's head. Padding by both halves means a region no longer
// than its clip spends those overlaps on fresh material instead of
// wrapping onto its own loop seam mid-crossfade.
const crossfadePaddingSeconds = 10.0

// AmbienceLoopSeconds is the length of one setting's ambience loop - long
// enough that the repeat isn't obvious under narration.
const AmbienceLoopSeconds = 60.0

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
	// AmbiencePrompt, when set, also renders that setting's ambience loop
	// (Result.AmbienceLoop). Leave empty when the loop is already cached.
	AmbiencePrompt string
}

// Result is GenerateRegion's output, raw WAV bytes each.
type Result struct {
	// Music is the region's loop-ready music clip, exactly
	// TargetDurationSeconds+crossfadePaddingSeconds long.
	Music []byte
	// AmbienceLoop is the AmbienceLoopSeconds seamless ambience loop, nil
	// unless Region.AmbiencePrompt was set.
	AmbienceLoop []byte
}

// GenerateRegion renders one chapter tone region's full-duration
// background-music clip, chaining as many Stable Audio Medium calls as it
// needs (see maxChunkSeconds) and stitching them together with plain
// concatenation - see the package doc comment for why no crossfade is
// needed at those internal seams. The result is trimmed to exactly
// TargetDurationSeconds+crossfadePaddingSeconds and loop-crossfaded,
// ready to persist and play as-is. With Region.AmbiencePrompt set it also
// renders that setting's ambience loop.
//
// Deliberately no minimum on TargetDurationSeconds - a region covering
// only a couple of short paragraphs generates a correspondingly short
// clip rather than being padded out to some "musically coherent" floor.
func GenerateRegion(ctx context.Context, backend Backend, in Region) (Result, error) {
	served := in.TargetDurationSeconds + crossfadePaddingSeconds
	renderTarget := served + loopCrossfadeSeconds

	numChunks := int(math.Ceil(renderTarget / maxChunkSeconds))
	if numChunks < 1 {
		numChunks = 1
	}
	chunkSeconds := renderTarget / float64(numChunks)

	var res Result
	var out *wav.Clip
	var seedAudio []byte
	if in.Seed != nil {
		seed, err := wav.Decode(in.Seed)
		if err != nil {
			return Result{}, fmt.Errorf("decode seed: %w", err)
		}
		seedAudio = tailWavBytes(seed)
	}
	for i := 0; i < numChunks; i++ {
		var chunk *wav.Clip
		var err error
		if seedAudio == nil {
			chunk, err = generateChunk(ctx, backend, in.Prompt, in.NegativePrompt, chunkSeconds)
		} else {
			chunk, err = generateContinuationChunk(ctx, backend, in.Prompt, in.NegativePrompt, seedAudio, chunkSeconds)
		}
		if err != nil {
			return Result{}, fmt.Errorf("generate chunk %d/%d: %w", i+1, numChunks, err)
		}
		if out == nil {
			out = chunk
		} else {
			out, err = wav.Concat(out, chunk)
			if err != nil {
				return Result{}, fmt.Errorf("stitch chunk %d/%d: %w", i+1, numChunks, err)
			}
		}
		if i < numChunks-1 {
			// Only needed to seed the *next* chunk - skipped on the last
			// one, which has no successor to feed.
			seedAudio = tailWavBytes(chunk)
		}
	}

	if in.AmbiencePrompt != "" {
		amb, err := generateChunk(ctx, backend, in.AmbiencePrompt, "", AmbienceLoopSeconds+loopCrossfadeSeconds)
		if err != nil {
			return Result{}, fmt.Errorf("generate ambience loop: %w", err)
		}
		if res.AmbienceLoop, err = encodeLoop(amb, AmbienceLoopSeconds); err != nil {
			return Result{}, err
		}
	}

	music, err := encodeLoop(out, served)
	if err != nil {
		return Result{}, err
	}
	res.Music = music
	return res, nil
}

func encodeLoop(clip *wav.Clip, servedSeconds float64) ([]byte, error) {
	out := trimToLoopableClip(clip, servedSeconds, loopCrossfadeSeconds)
	return wav.Encode(out.Samples, out.SampleRate, out.Channels)
}

// trimToLoopableClip trims clip (rendered crossfadeSeconds longer than
// servedSeconds) to exactly servedSeconds, folding the overrun back over
// the head with an equal-power crossfade: the served clip's first
// crossfadeSeconds fade from the overrun (the audio that followed the
// served clip's last sample in the continuous render) into the clip's own
// head. So when playback loops, the wrap from the last sample to the
// first is the same continuous audio the model rendered - not a jump to a
// start that fades in from silence. The served clip's tail is untouched
// render, which also keeps it clean as the next region's continuation
// seed.
//
// Falls back to clip unmodified if it came back shorter than
// servedSeconds+crossfadeSeconds (a short/degenerate render) - better a
// clip a bit longer or shorter than requested than an out-of-bounds slice.
func trimToLoopableClip(clip *wav.Clip, servedSeconds, crossfadeSeconds float64) *wav.Clip {
	if clip.Channels <= 0 || clip.SampleRate <= 0 {
		return clip
	}
	fadeFrames := int(crossfadeSeconds * float64(clip.SampleRate))
	servedFrames := int(servedSeconds * float64(clip.SampleRate))
	fadeSamples := fadeFrames * clip.Channels
	servedSamples := servedFrames * clip.Channels
	if fadeFrames < 0 || servedFrames <= 0 || fadeFrames > servedFrames || servedSamples+fadeSamples > len(clip.Samples) {
		return clip
	}

	out := make([]float32, servedSamples)
	copy(out, clip.Samples[:servedSamples])
	overrun := clip.Samples[servedSamples : servedSamples+fadeSamples]
	for f := 0; f < fadeFrames; f++ {
		// Equal-power so the blend's loudness stays roughly constant
		// across it rather than dipping partway through.
		t := float64(f) / float64(fadeFrames)
		fadeIn := float32(math.Sin(t * math.Pi / 2))
		fadeOut := float32(math.Cos(t * math.Pi / 2))
		for c := 0; c < clip.Channels; c++ {
			i := f*clip.Channels + c
			out[i] = out[i]*fadeIn + overrun[i]*fadeOut
		}
	}
	return &wav.Clip{Samples: out, SampleRate: clip.SampleRate, Channels: clip.Channels}
}

// TileLoop returns seconds of loop (raw WAV bytes of a seamless loop, e.g.
// Result.AmbienceLoop) repeated end to end, starting offsetSeconds into
// it - callers vary the offset so neighbouring regions sharing one loop
// don't all start on the same sound.
func TileLoop(loop []byte, seconds, offsetSeconds float64) ([]byte, error) {
	clip, err := wav.Decode(loop)
	if err != nil {
		return nil, fmt.Errorf("decode loop: %w", err)
	}
	if clip.Channels <= 0 || clip.SampleRate <= 0 {
		return nil, fmt.Errorf("loop has no audio")
	}
	frames := len(clip.Samples) / clip.Channels
	if frames == 0 {
		return nil, fmt.Errorf("loop has no audio")
	}
	outFrames := int(seconds * float64(clip.SampleRate))
	start := int(offsetSeconds*float64(clip.SampleRate)) % frames
	if start < 0 {
		start += frames
	}
	out := make([]float32, 0, outFrames*clip.Channels)
	for pos := start; len(out) < outFrames*clip.Channels; pos = 0 {
		n := min(frames-pos, outFrames-len(out)/clip.Channels)
		out = append(out, clip.Samples[pos*clip.Channels:(pos+n)*clip.Channels]...)
	}
	return wav.Encode(out, clip.SampleRate, clip.Channels)
}

// generateChunk renders seconds of plain, unseeded audio.
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

// ambienceRelativeLevel is the ambience layer's RMS relative to the music
// layer's own in MixAmbience - a little under the music (about -4.4dB), so
// the place is clearly audible without the soundscape drowning the score.
const ambienceRelativeLevel = 0.6

// mixPeakCeiling is the peak MixAmbience scales the summed mix back down
// to if the two layers together would otherwise clip.
const mixPeakCeiling = 0.98

// MixAmbience layers ambience (raw WAV bytes, the setting's ambience loop
// tiled to the region's length - see TileLoop) under music (a
// GenerateRegion clip), returning the
// summed clip as WAV bytes. The layers are rendered separately rather
// than from one combined prompt - Stable Audio 3's training data tags a
// clip as either music or SFX, so one prompt asking for both tends to
// come back as one with a trace of the other - and separate stems are
// what let this set their balance: ambience is level-matched to
// ambienceRelativeLevel of the music's own RMS, whatever loudness each
// render happened to come out at. Both come from the same checkpoint, so
// the formats match; the mix is as long as the shorter of the two (the
// caller tiles ambience to the music's length, so any difference is a
// frame or two of rounding).
func MixAmbience(music, ambience []byte) ([]byte, error) {
	m, err := wav.Decode(music)
	if err != nil {
		return nil, fmt.Errorf("decode music: %w", err)
	}
	a, err := wav.Decode(ambience)
	if err != nil {
		return nil, fmt.Errorf("decode ambience: %w", err)
	}
	if m.SampleRate != a.SampleRate || m.Channels != a.Channels {
		return nil, fmt.Errorf("layer format mismatch: music %dHz/%dch, ambience %dHz/%dch", m.SampleRate, m.Channels, a.SampleRate, a.Channels)
	}
	n := min(len(m.Samples), len(a.Samples))
	gain := float32(0)
	if ar := rms(a.Samples[:n]); ar > 0 {
		gain = float32(ambienceRelativeLevel * rms(m.Samples[:n]) / ar)
	}
	out := make([]float32, n)
	var peak float32
	for i := range out {
		v := m.Samples[i] + a.Samples[i]*gain
		out[i] = v
		if v < 0 {
			v = -v
		}
		peak = max(peak, v)
	}
	if peak > mixPeakCeiling {
		scale := mixPeakCeiling / peak
		for i := range out {
			out[i] *= scale
		}
	}
	return wav.Encode(out, m.SampleRate, m.Channels)
}

func rms(samples []float32) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(samples)))
}
