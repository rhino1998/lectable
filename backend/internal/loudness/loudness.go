// Package loudness measures ITU-R BS.1770-4 ("EBU R128") integrated
// loudness (LUFS) for mono PCM audio - the perceptually-weighted,
// silence-gated "how loud does this actually sound" measure streaming/
// broadcast platforms normalize against, as opposed to a plain
// root-mean-square amplitude over the whole clip. internal/voicerefs uses
// this (NormalizeVolume) in place of plain RMS specifically because RMS is
// diluted by however much silence/pause a clip happens to contain: two
// clips with the same actual voice level but different amounts of
// leading/trailing/internal silence read as different loudnesses under
// plain RMS, so matching against it can over-amplify a pauses-heavy clip
// (and whatever background noise floor rides along with its voice) well
// past what the voice itself actually needs. BS.1770's own gating (below)
// exists to solve exactly this problem for real broadcast content.
//
// Two building blocks, both from scratch:
//   - K-weighting (kWeight): two cascaded biquad filters - a high shelf
//     (approximating head/ear acoustics, boosting above ~2kHz) then a
//     high-pass (approximating the "RLB" curve, attenuating rumble below
//     ~40Hz) - applied before any energy measurement. Filter coefficients
//     are derived per sample rate from the (fc, Q, gain) analog design
//     parameters the reference implementation pyloudnorm
//     (https://github.com/csteinmetz1/pyloudnorm) uses, via the standard
//     RBJ Audio EQ Cookbook bilinear-transform biquad formulas -
//     deliberately not the spec's own fixed 48kHz-only coefficient table,
//     since these reference clips render at whatever sample rate
//     audio.cpp itself produces (see internal/wav), not necessarily
//     48kHz. This is an approximation of the certified-meter curve, the
//     same tradeoff every open-source implementation following this
//     approach accepts - not bit-exact against a specific certified
//     meter, but the right shape, and (importantly for this package's own
//     use) exactly reproducible between any two clips it measures.
//   - Gated block loudness (Integrated): 400ms windows, 75% overlap
//     (100ms hop), an absolute gate at -70 LUFS (drops actual silence
//     outright), then a relative gate 10 LU below the mean of whatever
//     survived the absolute gate (drops the quietest remaining passages
//     too) - averaging only what's left. A clip shorter than one 400ms
//     window is treated as a single block rather than producing no
//     measurement at all.
package loudness

import "math"

const (
	// blockSeconds/hopSeconds are BS.1770's own gating-block window: 400ms
	// blocks, 100ms hop (75% overlap).
	blockSeconds = 0.4
	hopSeconds   = 0.1

	// absoluteGateLUFS is BS.1770's fixed "this block is just silence,
	// never count it at all" threshold.
	absoluteGateLUFS = -70.0
	// relativeGateLU is how far below the absolute-gated mean a block can
	// be before the second (relative) gate drops it too.
	relativeGateLU = 10.0
)

// Integrated returns samples' BS.1770 integrated loudness in LUFS -
// negative infinity if samples has no content that survives gating at all
// (empty, or effectively silent throughout). Callers should check
// math.IsInf(result, -1) before feeding it to GainFor - see
// internal/voicerefs.NormalizeVolume. Mono only, matching every reference
// clip this package ever measures (internal/wav.Clip.Samples is always
// interleaved-mono for those).
func Integrated(samples []float32, sampleRate int) float64 {
	if len(samples) == 0 || sampleRate <= 0 {
		return math.Inf(-1)
	}
	weighted := kWeight(samples, sampleRate)

	blockSize := int(blockSeconds * float64(sampleRate))
	if blockSize <= 0 || blockSize > len(weighted) {
		blockSize = len(weighted) // shorter than one window: treat it as a single block
	}
	hopSize := int(hopSeconds * float64(sampleRate))
	if hopSize <= 0 {
		hopSize = blockSize
	}

	var z []float64
	for start := 0; start+blockSize <= len(weighted); start += hopSize {
		z = append(z, meanSquare(weighted[start:start+blockSize]))
	}
	if len(z) == 0 {
		return math.Inf(-1)
	}

	absGated, ok := gate(z, absoluteGateLUFS)
	if !ok {
		return math.Inf(-1)
	}
	relativeThreshold := loudnessOf(mean(absGated)) - relativeGateLU
	relGated, ok := gate(absGated, relativeThreshold)
	if !ok {
		return math.Inf(-1)
	}
	return loudnessOf(mean(relGated))
}

