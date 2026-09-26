package lac

import (
	"math/bits"
	"os"
	"strconv"
)

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return d
}

var (
	avgShift = uint(envInt("LAC_AVGSHIFT", 4))
	mantK    = envInt("LAC_MANT", 2)
	ctxFine  = envInt("LAC_CTXFINE", 0)
)

// residual coder: adaptive context from a running mean of |e|. Codes
// bitlen(|e|) with an adaptive binary tree per context, then the top
// modelled mantissa bits with adaptive models and the rest raw, then the
// sign.

const (
	nCtx      = 48
	maxLen    = 26 // |e| < 2^25
	lenTree   = 32
	mantModel = 4
)

type residModel struct {
	avg  int32 // running mean of |e| << 4
	lens [nCtx][lenTree]prob
	mant [nCtx][maxLen][1 << mantModel]prob
	sign [3]prob
	last int
}

func newResidModel() *residModel {
	m := &residModel{}
	for i := range m.lens {
		for j := range m.lens[i] {
			m.lens[i][j] = newProb()
		}
		for j := range m.mant[i] {
			for k := range m.mant[i][j] {
				m.mant[i][j][k] = newProb()
			}
		}
	}
	for i := range m.sign {
		m.sign[i] = newProb()
	}
	return m
}

func (m *residModel) ctx() int {
	c := bits.Len32(uint32(m.avg)) // avg has 4 fractional bits
	if ctxFine != 0 {
		// half-octave steps: the bit below the leading one
		c *= 2
		if c >= 4 && (uint32(m.avg)>>uint(c/2-2))&1 != 0 {
			c++
		}
	}
	if c >= nCtx {
		c = nCtx - 1
	}
	return c
}

func (m *residModel) adapt(a uint32) {
	// a clamp keeps one wild sample from dominating
	if a > 1<<20 {
		a = 1 << 20
	}
	m.avg += (int32(a<<4) - m.avg + int32(1)<<(avgShift-1)) >> avgShift
}

func (m *residModel) signCtx() int { return m.last }

func (m *residModel) encode(e *encoder, v int32) {
	c := m.ctx()
	a := uint32(v)
	if v < 0 {
		a = uint32(-v)
	}
	n := bits.Len32(a) // 0..25
	// binary tree over 5 bits of n
	node := 1
	for i := 4; i >= 0; i-- {
		b := (n >> uint(i)) & 1
		e.bit(&m.lens[c][node], b)
		node = node<<1 | b
	}
	if n > 1 {
		rem := n - 1 // bits below the leading one
		k := rem
		if k > mantK {
			k = mantK
		}
		top := int(a>>uint(rem-k)) & (1<<uint(k) - 1)
		node := 1
		for i := k - 1; i >= 0; i-- {
			b := (top >> uint(i)) & 1
			e.bit(&m.mant[c][n][node], b)
			node = node<<1 | b
		}
		if rem > k {
			e.direct(a&(1<<uint(rem-k)-1), rem-k)
		}
	}
	if n > 0 {
		s := 0
		if v < 0 {
			s = 1
		}
		e.bit(&m.sign[m.last], s)
		m.last = 1 + s
	} else {
		m.last = 0
	}
	m.adapt(a)
}

func (m *residModel) decode(d *decoder) int32 {
	c := m.ctx()
	node := 1
	for i := 0; i < 5; i++ {
		node = node<<1 | d.bit(&m.lens[c][node])
	}
	n := node - 32
	var a uint32
	if n > 0 {
		a = 1
	}
	if n > 1 {
		rem := n - 1
		k := rem
		if k > mantK {
			k = mantK
		}
		node := 1
		for i := 0; i < k; i++ {
			node = node<<1 | d.bit(&m.mant[c][n][node])
		}
		a = a<<uint(k) | uint32(node-(1<<uint(k)))
		if rem > k {
			a = a<<uint(rem-k) | d.direct(rem-k)
		}
	}
	v := int32(a)
	if n > 0 {
		if d.bit(&m.sign[m.last]) == 1 {
			v = -v
			m.last = 2
		} else {
			m.last = 1
		}
	} else {
		m.last = 0
	}
	m.adapt(a)
	return v
}
