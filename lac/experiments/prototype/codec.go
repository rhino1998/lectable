package lac

import (
	"encoding/binary"
	"errors"
	"math"
)

type StageKind uint8

const (
	KindNLMS  StageKind = 1
	KindSSLMS StageKind = 2
)

type Stage struct {
	Kind  StageKind
	Order int
	Mu    float64 // NLMS step
	Shift uint    // SSLMS shift
}

type Config struct {
	PreEmph bool
	MixMu   float64 // >0: adaptive mix of stage predictions
	Stages  []Stage
}

type stage interface {
	pred(c int) int32
	upd(c int, err int32)
	in(c int, v int32)
}

type nlmsStage struct{ *nlms }

func (s nlmsStage) pred(c int) int32 {
	p := s.predict(c)
	if p > 1<<30 {
		p = 1 << 30
	} else if p < -(1 << 30) {
		p = -(1 << 30)
	}
	return int32(math.Floor(p + 0.5))
}
func (s nlmsStage) upd(c int, e int32) { s.update(c, e) }
func (s nlmsStage) in(c int, v int32)  { s.push(v) }

type sslmsStage struct{ *sslms }

func (s sslmsStage) pred(c int) int32   { return s.predict(c) }
func (s sslmsStage) upd(c int, e int32) { s.update(c, e) }
func (s sslmsStage) in(c int, v int32)  { s.push(c, v) }

func build(cfg Config, ch int) []stage {
	var st []stage
	for _, s := range cfg.Stages {
		switch s.Kind {
		case KindNLMS:
			st = append(st, nlmsStage{newNLMS(s.Order, ch, s.Mu)})
		case KindSSLMS:
			st = append(st, sslmsStage{newSSLMS(s.Order, ch, s.Shift)})
		}
	}
	return st
}

// Encode compresses interleaved 16-bit PCM with one config.
func EncodeWith(pcm []int16, ch, rate int, cfg Config) []byte {
	cfg.Stages = append([]Stage(nil), cfg.Stages...)
	for i := range cfg.Stages {
		cfg.Stages[i].Mu = float64(float32(cfg.Stages[i].Mu)) // as stored
	}
	cfg.MixMu = float64(float32(cfg.MixMu))
	hdr := header(ch, rate, len(pcm)/ch, cfg)
	st := build(cfg, ch)
	models := make([]rmodel, ch)
	for c := range models {
		if useResid2 {
			models[c] = newResidModel2()
		} else {
			models[c] = newResidModel()
		}
	}
	prev := make([]int32, ch)
	mx := newMixer(cfg, ch, len(st))
	p := make([]int32, len(st))
	inp := make([]int32, len(st))
	e := newEncoder()
	for i := 0; i < len(pcm); i += ch {
		for c := 0; c < ch; c++ {
			x := int32(pcm[i+c])
			v := x
			if cfg.PreEmph {
				v = x - (prev[c]*31)>>5
				prev[c] = x
			}
			sIn := v
			for k, s := range st {
				p[k] = s.pred(c)
				inp[k] = v
				v -= p[k]
			}
			if mx != nil {
				v = sIn - mx.pred(c, p)
				mx.upd(c, p, v)
			}
			models[c].encode(e, v)
			for k, s := range st {
				s.upd(c, inp[k]-p[k])
				s.in(c, inp[k])
			}
		}
	}
	return append(hdr, e.finish()...)
}

func header(ch, rate, frames int, cfg Config) []byte {
	b := []byte("LAC1")
	b = append(b, byte(ch))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate))
	b = binary.LittleEndian.AppendUint32(b, uint32(frames))
	pe := byte(0)
	if cfg.PreEmph {
		pe = 1
	}
	b = append(b, pe, byte(len(cfg.Stages)))
	b = binary.LittleEndian.AppendUint32(b, math.Float32bits(float32(cfg.MixMu)))
	for _, s := range cfg.Stages {
		b = append(b, byte(s.Kind))
		b = binary.LittleEndian.AppendUint16(b, uint16(s.Order))
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(float32(s.Mu)))
		b = append(b, byte(s.Shift))
	}
	return b
}

func Decode(b []byte) (pcm []int16, ch, rate int, err error) {
	if len(b) < 15 || string(b[:4]) != "LAC1" {
		return nil, 0, 0, errors.New("not LAC1")
	}
	ch = int(b[4])
	rate = int(binary.LittleEndian.Uint32(b[5:]))
	frames := int(binary.LittleEndian.Uint32(b[9:]))
	var cfg Config
	cfg.PreEmph = b[13] == 1
	n := int(b[14])
	cfg.MixMu = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[15:])))
	p := 19
	for i := 0; i < n; i++ {
		var s Stage
		s.Kind = StageKind(b[p])
		s.Order = int(binary.LittleEndian.Uint16(b[p+1:]))
		s.Mu = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[p+3:])))
		s.Shift = uint(b[p+7])
		cfg.Stages = append(cfg.Stages, s)
		p += 8
	}
	st := build(cfg, ch)
	models := make([]rmodel, ch)
	for c := range models {
		if useResid2 {
			models[c] = newResidModel2()
		} else {
			models[c] = newResidModel()
		}
	}
	prev := make([]int32, ch)
	mx := newMixer(cfg, ch, len(st))
	pr := make([]int32, len(st))
	d := newDecoder(b[p:])
	pcm = make([]int16, frames*ch)
	for i := 0; i < len(pcm); i += ch {
		for c := 0; c < ch; c++ {
			for k, s := range st {
				pr[k] = s.pred(c)
			}
			v := models[c].decode(d)
			if mx != nil {
				r := v
				v += mx.pred(c, pr)
				mx.upd(c, pr, r)
				// rebuild the plain cascade residual from the signal
				u := v
				for k := range st {
					u -= pr[k]
				}
				v = u
			}
			for k := len(st) - 1; k >= 0; k-- {
				e := v
				v += pr[k]
				st[k].upd(c, e)
				st[k].in(c, v)
			}
			x := v
			if cfg.PreEmph {
				x = v + (prev[c]*31)>>5
				prev[c] = x
			}
			pcm[i+c] = int16(x)
		}
	}
	return pcm, ch, rate, nil
}

// mixer predicts the signal as a weighted sum of the stage predictions
// (a plain cascade is all weights 1), NLMS-adapted per channel.
type mixer struct {
	w  [][]float64
	mu float64
}

func newMixer(cfg Config, ch, n int) *mixer {
	if cfg.MixMu <= 0 || n == 0 {
		return nil
	}
	m := &mixer{mu: cfg.MixMu, w: make([][]float64, ch)}
	for c := range m.w {
		m.w[c] = make([]float64, n)
		for i := range m.w[c] {
			m.w[c][i] = 1
		}
	}
	return m
}

func (m *mixer) pred(c int, p []int32) int32 {
	var s float64
	for i, v := range p {
		s += float64(m.w[c][i] * float64(v))
	}
	if s > 1<<30 {
		s = 1 << 30
	} else if s < -(1 << 30) {
		s = -(1 << 30)
	}
	return int32(math.Floor(s + 0.5))
}

func (m *mixer) upd(c int, p []int32, err int32) {
	var pow float64
	for _, v := range p {
		pow += float64(float64(v) * float64(v))
	}
	g := m.mu * float64(err) / (pow + 64)
	for i, v := range p {
		m.w[c][i] += float64(g * float64(v))
	}
}

type rmodel interface {
	encode(e *encoder, v int32)
	decode(d *decoder) int32
}
