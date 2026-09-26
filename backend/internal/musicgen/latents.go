package musicgen

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"math"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// LatentsBackend is a Backend that can also return Stable Audio's latents
// and take them back as inpainting context (satisfied by
// *ttsworker.Manager). With one, GenerateRegion seeds continuations from
// the model's own latents instead of re-encoding WAV tails (a ~17%-error
// round trip), and returns Result.SeedLatents for the next region.
type LatentsBackend interface {
	Backend
	StableAudioMediumLatents(ctx context.Context, req ttsproto.StableAudioRequest) ([]byte, *ttsproto.Latents, error)
}

// Stable Audio 3's latent frame: 4096 samples at 44.1 kHz (~93 ms). Checked
// against every reply.
const (
	latentHop  = 4096
	latentRate = 44100
)

// latentChunk is one generated chunk as the latents path tracks it: its new
// audio (whole frames) and the latent window it came from, where that new
// audio starts at frame seedFrames.
type latentChunk struct {
	audio      *wav.Clip
	lat        *ttsproto.Latents
	seedFrames int
	newFrames  int
}

// frameSeconds is a request duration/mask position that lands on frame n:
// a quarter-frame past it, so the worker's float-to-frame flooring can't
// fall one short.
func frameSeconds(n int) float64 {
	return (float64(n*latentHop) + latentHop/4) / latentRate
}

// generateLatentChunk renders newFrames of new audio after seed (nil:
// unseeded), returning exactly newFrames*latentHop samples and the window's
// latents.
func generateLatentChunk(ctx context.Context, backend LatentsBackend, prompt, negativePrompt string, seed *ttsproto.Latents, seedWAV []byte, seedFrames, newFrames int) (latentChunk, error) {
	req := ttsproto.StableAudioRequest{
		Prompt:          prompt,
		NegativePrompt:  negativePrompt,
		DurationSeconds: frameSeconds(seedFrames + newFrames),
	}
	switch {
	case seed != nil:
		req.InitLatents = seed
	case seedWAV != nil:
		req.InitAudioBase64 = base64.StdEncoding.EncodeToString(seedWAV)
	}
	if seedFrames > 0 {
		req.InpaintMaskStartSeconds = frameSeconds(seedFrames)
		req.InpaintMaskEndSeconds = req.DurationSeconds
	}
	wavBytes, lat, err := backend.StableAudioMediumLatents(ctx, req)
	if err != nil {
		return latentChunk{}, err
	}
	if lat == nil || lat.Kind != latents.KindLatents || lat.HopSamples != latentHop || lat.SampleRate != latentRate {
		return latentChunk{}, fmt.Errorf("unexpected latents from stable audio: %+v", lat)
	}
	if lat.Frames < seedFrames+newFrames {
		return latentChunk{}, fmt.Errorf("stable audio returned %d latent frames, need %d", lat.Frames, seedFrames+newFrames)
	}
	clip, err := wav.Decode(wavBytes)
	if err != nil {
		return latentChunk{}, err
	}
	if clip.SampleRate != latentRate || clip.Channels <= 0 {
		return latentChunk{}, fmt.Errorf("stable audio returned %d Hz x %d audio", clip.SampleRate, clip.Channels)
	}
	from, to := seedFrames*latentHop*clip.Channels, (seedFrames+newFrames)*latentHop*clip.Channels
	if to > len(clip.Samples) {
		return latentChunk{}, fmt.Errorf("stable audio returned %d samples, need %d", len(clip.Samples), to)
	}
	return latentChunk{
		audio:      &wav.Clip{Samples: clip.Samples[from:to], SampleRate: clip.SampleRate, Channels: clip.Channels},
		lat:        lat,
		seedFrames: seedFrames,
		newFrames:  newFrames,
	}, nil
}

// alignedSeedWAV trims a legacy WAV seed's head so its length is whole
// latent frames (so the chunk it seeds stays frame-aligned), returning it
// and its frame count. nil when it's shorter than a frame or unreadable.
func alignedSeedWAV(seed []byte) ([]byte, int) {
	clip, err := wav.Decode(seed)
	if err != nil || clip.Channels <= 0 || clip.SampleRate != latentRate {
		return nil, 0
	}
	tail := tailWavBytes(clip)
	if tail == nil {
		return nil, 0
	}
	if clip, err = wav.Decode(tail); err != nil {
		return nil, 0
	}
	frames := len(clip.Samples) / clip.Channels / latentHop
	if frames == 0 {
		return nil, 0
	}
	data, err := wav.Encode(clip.Samples[len(clip.Samples)-frames*latentHop*clip.Channels:], clip.SampleRate, clip.Channels)
	if err != nil {
		return nil, 0
	}
	return data, frames
}

// seedFramesFor is how many frames a continuation seed spans.
func seedFramesFor() int {
	return int(math.Round(continuationSeedSeconds * latentRate / latentHop))
}

