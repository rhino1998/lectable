package lac

// Binary adaptive range coder (LZMA-style carry-less with cache), 32-bit
// range, 12-bit probabilities. Deterministic integer arithmetic only.

const (
	probBits  = 12
	probOne   = 1 << probBits
	topValue  = 1 << 24
)

// prob is a two-rate adaptive bit model: the mean of a fast and a slow
// estimator of P(bit==0), each 16-bit precision.
type prob struct{ fast, slow uint16 }

func newProb() prob { return prob{1 << 15, 1 << 15} }

func (p *prob) p12() uint32 {
	v := (uint32(p.fast) + uint32(p.slow)) >> 5 // 17-bit sum -> 12 bits
	if v < 16 {
		v = 16
	} else if v > probOne-16 {
		v = probOne - 16
	}
	return v
}

func (p *prob) update(bit int) {
	if bit == 0 {
		p.fast += (65535 - p.fast) >> 4
		p.slow += (65535 - p.slow) >> 7
	} else {
		p.fast -= p.fast >> 4
		p.slow -= p.slow >> 7
	}
}

type encoder struct {
	low       uint64
	rng       uint32
	cache     byte
	cacheSize int64
	out       []byte
}

func newEncoder() *encoder { return &encoder{rng: 0xFFFFFFFF, cacheSize: 1} }

func (e *encoder) shiftLow() {
	if uint32(e.low) < 0xFF000000 || (e.low>>32) != 0 {
		carry := byte(e.low >> 32)
		temp := e.cache
		for {
			e.out = append(e.out, temp+carry)
			temp = 0xFF
			e.cacheSize--
			if e.cacheSize == 0 {
				break
			}
		}
		e.cache = byte(uint32(e.low) >> 24)
	}
	e.cacheSize++
	e.low = uint64(uint32(e.low) << 8)
}

func (e *encoder) bit(p *prob, bit int) {
	bound := (e.rng >> probBits) * p.p12()
	if bit == 0 {
		e.rng = bound
	} else {
		e.low += uint64(bound)
		e.rng -= bound
	}
	p.update(bit)
	for e.rng < topValue {
		e.rng <<= 8
		e.shiftLow()
	}
}

// raw writes n <= 16 equiprobable bits in one step.
func (e *encoder) raw(v uint32, n int) {
	if n == 0 {
		return
	}
	r := e.rng >> uint(n)
	e.low += uint64(v) * uint64(r)
	e.rng = r
	for e.rng < topValue {
		e.rng <<= 8
		e.shiftLow()
	}
}

// direct writes n equiprobable bits (n <= 24).
func (e *encoder) direct(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		e.rng >>= 1
		if (v>>uint(i))&1 != 0 {
			e.low += uint64(e.rng)
		}
		for e.rng < topValue {
			e.rng <<= 8
			e.shiftLow()
		}
	}
}

func (e *encoder) finish() []byte {
	for i := 0; i < 5; i++ {
		e.shiftLow()
	}
	return e.out
}

type decoder struct {
	code, rng uint32
	in        []byte
	pos       int
}

func newDecoder(in []byte) *decoder {
	d := &decoder{rng: 0xFFFFFFFF, in: in}
	for i := 0; i < 5; i++ {
		d.code = d.code<<8 | uint32(d.next())
	}
	return d
}

func (d *decoder) next() byte {
	if d.pos < len(d.in) {
		b := d.in[d.pos]
		d.pos++
		return b
	}
	d.pos++
	return 0
}

func (d *decoder) bit(p *prob) int {
	bound := (d.rng >> probBits) * p.p12()
	var bit int
	if d.code < bound {
		d.rng = bound
	} else {
		d.code -= bound
		d.rng -= bound
		bit = 1
	}
	p.update(bit)
	for d.rng < topValue {
		d.rng <<= 8
		d.code = d.code<<8 | uint32(d.next())
	}
	return bit
}

func (d *decoder) raw(n int) uint32 {
	if n == 0 {
		return 0
	}
	r := d.rng >> uint(n)
	v := d.code / r
	d.code -= v * r
	d.rng = r
	for d.rng < topValue {
		d.rng <<= 8
		d.code = d.code<<8 | uint32(d.next())
	}
	return v
}

func (d *decoder) direct(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		d.rng >>= 1
		var b uint32
		if d.code >= d.rng {
			d.code -= d.rng
			b = 1
		}
		v = v<<1 | b
		for d.rng < topValue {
			d.rng <<= 8
			d.code = d.code<<8 | uint32(d.next())
		}
	}
	return v
}
