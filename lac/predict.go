package lac

import "math"

// predClamp bounds every prediction, which bounds every residual below
// 1<<25 (what the residual coder handles) for any input - including a
// crafted or corrupt stream that drives the adaptive weights wild.
const predClamp = 1 << 20

// clampRound rounds p to the nearest integer within ±predClamp. NaN maps
// to 0: Go leaves converting NaN or out-of-range floats to int
// implementation-defined, so it must never reach int32().
func clampRound(p float64) int32 {
	switch {
	case p >= predClamp:
		return predClamp
	case p <= -predClamp:
		return -predClamp
	case p != p:
		return 0
	}
	return int32(math.Floor(p + 0.5))
}

func clampInt(p int64) int32 {
	return int32(min(max(p, -predClamp), predClamp))
}

// stage is one predictor in the cascade. Its history is shared by all
// channels in interleaved order, so a stereo channel's prediction also
// sees the other channel's (already coded) samples; weights are per
// channel.
type stage interface {
	predict(c int) int32
	update(c int, err int32)
	push(c int, v int32)
}

// nlms is a normalized LMS stage in float64. Its weight update is
// deferred and fused into the channel's next prediction - one pass over
// the taps per sample instead of two. The window a channel last predicted
// from then sits `channels` entries back (lag).
type nlms struct {
	buf    []float64
	n, lag int
	pos    int
	w      [][]float64
	g      []float64 // pending update step per channel
	pow    int64     // exact sum of squares over the window
	mu     float64
}

func newNLMS(order, channels int, mu float64) *nlms {
	s := &nlms{buf: make([]float64, order+channels+4096), n: order, lag: channels, pos: order + channels, mu: mu, g: make([]float64, channels)}
	s.w = make([][]float64, channels)
	for c := range s.w {
		s.w[c] = make([]float64, order)
	}
	return s
}

func (s *nlms) predict(c int) int32 {
	x := s.buf[s.pos-s.n : s.pos]
	w := s.w[c][:len(x)]
	g := s.g[c]
	var p0, p1, p2, p3 float64
	if g != 0 {
		xo := s.buf[s.pos-s.n-s.lag : s.pos-s.lag][:len(x)]
		i := 0
		for ; i+4 <= len(x); i += 4 {
			w0 := w[i] + float64(g*xo[i])
			w1 := w[i+1] + float64(g*xo[i+1])
			w2 := w[i+2] + float64(g*xo[i+2])
			w3 := w[i+3] + float64(g*xo[i+3])
			w[i], w[i+1], w[i+2], w[i+3] = w0, w1, w2, w3
			p0 += float64(w0 * x[i])
			p1 += float64(w1 * x[i+1])
			p2 += float64(w2 * x[i+2])
			p3 += float64(w3 * x[i+3])
		}
		for ; i < len(x); i++ {
			w[i] += float64(g * xo[i])
			p0 += float64(w[i] * x[i])
		}
		s.g[c] = 0
	} else {
		i := 0
		for ; i+4 <= len(x); i += 4 {
			p0 += float64(w[i] * x[i])
			p1 += float64(w[i+1] * x[i+1])
			p2 += float64(w[i+2] * x[i+2])
			p3 += float64(w[i+3] * x[i+3])
		}
		for ; i < len(x); i++ {
			p0 += float64(w[i] * x[i])
		}
	}
	return clampRound((p0 + p1) + (p2 + p3))
}

func (s *nlms) update(c int, err int32) {
	s.g[c] = float64(s.mu*float64(err)) / (float64(s.pow) + 1024)
}

func (s *nlms) push(_ int, v int32) {
	if s.pos == len(s.buf) {
		copy(s.buf, s.buf[s.pos-s.n-s.lag:s.pos])
		s.pos = s.n + s.lag
	}
	old := int64(s.buf[s.pos-s.n])
	s.buf[s.pos] = float64(v)
	s.pos++
	s.pow += int64(v)*int64(v) - old*old
}

// sslms is a Monkey's-Audio-style sign-sign LMS stage in integers: input
// saturated to 16 bits, weights nudged by ±adapt (a magnitude-scaled sign
// of each input) in the direction of the error's sign.
type sslms struct {
	x, adapt []int32
	n, pos   int
	w        [][]int32
	shift    uint
	avg      []int32
}

func newSSLMS(order, channels int, shift uint) *sslms {
	s := &sslms{x: make([]int32, order+4096), adapt: make([]int32, order+4096), n: order, pos: order, shift: shift, avg: make([]int32, channels)}
	s.w = make([][]int32, channels)
	for c := range s.w {
		s.w[c] = make([]int32, order)
	}
	return s
}

func (s *sslms) predict(c int) int32 {
	x := s.x[s.pos-s.n : s.pos]
	w := s.w[c][:len(x)]
	var p int64
	for i := range x {
		p += int64(w[i]) * int64(x[i])
	}
	return clampInt((p + 1<<(s.shift-1)) >> s.shift)
}

// update applies right away (unlike nlms): push ages the newest adapt
// entries, and the update has to see them before that.
func (s *sslms) update(c int, err int32) {
	a := s.adapt[s.pos-s.n : s.pos]
	w := s.w[c][:len(a)]
	if err > 0 {
		for i := range a {
			w[i] += a[i]
		}
	} else if err < 0 {
		for i := range a {
			w[i] -= a[i]
		}
	}
}

func (s *sslms) push(c int, v int32) {
	if s.pos == len(s.x) {
		copy(s.x, s.x[s.pos-s.n:s.pos])
		copy(s.adapt, s.adapt[s.pos-s.n:s.pos])
		s.pos = s.n
	}
	s.x[s.pos] = min(max(v, -32768), 32767)
	av := v
	if av < 0 {
		av = -av
	}
	var m int32
	switch {
	case av > s.avg[c]*3:
		m = 32
	case av > (s.avg[c]*4)/3:
		m = 16
	case av > 0:
		m = 8
	}
	if v < 0 {
		m = -m
	}
	s.avg[c] += (av - s.avg[c]) / 16
	s.adapt[s.pos] = m
	s.pos++
	// Age the newest deltas, as Monkey's Audio does. order >= minSSLMSOrder
	// keeps these in range.
	s.adapt[s.pos-2] >>= 1
	s.adapt[s.pos-3] >>= 1
	s.adapt[s.pos-9] >>= 1
}

// mixer predicts the signal as a weighted sum of the stage predictions
// (a plain cascade is all weights 1), NLMS-adapted per channel.
type mixer struct {
	w  [][]float64
	mu float64
}

func newMixer(mu float64, channels, stages int) *mixer {
	m := &mixer{mu: mu, w: make([][]float64, channels)}
	for c := range m.w {
		m.w[c] = make([]float64, stages)
		for i := range m.w[c] {
			m.w[c][i] = 1
		}
	}
	return m
}

func (m *mixer) predict(c int, p []int32) int32 {
	var s float64
	for i, v := range p {
		s += float64(m.w[c][i] * float64(v))
	}
	return clampRound(s)
}

func (m *mixer) update(c int, p []int32, err int32) {
	var pow float64
	for _, v := range p {
		pow += float64(float64(v) * float64(v))
	}
	g := float64(m.mu*float64(err)) / (pow + 64)
	for i, v := range p {
		m.w[c][i] += float64(g * float64(v))
	}
}