// windowLatents gathers frames [from, to) of the region's frame-aligned
// timeline (chunk after chunk of new audio) from the chunks' latent
// windows.
func windowLatents(chunks []latentChunk, from, to int) (*ttsproto.Latents, error) {
	var pieces []*ttsproto.Latents
	start := 0
	for _, c := range chunks {
		end := start + c.newFrames
		lo, hi := max(from, start), min(to, end)
		if lo < hi {
			piece, err := latents.Slice(c.lat, c.seedFrames+lo-start, c.seedFrames+hi-start)
			if err != nil {
				return nil, err
			}
			pieces = append(pieces, piece)
		}
		start = end
	}
	if len(pieces) == 0 {
		return nil, fmt.Errorf("no latents cover frames [%d, %d)", from, to)
	}
	out, err := latents.Concat(pieces...)
	if err != nil {
		return nil, err
	}
	// A seed is plain conditioning, decoded as one window - chunk
	// boundaries between the pieces don't apply.
	delete(out.Meta, "chunk_frames")
	return out, nil
}

// generateRegionLatents is GenerateRegion's latents path - see
// LatentsBackend. Every chunk but the last is whole frames, and seeds are
// whole frames, so the region's audio timeline is frame-aligned throughout
// and in-region seeds end exactly where the next chunk begins.
func generateRegionLatents(ctx context.Context, backend LatentsBackend, in Region) (Result, error) {
	served := in.TargetDurationSeconds + crossfadePaddingSeconds
	renderTarget := served + loopCrossfadeSeconds
	totalFrames := int(math.Ceil(renderTarget * latentRate / latentHop))
	numChunks := max(1, int(math.Ceil(renderTarget/maxChunkSeconds)))
	perChunk := totalFrames / numChunks

	seedLat := in.SeedLatents
	var seedWAV []byte
	seedFrames := 0
	switch {
	case seedLat != nil:
		seedFrames = seedLat.Frames
	case in.Seed != nil:
		seedWAV, seedFrames = alignedSeedWAV(in.Seed)
	}

	var chunks []latentChunk
	var out *wav.Clip
	for i := 0; i < numChunks; i++ {
		newFrames := perChunk
		if i == numChunks-1 {
			newFrames = totalFrames - perChunk*(numChunks-1)
		}
		chunk, err := generateLatentChunk(ctx, backend, in.Prompt, in.NegativePrompt, seedLat, seedWAV, seedFrames, newFrames)
		if err != nil && i == 0 && seedLat != nil && strings.Contains(err.Error(), ttsproto.ErrLatentsModelMismatch) {
			// A seed from another checkpoint (an imported book, a model
			// upgrade): start this region fresh, like a "cut".
			log.Printf("musicgen: seed latents unusable, starting unseeded: %v", err)
			chunk, err = generateLatentChunk(ctx, backend, in.Prompt, in.NegativePrompt, nil, nil, 0, newFrames)
		}
		if err != nil {
			return Result{}, fmt.Errorf("generate chunk %d/%d: %w", i+1, numChunks, err)
		}
		chunks = append(chunks, chunk)
		if out == nil {
			out = chunk.audio
		} else if out, err = wav.Concat(out, chunk.audio); err != nil {
			return Result{}, fmt.Errorf("stitch chunk %d/%d: %w", i+1, numChunks, err)
		}
		// The next chunk continues from this one's last seedFramesFor()
		// frames of new audio, exactly.
		n := min(seedFramesFor(), chunk.newFrames)
		if seedLat, err = latents.Slice(chunk.lat, chunk.seedFrames+chunk.newFrames-n, chunk.seedFrames+chunk.newFrames); err != nil {
			return Result{}, err
		}
		seedWAV, seedFrames = nil, n
	}

	var res Result
	if in.AmbiencePrompt != "" {
		wavBytes, lat, err := backend.StableAudioMediumLatents(ctx, ttsproto.StableAudioRequest{
			Prompt:          in.AmbiencePrompt,
			DurationSeconds: AmbienceLoopSeconds + loopCrossfadeSeconds,
		})
		if err != nil {
			return Result{}, fmt.Errorf("generate ambience loop: %w", err)
		}
		if res.AmbienceLoop, err = LoopFromDecoded(wavBytes); err != nil {
			return Result{}, err
		}
		res.AmbienceLoopLatents = lat
	}
	music, err := encodeLoop(out, served)
	if err != nil {
		return Result{}, err
	}
	res.Music = music

	// The next region's seed: the served clip's last seconds (its tail is
	// untouched render - see trimToLoopableClip), to the nearest frame. Up
	// to half a frame off, which a region switch's client-side crossfade
	// covers.
	end := int(math.Round(served * latentRate / latentHop))
	end = min(end, totalFrames)
	if res.SeedLatents, err = windowLatents(chunks, max(0, end-seedFramesFor()), end); err != nil {
		return Result{}, fmt.Errorf("seed latents: %w", err)
	}
	return res, nil
}

// LoopFromDecoded turns an ambience render - as generated, or decoded back
// from Result.AmbienceLoopLatents (whose decode meta reproduce it) - into
// the served AmbienceLoopSeconds seamless loop. GenerateRegion's own
// recipe, so a loop rebuilt from latents matches the original exactly on
// the same backend.
func LoopFromDecoded(wavBytes []byte) ([]byte, error) {
	clip, err := wav.Decode(wavBytes)
	if err != nil {
		return nil, err
	}
	return encodeLoop(clip, AmbienceLoopSeconds)
}
