// Package voicefx holds the voice effects a kind of being's lines can be
// given after they are generated - the Antinium's insect buzz, the Drowned
// Folk's underwater gurgle - which no voice model produces from a casting.
// An effect is plain signal processing on a finished clip: it keeps the
// clip's length (its word timings stay true) and its average level, and is
// the same on every line.
//
// The chains were tuned by ear on rendered lines (2026-10-12) as ffmpeg
// filter graphs and ported here; each stage names the filter it stands for.
package voicefx

import (
	"fmt"
	"math"
	"sort"

	"github.com/rhino1998/lectable/backend/internal/wav"
	"github.com/rhino1998/lectable/backend/internal/wsola"
)

// VoiceEffect names one of the effects ("" for none).
type VoiceEffect string

const (
	// Insectoid layers a pitched-up copy under the voice, flutters its
	// level fast enough to buzz, and adds a short metallic resonance.
	Insectoid VoiceEffect = "insectoid"
	// Underwater wobbles the pitch of a layered copy into a gurgle and
	// muffles the voice as if heard through water.
	Underwater VoiceEffect = "underwater"
)

// effect is one chain and the version it is at: Version changes whenever
// the chain would sound different, so clips made with the old one are not
// taken for the new (Tag).
type effect struct {
	version int
	process func(x []float32, rate int) []float32
}

var effects = map[VoiceEffect]effect{
	Insectoid:  {1, insectoid},
	Underwater: {1, underwater},
}

