package lac

import "math/bits"

// residModel2: fewer binary decisions per sample. The bit length n of |e|
// is coded as an adaptive unary walk away from k, the length its context
// (running mean of |e|) predicts; then one modelled mantissa bit; then the
// remaining mantissa bits and the sign as one raw field.

const r2Walk = 20

type residModel2 struct {
	avg  int32
	dir  [nCtx]prob
	up   [nCtx][r2Walk]prob
	down [nCtx][r2Walk]prob
	mant [nCtx][maxLen]prob
}

var useResid2 = envInt("LAC_R2", 1) != 0

func newResidModel2() *residModel2 {
	m := &residModel2{}
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

func (m *residModel2) ctx() (c, k int) {
	c = bits.Len32(uint32(m.avg))
	if c >= nCtx {
		c = nCtx - 1
	}
	k = c - 4
	if k < 0 {
		k = 0
	}
	return c, k
}

func (m *residModel2) adapt(a uint32) {
	if a > 1<<20 {
		a = 1 << 20
	}
	m.avg += (int32(a<<4) - m.avg + 8) >> 4
}

func (m *residModel2) encode(e *encoder, v int32) {
	c, k := m.ctx()
	a := uint32(v)
	if v < 0 {
		a = uint32(-v)
	}
	n := bits.Len32(a)
	if n >= k {
		e.bit(&m.dir[c], 0)
		for j := 0; j < n-k; j++ {
			e.bit(&m.up[c][min(j, r2Walk-1)], 1)
		}
		if n < maxLen-1 {
			e.bit(&m.up[c][min(n-k, r2Walk-1)], 0)
		}
	} else {
		e.bit(&m.dir[c], 1)
		for j := 0; j < k-1-n; j++ {
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

func (m *residModel2) decode(d *decoder) int32 {
	c, k := m.ctx()
	var n int
	if d.bit(&m.dir[c]) == 0 {
		n = k
		for n < maxLen-1 && d.bit(&m.up[c][min(n-k, r2Walk-1)]) == 1 {
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
