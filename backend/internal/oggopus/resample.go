package oggopus

import "math"

// resampleHalfTaps is the windowed-sinc filter's half-width, in input
// samples at the filter's cutoff - 32 taps is well past audible for a
// 44.1->48kHz conversion.
const resampleHalfTaps = 16

// resample converts interleaved samples from one rate to another with a
// polyphase windowed-sinc (Blackman) filter. The phase table is exact for
// the rational ratio to/from (160/147 for 44.1->48kHz), so there's no
// per-sample trig and no drift.
func resample(in []float32, channels, from, to int) []float32 {
	g := gcd(from, to)
	up, down := to/g, from/g
	// Cut off at the lower Nyquist, a little early for the window's
	// transition band.
	cutoff := 0.97 * math.Min(1, float64(to)/float64(from))
	half := int(math.Ceil(resampleHalfTaps / cutoff))
	taps := 2 * half

	// table[p][j] weighs input sample base+j-half+1 for output phase p,
	// whose position lies p/up of the way past input sample base.
	table := make([]float32, up*taps)
	for p := range up {
		row := table[p*taps : (p+1)*taps]
		var sum float64
		for j := range taps {
			d := float64(j-half+1) - float64(p)/float64(up)
			u := d / float64(half)
			if u <= -1 || u >= 1 {
				continue
			}
			win := 0.42 + 0.5*math.Cos(math.Pi*u) + 0.08*math.Cos(2*math.Pi*u)
			v := cutoff * sinc(cutoff*d) * win
			row[j] = float32(v)
			sum += v
		}
		for j := range row { // unity DC gain for every phase
			row[j] = float32(float64(row[j]) / sum)
		}
	}

	inFrames := len(in) / channels
	outFrames := int((int64(inFrames)*int64(up) + int64(down) - 1) / int64(down))
	out := make([]float32, outFrames*channels)
	for n := range outFrames {
		pos := int64(n) * int64(down)
		base := int(pos / int64(up))
		row := table[int(pos%int64(up))*taps:][:taps]
		for c := range channels {
			var acc float32
			for j, w := range row {
				k := base + j - half + 1
				if k < 0 || k >= inFrames {
					continue
				}
				acc += w * in[k*channels+c]
			}
			out[n*channels+c] = acc
		}
	}
	return out
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
