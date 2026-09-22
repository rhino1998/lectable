// Portions derived from Chromium's media/filters/audio_renderer_algorithm.cc
// and media/filters/wsola_internals.cc:
//
// Copyright 2013 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-chromium.txt file in this directory.

// Package wsola is a time-domain, pitch-preserving audio time-stretcher.
//
// A from-scratch Go port of Chromium's WSOLA (Waveform Similarity
// Overlap-Add) implementation - the algorithm behind the browser's own
// HTML5 <audio> playbackRate:
//
//	https://chromium.googlesource.com/chromium/chromium/+/51ed77e3f37a9a9b80d6d0a8259e84a8ca635259/media/filters/audio_renderer_algorithm.cc
//	https://chromium.googlesource.com/chromium/chromium/+/51ed77e3f37a9a9b80d6d0a8259e84a8ca635259/media/filters/wsola_internals.cc
//
// Ported as a single-shot batch transform rather than the original's
// streaming design: Chromium pulls frames from a live audio device and has
// to bound memory via a ring buffer (search_block_index_/
// target_block_index_ eviction, IncreaseQueueCapacity, ...) and bound
// per-callback CPU via a decimate-then-refine search. Here the whole clip
// already sits in memory before and after the call, so none of that
// queue/capacity machinery has a job to do, and the search itself - a
// position-by-position loop in the original - collapses to one
// cross-correlation per WSOLA iteration; that makes an exhaustive search
// over the ~30ms search window cheap enough that there's no need to trade
// accuracy for the decimated coarse search Chromium uses to stay within a
// real-time budget. Mono only, since every waveform this app ever
// stretches (TTS output, reference clips) is mono already.
//
// This is a direct, function-for-function port of tts-service/wsola.py
// (itself a from-scratch numpy port of the same Chromium source) - keep
// the two in sync structurally if either ever needs a fix.
package wsola

import "math"

// Audio outside this playback-rate range is muted rather than stretched,
// exactly as Chromium does - WSOLA's block-matching breaks down at extreme
// rates, so mangled audio is worse than silence.
const (
	MinPlaybackRate = 0.5
	MaxPlaybackRate = 4.0
)

const (
	olaWindowMS      = 20
	searchIntervalMS = 30
	// Frames around the previous match excluded from the next search, to
	// avoid converging on a "buzzy" repeating loop. A literal frame count
	// (not scaled by sample rate) in Chromium's source too - kept as-is for
	// fidelity.
	excludeIntervalFrames = 160
)

// periodicHann returns the first windowLength samples of a
// (windowLength+1)-point Hann window ("periodic"), which is what makes
// 50%-overlap-add reconstruct a constant-amplitude signal exactly.
func periodicHann(windowLength int) []float32 {
	out := make([]float32, windowLength)
	for n := 0; n < windowLength; n++ {
		out[n] = float32(0.5 * (1.0 - math.Cos(2.0*math.Pi*float64(n)/float64(windowLength))))
	}
	return out
}

// movingBlockEnergies returns the energy (sum of squares) of every
// length-blockSize sliding window in x, via a cumulative sum of squares
// (accumulated in float64, matching the original's explicit
// x.astype(np.float64) ** 2 - summing many squared float32 samples in
// float32 would lose precision differently than this does, and this
// function's whole purpose is numerical parity with the reference
// implementation, not just "sounds similar").
func movingBlockEnergies(x []float32, blockSize int) []float64 {
	cumsum := make([]float64, len(x)+1)
	for i, v := range x {
		sq := float64(v) * float64(v)
		cumsum[i+1] = cumsum[i] + sq
	}
	out := make([]float64, len(x)-blockSize+1)
	for i := range out {
		out[i] = cumsum[i+blockSize] - cumsum[i]
	}
	return out
}

