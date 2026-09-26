package lac

// Adaptive binary range coder (LZMA-style carry propagation), 32-bit
// range, 12-bit probabilities. Integer arithmetic only.

const (
	probBits = 12
	probOne  = 1 << probBits
	topValue = 1 << 24
)

// prob is an adaptive estimate of P(bit == 0): the mean of a fast and a
// slow exponential estimator, each with 16 bits of precision.
type prob struct{ fast, slow uint16 }

func newProb() prob { return prob{1 << 15, 1 << 15} }

func (p *prob) p12() uint32 {
	v := (uint32(p.fast) + uint32(p.slow)) >> 5
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

func newEncoder(out []byte) *encoder { return &encoder{rng: 0xFFFFFFFF, cacheSize: 1, out: out} }

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

// raw writes n (1..16) equiprobable bits in one step.
func (e *encoder) raw(v uint32, n int) {
	r := e.rng >> uint(n)
	e.low += uint64(v) * uint64(r)
	e.rng = r
	for e.rng < topValue {
		e.rng <<= 8
		e.shiftLow()
	}
}

func (e *encoder) finish() []byte {
	for range 5 {
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
	for range 5 {
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

// overrun reports whether decoding has read well past the end of the
// payload, which only a corrupt stream does (a valid one ends within the
// 5 bytes finish flushes).
func (d *decoder) overrun() bool { return d.pos > len(d.in)+8 }

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
	r := d.rng >> uint(n)
	v := d.code / r
	if v >= 1<<uint(n) { // only reachable from a corrupt stream
		v = 1<<uint(n) - 1
	}
	d.code -= v * r
	d.rng = r
	for d.rng < topValue {
		d.rng <<= 8
		d.code = d.code<<8 | uint32(d.next())
	}
	return v
}