// GainFor returns the linear gain factor that rescales a clip currently
// measured at currentLUFS so it reads as targetLUFS instead - neither
// argument should be an infinite Integrated result (see its own doc
// comment); callers check for that themselves since "no gain makes sense"
// is a different condition from "here's the gain."
func GainFor(currentLUFS, targetLUFS float64) float64 {
	return math.Pow(10, (targetLUFS-currentLUFS)/20)
}

// gate returns the z (mean-square) values whose own loudnessOf is at or
// above threshold, and whether any survived at all.
func gate(z []float64, threshold float64) ([]float64, bool) {
	kept := z[:0:0]
	for _, zi := range z {
		if loudnessOf(zi) >= threshold {
			kept = append(kept, zi)
		}
	}
	return kept, len(kept) > 0
}

// loudnessOf converts one block's (or the whole signal's) mean-square
// value into LUFS via BS.1770's fixed calibration offset. z == 0 (digital
// silence) maps to negative infinity, which sorts below every real
// threshold in gate/Integrated without needing its own special case.
func loudnessOf(z float64) float64 {
	if z <= 0 {
		return math.Inf(-1)
	}
	return -0.691 + 10*math.Log10(z)
}

func mean(z []float64) float64 {
	var sum float64
	for _, v := range z {
		sum += v
	}
	return sum / float64(len(z))
}

func meanSquare(samples []float64) float64 {
	var sum float64
	for _, s := range samples {
		sum += s * s
	}
	return sum / float64(len(samples))
}

// kWeight runs samples through BS.1770's two-stage K-weighting pre-filter
// (see this package's own doc comment) and returns the filtered signal as
// float64 for the energy math above.
func kWeight(samples []float32, sampleRate int) []float64 {
	out := make([]float64, len(samples))
	for i, s := range samples {
		out[i] = float64(s)
	}
	// Stage 1: high shelf modeling head/ear acoustics.
	highShelf(1681.9744509555319, 3.99984385397, 0.7071752369554193, sampleRate).applyInPlace(out)
	// Stage 2: high-pass approximating the RLB low-frequency curve.
	highPass(38.13547087613982, 0.5003270373238773, sampleRate).applyInPlace(out)
	return out
}

// biquad is a Direct Form I second-order IIR section, already normalized
// so its own a0 == 1 - the shape both K-weighting stages take.
type biquad struct {
	b0, b1, b2 float64
	a1, a2     float64
}

// applyInPlace runs samples through the filter in place, zero initial
// state - each reference clip is its own self-contained signal, with
// nothing to carry state in from.
func (f biquad) applyInPlace(samples []float64) {
	var x1, x2, y1, y2 float64
	for i, x0 := range samples {
		y0 := f.b0*x0 + f.b1*x1 + f.b2*x2 - f.a1*y1 - f.a2*y2
		samples[i] = y0
		x2, x1 = x1, x0
		y2, y1 = y1, y0
	}
}

// highShelf builds an RBJ Audio EQ Cookbook high-shelf biquad (gainDB of
// boost above fc) for sampleRate.
func highShelf(fc, gainDB, q float64, sampleRate int) biquad {
	a := math.Pow(10, gainDB/40)
	w0 := 2 * math.Pi * fc / float64(sampleRate)
	cosw0, sinw0 := math.Cos(w0), math.Sin(w0)
	alpha := sinw0 / (2 * q)
	sqrtA := math.Sqrt(a)

	b0 := a * ((a + 1) + (a-1)*cosw0 + 2*sqrtA*alpha)
	b1 := -2 * a * ((a - 1) + (a+1)*cosw0)
	b2 := a * ((a + 1) + (a-1)*cosw0 - 2*sqrtA*alpha)
	a0 := (a + 1) - (a-1)*cosw0 + 2*sqrtA*alpha
	a1 := 2 * ((a - 1) - (a+1)*cosw0)
	a2 := (a + 1) - (a-1)*cosw0 - 2*sqrtA*alpha

	return biquad{b0: b0 / a0, b1: b1 / a0, b2: b2 / a0, a1: a1 / a0, a2: a2 / a0}
}

// highPass builds an RBJ Audio EQ Cookbook high-pass biquad for
// sampleRate.
func highPass(fc, q float64, sampleRate int) biquad {
	w0 := 2 * math.Pi * fc / float64(sampleRate)
	cosw0, sinw0 := math.Cos(w0), math.Sin(w0)
	alpha := sinw0 / (2 * q)

	b0 := (1 + cosw0) / 2
	b1 := -(1 + cosw0)
	b2 := (1 + cosw0) / 2
	a0 := 1 + alpha
	a1 := -2 * cosw0
	a2 := 1 - alpha

	return biquad{b0: b0 / a0, b1: b1 / a0, b2: b2 / a0, a1: a1 / a0, a2: a2 / a0}
}