// correlateValid is np.correlate(a, v, mode="valid"): a plain sliding-window
// dot product (not flipped, unlike convolution), producing
// len(a)-len(v)+1 outputs. WSOLA's search window is small (~30ms of
// audio), so a direct O(n*m) loop is fine - no need for an FFT-based
// implementation.
//
// Accumulates in float32, not float64: numpy's correlate on two float32
// arrays accumulates in float32, and this function's result feeds directly
// into an argmax decision (optimalIndex below) - matching numpy's actual
// rounding behavior here isn't a style choice, it's required for the
// argmax to land on the same index at near-tied candidates. Confirmed by
// direct comparison against the Python reference: accumulating in float64
// here (theoretically "more precise") diverges from it at exactly the
// points where two candidate blocks are near-equally good matches.
func correlateValid(a, v []float32) []float32 {
	n := len(a) - len(v) + 1
	if n <= 0 {
		return nil
	}
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		var sum float32
		for j, vj := range v {
			sum += a[i+j] * vj
		}
		out[i] = sum
	}
	return out
}

// optimalIndex is OptimalIndex/FullSearch: the offset within searchBlock
// whose window is most similar (normalized dot product) to targetBlock,
// excluding [excludeLo, excludeHi]. The returned index is relative to
// searchBlock.
func optimalIndex(searchBlock, targetBlock []float32, excludeLo, excludeHi int) int {
	blockSize := len(targetBlock)
	dotProducts := correlateValid(searchBlock, targetBlock)

	// energy_target = float(np.dot(target_block, target_block)) in the
	// reference - np.dot on float32 arrays also accumulates in float32,
	// same reasoning as correlateValid above.
	var energyTarget32 float32
	for _, v := range targetBlock {
		energyTarget32 += v * v
	}
	energyTarget := float64(energyTarget32)
	energyCandidates := movingBlockEnergies(searchBlock, blockSize)

	similarity := make([]float64, len(dotProducts))
	for i := range similarity {
		similarity[i] = float64(dotProducts[i]) / math.Sqrt(energyTarget*energyCandidates[i]+1e-12)
	}

	lo := excludeLo
	if lo < 0 {
		lo = 0
	}
	hi := excludeHi
	if hi > len(similarity)-1 {
		hi = len(similarity) - 1
	}
	if lo <= hi {
		for i := lo; i <= hi; i++ {
			similarity[i] = math.Inf(-1)
		}
	}

	best := 0
	for i := 1; i < len(similarity); i++ {
		if similarity[i] > similarity[best] {
			best = i
		}
	}
	return best
}

// peekWithZeroPrepend is PeekAudioWithZeroPrepend: reads a length-length
// window starting at offset, treating samples before index 0 as silence
// (happens for the first target/search block, which start before the clip
// begins).
func peekWithZeroPrepend(samples []float32, offset, length int) []float32 {
	dest := make([]float32, length)
	writeOffset := 0
	readOffset := offset
	numToRead := length
	if readOffset < 0 {
		numZeros := -readOffset
		if numZeros > numToRead {
			numZeros = numToRead
		}
		readOffset = 0
		numToRead -= numZeros
		writeOffset = numZeros
	}
	if numToRead > 0 {
		copy(dest[writeOffset:writeOffset+numToRead], samples[readOffset:readOffset+numToRead])
	}
	return dest
}

