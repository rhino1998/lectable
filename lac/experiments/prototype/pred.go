package lac

// Backward-adaptive prediction cascade. Every stage adapts from data the
// decoder has already reconstructed, so there's no side information.
//
// Float stages stay deterministic across platforms: every product goes
// through an explicit float64 conversion (the Go spec's guard against
// fused multiply-add), sums run in a fixed order, and the running input
// power is an exact integer.
//
// Each stage's weight update is deferred and fused into the next
// prediction for the same channel: that's one pass over the taps per
// sample instead of two. With a channel-interleaved history, the window a
// channel last predicted from sits `channels` entries back.

type fring struct {
	buf []float64
	n   int // window length
	lag int // extra history kept for the deferred update (= channels)
	pos int
}

func newFRing(n, lag int) *fring {
	return &fring{buf: make([]float64, n+lag+4096), n: n, lag: lag, pos: n + lag}
}

func (r *fring) push(v float64) float64 {
	if r.pos == len(r.buf) {
		copy(r.buf, r.buf[r.pos-r.n-r.lag:r.pos])
		r.pos = r.n + r.lag
	}
	old := r.buf[r.pos-r.n]
	r.buf[r.pos] = v
	r.pos++
	return old
}

type nlms struct {
	r   *fring
	w   [][]float64
	g   []float64 // pending update step per channel
	pow int64
	mu  float64
}

func newNLMS(order, channels int, mu float64) *nlms {
	s := &nlms{r: newFRing(order, channels), mu: mu, g: make([]float64, channels)}
	s.w = make([][]float64, channels)
	for c := range s.w {
		s.w[c] = make([]float64, order)
	}
	return s
}

func (s *nlms) predict(c int) float64 {
	r := s.r
	x := r.buf[r.pos-r.n : r.pos]
	w := s.w[c][:len(x)]
	g := s.g[c]
	var p0, p1, p2, p3 float64
	if g != 0 {
		xo := r.buf[r.pos-r.n-r.lag : r.pos-r.lag][:len(x)]
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
	return (p0 + p1) + (p2 + p3)
}

func (s *nlms) update(c int, err int32) {
	s.g[c] = s.mu * float64(err) / (float64(s.pow) + 1024)
}

func (s *nlms) push(v int32) {
	old := int64(s.r.push(float64(v)))
	s.pow += int64(v)*int64(v) - old*old
}

type iring struct {
	buf      []int32
	n, lag   int
	pos      int
}

func newIRing(n, lag int) *iring {
	return &iring{buf: make([]int32, n+lag+4096), n: n, lag: lag, pos: n + lag}
}

func (r *iring) push(v int32) {
	if r.pos == len(r.buf) {
		copy(r.buf, r.buf[r.pos-r.n-r.lag:r.pos])
		r.pos = r.n + r.lag
	}
	r.buf[r.pos] = v
	r.pos++
}

// sslms is a Monkey's-Audio-style integer sign-sign LMS stage. All
// arithmetic is integer, so it's deterministic.
type sslms struct {
	r, adapt *iring
	w        [][]int32
	shift    uint
	avg      []int32
}

func newSSLMS(order, channels int, shift uint) *sslms {
	s := &sslms{r: newIRing(order, channels), adapt: newIRing(order, channels), shift: shift, avg: make([]int32, channels)}
	s.w = make([][]int32, channels)
	for c := range s.w {
		s.w[c] = make([]int32, order)
	}
	return s
}

func (s *sslms) predict(c int) int32 {
	r := s.r
	x := r.buf[r.pos-r.n : r.pos]
	w := s.w[c][:len(x)]
	var p int64
	for i := range x {
		p += int64(w[i]) * int64(x[i])
	}
	return int32((p + 1<<(s.shift-1)) >> s.shift)
}

// update applies immediately (not deferred like nlms): push ages the
// newest adapt entries, and the update must see them before that.
func (s *sslms) update(c int, err int32) {
	a := s.adapt.buf[s.adapt.pos-s.r.n : s.adapt.pos]
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

func sat16(v int32) int32 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return v
}

func (s *sslms) push(c int, v int32) {
	s.r.push(sat16(v))
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
	s.adapt.push(m)
	// age recent deltas like MAC does
	a := s.adapt.buf[:s.adapt.pos]
	n := len(a)
	if s.r.n >= 8 {
		a[n-2] >>= 1
		a[n-3] >>= 1
		a[n-9] >>= 1
	}
}
