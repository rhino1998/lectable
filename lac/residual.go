package lac

import "math/bits"

// residual codes one channel's prediction residuals. The context is the
// bit length of a running mean of |e|; from it, k is the bit length a
// residual is expected to have. The actual length n is coded as an
// adaptive unary walk away from k (up or down), then the bit below the
// leading one with an adaptive model, then the remaining bits and the
// sign as one raw field. Averages about three binary decisions a sample.

const (
	nCtx     = 48
	maxLen   = 26 // |e| < 1<<25; predictions are clamped to keep it so
	walkLen  = 20
	avgShift = 4
)

type residual struct {
	avg  int32 // running mean of |e|, 4 fractional bits
	dir  [nCtx]prob
	up   [nCtx][walkLen]prob
	down [nCtx][walkLen]prob
	mant [nCtx][maxLen]prob
}

func newResidual() *residual {
	m := &residual{}
	for i := range m.dir {
		m.dir[i] = newProb()
		for j := range m.up[i] {
			m.up[i][j] = newProb()
			m.down[i][j] = newProb()
		}
		for j := range m.mant[i] {
			m.mant[i][j] = newProb()
		}
	}
	return m
}

func (m *residual) ctx() (c, k int) {
	c = bits.Len32(uint32(m.avg))
	if c >= nCtx {
		c = nCtx - 1
	}
	k = max(c-4, 0)
	return c, k
}

func (m *residual) adapt(a uint32) {
	a = min(a, 1<<20) // one wild sample shouldn't dominate
	m.avg += (int32(a<<4) - m.avg + 1<<(avgShift-1)) >> avgShift
}

func (m *residual) encode(e *encoder, v int32) {
	c, k := m.ctx()
	a := uint32(v)
	if v < 0 {
		a = uint32(-v)
	}
	n := bits.Len32(a)
	if n >= k {
		e.bit(&m.dir[c], 0)
		for j := range n - k {
			e.bit(&m.up[c][min(j, walkLen-1)], 1)
		}
		if n < maxLen-1 {
			e.bit(&m.up[c][min(n-k, walkLen-1)], 0)
		}
	} else {
		e.bit(&m.dir[c], 1)
		for j := range k - 1 - n {
			e.bit(&m.down[c][j], 1)
		}
		if n > 0 {
			e.bit(&m.down[c][k-1-n], 0)
		}
	}
	if n > 0 {
		var s uint32
		if v < 0 {
			s = 1
		}
		if n > 1 {
			rem := n - 2 // bits below the modelled one
			e.bit(&m.mant[c][n], int(a>>uint(rem))&1)
			r := (a&(1<<uint(rem)-1))<<1 | s
			if rem+1 > 16 {
				e.raw(r>>16, rem+1-16)
				e.raw(r&0xffff, 16)
			} else {
				e.raw(r, rem+1)
			}
		} else {
			e.raw(s, 1)
		}
	}
	m.adapt(a)
}

func (m *residual) decode(d *decoder) int32 {
	c, k := m.ctx()
	var n int
	if d.bit(&m.dir[c]) == 0 {
		n = k
		for n < maxLen-1 && d.bit(&m.up[c][min(n-k, walkLen-1)]) == 1 {
			n++
		}
	} else {
		n = k - 1
		for n > 0 && d.bit(&m.down[c][k-1-n]) == 1 {
			n--
		}
	}
	if n == 0 {
		m.adapt(0)
		return 0
	}
	var a, s uint32
	if n > 1 {
		rem := n - 2
		a = 2 | uint32(d.bit(&m.mant[c][n]))
		var r uint32
		if rem+1 > 16 {
			r = d.raw(rem+1-16) << 16
			r |= d.raw(16)
		} else {
			r = d.raw(rem + 1)
		}
		a = a<<uint(rem) | r>>1
		s = r & 1
	} else {
		a = 1
		s = d.raw(1)
	}
	m.adapt(a)
	if s == 1 {
		return -int32(a)
	}
	return int32(a)
}