// TimeStretch speeds mono samples up/down by playbackRate with pitch
// preserved.
//
// playbackRate > 1 shortens (faster), < 1 lengthens (slower), matching
// Chromium's AudioRendererAlgorithm::FillBuffer convention (and this app's
// speed_multiplier fields).
func TimeStretch(samples []float32, sampleRate int, playbackRate float64) []float32 {
	n := len(samples)
	if n == 0 || playbackRate == 1.0 {
		out := make([]float32, n)
		copy(out, samples)
		return out
	}

	if playbackRate < MinPlaybackRate || playbackRate > MaxPlaybackRate {
		// Mirrors FillBuffer's muted fast/slow-forward path: outside the
		// supported range WSOLA can't preserve quality, so Chromium plays
		// silence at the sped-through rate instead of mangled audio.
		return make([]float32, int(float64(n)/playbackRate))
	}

	olaWindowSize := olaWindowMS * sampleRate / 1000
	olaWindowSize += olaWindowSize & 1 // force even
	olaHopSize := olaWindowSize / 2
	numCandidateBlocks := searchIntervalMS * sampleRate / 1000
	searchBlockCenterOffset := numCandidateBlocks/2 + (olaWindowSize/2 - 1)
	searchBlockSize := numCandidateBlocks + (olaWindowSize - 1)

	// Optimize the common ~1x case exactly like FillBuffer: if both a
	// slowed-down and sped-up window still span at least one full window,
	// playbackRate is close enough to 1 that WSOLA would degrade to a copy.
	slowerStep := int(math.Ceil(float64(olaWindowSize) * playbackRate))
	fasterStep := int(math.Ceil(float64(olaWindowSize) / playbackRate))
	if olaWindowSize <= fasterStep && slowerStep >= olaWindowSize {
		out := make([]float32, n)
		copy(out, samples)
		return out
	}

	olaWindow := periodicHann(olaWindowSize)
	transitionWindow := periodicHann(2 * olaWindowSize)

	output := make([]float32, int(float64(n)/playbackRate)+olaWindowSize*4)
	numCompleteFrames := 0
	outputTime := 0.0
	searchBlockIndex := 0
	targetBlockIndex := 0

	for targetBlockIndex+olaWindowSize <= n && searchBlockIndex+searchBlockSize <= n {
		var optimalBlock []float32
		var optimalIdx int

		// GetOptimalBlock: skip the search entirely if the natural
		// continuation of the output already lies within the search region.
		if targetBlockIndex >= searchBlockIndex && targetBlockIndex+olaWindowSize <= searchBlockIndex+searchBlockSize {
			optimalIdx = targetBlockIndex
			optimalBlock = peekWithZeroPrepend(samples, optimalIdx, olaWindowSize)
		} else {
			targetBlock := peekWithZeroPrepend(samples, targetBlockIndex, olaWindowSize)
			searchBlock := peekWithZeroPrepend(samples, searchBlockIndex, searchBlockSize)

			lastOptimal := targetBlockIndex - olaHopSize - searchBlockIndex
			excludeLo := lastOptimal - excludeIntervalFrames/2
			excludeHi := lastOptimal + excludeIntervalFrames/2
			localOptimal := optimalIndex(searchBlock, targetBlock, excludeLo, excludeHi)

			optimalIdx = localOptimal + searchBlockIndex
			optimalBlock = peekWithZeroPrepend(samples, optimalIdx, olaWindowSize)

			// Blend toward targetBlock (the "natural" continuation) rather
			// than jump-cutting to optimalBlock (the best match) outright.
			blended := make([]float32, olaWindowSize)
			for i := 0; i < olaWindowSize; i++ {
				blended[i] = optimalBlock[i]*transitionWindow[i] + targetBlock[i]*transitionWindow[olaWindowSize+i]
			}
			optimalBlock = blended
		}

		targetBlockIndex = optimalIdx + olaHopSize

		if numCompleteFrames+olaWindowSize > len(output) {
			grown := make([]float32, len(output)*2)
			copy(grown, output)
			output = grown
		}

		for i := 0; i < olaHopSize; i++ {
			output[numCompleteFrames+i] = output[numCompleteFrames+i]*olaWindow[olaHopSize+i] + optimalBlock[i]*olaWindow[i]
		}
		copy(output[numCompleteFrames+olaHopSize:numCompleteFrames+olaWindowSize], optimalBlock[olaHopSize:])
		numCompleteFrames += olaHopSize

		outputTime += float64(olaHopSize)
		searchBlockCenterIndex := int(outputTime*playbackRate + 0.5)
		searchBlockIndex = searchBlockCenterIndex - searchBlockCenterOffset
		// No RemoveOldInputFrames: that only exists to evict a bounded ring
		// buffer. We hold the whole clip, so nothing needs evicting.
	}

	// Fewer than one window's worth of future context is left, so no further
	// WSOLA iteration can run. Chromium's streaming version just leaves this
	// for a later FillBuffer call; a one-shot transform has no "later", so
	// append the leftover tail unstretched - targetBlockIndex already
	// points at the natural (rate-correct) continuation of the output.
	var tail []float32
	if targetBlockIndex < n {
		tail = samples[targetBlockIndex:]
	}
	end := numCompleteFrames + len(tail)
	if end > len(output) {
		grown := make([]float32, end)
		copy(grown, output)
		output = grown
	}
	copy(output[numCompleteFrames:end], tail)
	numCompleteFrames = end

	return output[:numCompleteFrames]
}