// Names lists the effects there are, in order.
func Names() []VoiceEffect {
	names := make([]VoiceEffect, 0, len(effects))
	for name := range effects {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// Valid reports whether name is an effect, or "" for none.
func Valid(name VoiceEffect) bool {
	_, ok := effects[name]
	return name == "" || ok
}

// Tag identifies name's chain as it is now, for a clip's content key: ""
// for no effect (or an unknown one, which Apply leaves alone too).
func Tag(name VoiceEffect) string {
	e, ok := effects[name]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s/%d", name, e.version)
}

// Apply runs effect name over a WAV clip and returns the processed WAV.
// The clip comes back as given for "" and for an effect this build doesn't
// know.
func Apply(name VoiceEffect, wavBytes []byte) ([]byte, error) {
	if _, ok := effects[name]; !ok {
		return wavBytes, nil
	}
	clip, err := wav.Decode(wavBytes)
	if err != nil {
		return nil, fmt.Errorf("voicefx: %w", err)
	}
	if clip.Channels != 1 {
		return wavBytes, nil // narration is mono; anything else is left alone
	}
	return wav.Encode(Process(name, clip.Samples, clip.SampleRate), clip.SampleRate, 1)
}

// Process runs effect name over mono samples: the result has the same
// length and average level as x. x is returned as is for "" or an unknown
// effect.
func Process(name VoiceEffect, x []float32, rate int) []float32 {
	e, ok := effects[name]
	if !ok || len(x) == 0 || rate <= 0 {
		return x
	}
	y := e.process(x, rate)
	if len(y) > len(x) {
		y = y[:len(x)]
	}
	for len(y) < len(x) {
		y = append(y, 0)
	}
	matchLevel(y, x)
	return y
}

func insectoid(x []float32, rate int) []float32 {
	dry := biquad(x, highpass(120, 0.707, rate))
	// asetrate*1.09,atempo=1/1.09: the same words at the same pace, pitched up.
	voice := mix(dry, 1, delay(pitchShift(dry, rate, 1.09), ms(14, rate)), 0.75)
	tremolo(voice, rate, 42, 0.60)
	// aecho 5|8.5 ms: delays this short ring like a small hard shell.
	voice = mix(mix(voice, 1, delay(voice, ms(5, rate)), 0.45), 1, delay(voice, ms(8.5, rate)), 0.30)
	return biquad(voice, peaking(3400, 1.2, 3, rate))
}

func underwater(x []float32, rate int) []float32 {
	dry := biquad(x, highpass(70, 0.707, rate))
	// vibrato f=11 d=0.35 on a copy: its pitch wavering against the dry voice gurgles.
	voice := mix(dry, 1, delay(vibrato(dry, rate, 11, 0.35), ms(9, rate)), 0.55)
	tremolo(voice, rate, 13, 0.30)
	voice = biquad(voice, lowpass(4500, 0.707, rate))
	voice = biquad(voice, peaking(320, 1, 4, rate))
	return mix(mix(voice, 1, delay(voice, ms(23, rate)), 0.30), 1, delay(voice, ms(37, rate)), 0.20)
}

func ms(v float64, rate int) int { return int(v*float64(rate)/1000 + 0.5) }

// mix is a*ga + b*gb over a's length.
func mix(a []float32, ga float32, b []float32, gb float32) []float32 {
	out := make([]float32, len(a))
	for i := range a {
		out[i] = a[i] * ga
		if i < len(b) {
			out[i] += b[i] * gb
		}
	}
	return out
}

// delay is x moved n samples later, at x's length.
func delay(x []float32, n int) []float32 {
	out := make([]float32, len(x))
	if n < len(x) {
		copy(out[n:], x)
	}
	return out
}

// pitchShift raises x's pitch by ratio without changing its length: the
// clip is slowed by ratio (wsola keeps the pitch) and then read ratio
// times as fast.
func pitchShift(x []float32, rate int, ratio float64) []float32 {
	slow := wsola.TimeStretch(x, rate, 1/ratio)
	out := make([]float32, len(x))
	for i := range out {
		pos := float64(i) * ratio
		j := int(pos)
		if j+1 >= len(slow) {
			break
		}
		f := float32(pos - float64(j))
		out[i] = slow[j]*(1-f) + slow[j+1]*f
	}
	return out
}

// tremolo swings x's level between 1-depth and 1, hz times a second.
func tremolo(x []float32, rate int, hz, depth float64) {
	for i := range x {
		g := 1 - depth/2 + depth/2*math.Sin(2*math.Pi*hz*float64(i)/float64(rate))
		x[i] *= float32(g)
	}
}

// vibrato wavers x's pitch hz times a second by reading it through a delay
// that swings between 0 and depth*5 ms.
func vibrato(x []float32, rate int, hz, depth float64) []float32 {
	span := depth * 0.005 * float64(rate)
	out := make([]float32, len(x))
	for i := range x {
		d := span * (0.5 + 0.5*math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
		pos := float64(i) - d
		if pos < 0 {
			continue
		}
		j := int(pos)
		f := float32(pos - float64(j))
		out[i] = x[j] * (1 - f)
		if j+1 < len(x) {
			out[i] += x[j+1] * f
		}
	}
	return out
}

// coeffs is a biquad section, normalized (a0 = 1).
type coeffs struct{ b0, b1, b2, a1, a2 float64 }

// The three sections are the Audio EQ Cookbook's.
func highpass(hz, q float64, rate int) coeffs {
	w := 2 * math.Pi * hz / float64(rate)
	alpha, cos := math.Sin(w)/(2*q), math.Cos(w)
	a0 := 1 + alpha
	return coeffs{(1 + cos) / 2 / a0, -(1 + cos) / a0, (1 + cos) / 2 / a0, -2 * cos / a0, (1 - alpha) / a0}
}

func lowpass(hz, q float64, rate int) coeffs {
	w := 2 * math.Pi * math.Min(hz, 0.45*float64(rate)) / float64(rate)
	alpha, cos := math.Sin(w)/(2*q), math.Cos(w)
	a0 := 1 + alpha
	return coeffs{(1 - cos) / 2 / a0, (1 - cos) / a0, (1 - cos) / 2 / a0, -2 * cos / a0, (1 - alpha) / a0}
}

func peaking(hz, q, gainDB float64, rate int) coeffs {
	w := 2 * math.Pi * math.Min(hz, 0.45*float64(rate)) / float64(rate)
	a := math.Pow(10, gainDB/40)
	alpha, cos := math.Sin(w)/(2*q), math.Cos(w)
	a0 := 1 + alpha/a
	return coeffs{(1 + alpha*a) / a0, -2 * cos / a0, (1 - alpha*a) / a0, -2 * cos / a0, (1 - alpha/a) / a0}
}

func biquad(x []float32, c coeffs) []float32 {
	out := make([]float32, len(x))
	var x1, x2, y1, y2 float64
	for i, s := range x {
		in := float64(s)
		y := c.b0*in + c.b1*x1 + c.b2*x2 - c.a1*y1 - c.a2*y2
		x2, x1, y2, y1 = x1, in, y1, y
		out[i] = float32(y)
	}
	return out
}

// matchLevel scales y to ref's RMS, and down again if that would clip.
func matchLevel(y, ref []float32) {
	rms := func(s []float32) float64 {
		var sum float64
		for _, v := range s {
			sum += float64(v) * float64(v)
		}
		return math.Sqrt(sum / float64(len(s)))
	}
	have := rms(y)
	if have == 0 {
		return
	}
	gain := rms(ref) / have
	var peak float64
	for _, v := range y {
		peak = math.Max(peak, math.Abs(float64(v))*gain)
	}
	if peak > 0.97 {
		gain *= 0.97 / peak
	}
	for i := range y {
		y[i] *= float32(gain)
	}
}
