package lac

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
)

const (
	magic   = "LAC"
	version = 1

	maxChannels   = 8
	maxStages     = 8
	maxOrder      = 2048
	minSSLMSOrder = 16 // sslms.push ages the adapt entry 9 back
	maxSamples    = 1<<31 - 1
)

// ErrCorrupt is returned (wrapped) for a stream that fails validation.
var ErrCorrupt = errors.New("lac: corrupt stream")

// Preset selects the predictor cascade. The chosen cascade is written into
// the stream, so decoding never depends on these definitions.
type Preset int

const (
	// PresetAuto picks PresetSpeech for mono audio, PresetMusic otherwise.
	PresetAuto Preset = iota
	// PresetSpeech is tuned on TTS speech: ~10% smaller than FLAC -8,
	// ~125x real time on mono 24 kHz, but only ~33x on stereo 44.1 kHz.
	PresetSpeech
	// PresetMusic is the fast cascade, for stereo music and ambience:
	// ~150x real time on stereo 44.1 kHz, ~570x on mono 24 kHz.
	PresetMusic
)

type stageKind uint8

const (
	kindNLMS  stageKind = 1
	kindSSLMS stageKind = 2
)

type stageSpec struct {
	kind  stageKind
	order int
	mu    float64 // nlms step
	shift uint    // sslms weight scale
}

type config struct {
	preEmph bool
	mixMu   float64 // 0: plain cascade
	stages  []stageSpec
}

func presetConfig(p Preset, channels int) config {
	if p == PresetAuto {
		p = PresetMusic
		if channels == 1 {
			p = PresetSpeech
		}
	}
	if p == PresetSpeech {
		return config{preEmph: true, mixMu: 0.002, stages: []stageSpec{
			{kind: kindNLMS, order: 16, mu: 0.016},
			{kind: kindSSLMS, order: 256, shift: 13},
			{kind: kindSSLMS, order: 32, shift: 10},
			{kind: kindSSLMS, order: 16, shift: 11},
		}}
	}
	return config{preEmph: true, stages: []stageSpec{
		{kind: kindNLMS, order: 32, mu: 0.008},
		{kind: kindSSLMS, order: 16, shift: 11},
	}}
}

func (c config) validate() error {
	if c.mixMu < 0 || c.mixMu > 1 || c.mixMu != c.mixMu {
		return fmt.Errorf("%w: mix step %v", ErrCorrupt, c.mixMu)
	}
	if len(c.stages) > maxStages {
		return fmt.Errorf("%w: %d stages", ErrCorrupt, len(c.stages))
	}
	for _, s := range c.stages {
		switch {
		case s.order < 1 || s.order > maxOrder:
			return fmt.Errorf("%w: stage order %d", ErrCorrupt, s.order)
		case s.kind == kindNLMS && !(s.mu > 0 && s.mu <= 1):
			return fmt.Errorf("%w: nlms step %v", ErrCorrupt, s.mu)
		case s.kind == kindSSLMS && (s.order < minSSLMSOrder || s.shift < 1 || s.shift > 30):
			return fmt.Errorf("%w: sslms order %d shift %d", ErrCorrupt, s.order, s.shift)
		case s.kind != kindNLMS && s.kind != kindSSLMS:
			return fmt.Errorf("%w: stage kind %d", ErrCorrupt, s.kind)
		}
	}
	return nil
}

// cascade is the shared encoder/decoder predictor state.
type cascade struct {
	cfg    config
	ch     int
	stages []stage
	mix    *mixer
	prev   []int32 // pre-emphasis state per channel
	models []*residual
	pred   []int32
	inp    []int32
}

func newCascade(cfg config, ch int) *cascade {
	k := &cascade{cfg: cfg, ch: ch, prev: make([]int32, ch), pred: make([]int32, len(cfg.stages)), inp: make([]int32, len(cfg.stages))}
	for _, s := range cfg.stages {
		switch s.kind {
		case kindNLMS:
			k.stages = append(k.stages, newNLMS(s.order, ch, s.mu))
		case kindSSLMS:
			k.stages = append(k.stages, newSSLMS(s.order, ch, s.shift))
		}
	}
	if cfg.mixMu > 0 && len(cfg.stages) > 0 {
		k.mix = newMixer(cfg.mixMu, ch, len(cfg.stages))
	}
	for range ch {
		k.models = append(k.models, newResidual())
	}
	return k
}

func (k *cascade) encodeSample(e *encoder, c int, x int32) {
	v := x
	if k.cfg.preEmph {
		v = x - (k.prev[c]*31)>>5
		k.prev[c] = x
	}
	sig := v
	for i, s := range k.stages {
		k.pred[i] = s.predict(c)
		k.inp[i] = v
		v -= k.pred[i]
	}
	if k.mix != nil {
		v = sig - k.mix.predict(c, k.pred)
		k.mix.update(c, k.pred, v)
	}
	k.models[c].encode(e, v)
	for i, s := range k.stages {
		s.update(c, k.inp[i]-k.pred[i])
		s.push(c, k.inp[i])
	}
}

func (k *cascade) decodeSample(d *decoder, c int) int32 {
	for i, s := range k.stages {
		k.pred[i] = s.predict(c)
	}
	v := k.models[c].decode(d)
	if k.mix != nil {
		r := v
		v += k.mix.predict(c, k.pred)
		k.mix.update(c, k.pred, r)
		for _, p := range k.pred { // back to the plain cascade residual
			v -= p
		}
	}
	for i := len(k.stages) - 1; i >= 0; i-- {
		e := v
		v += k.pred[i]
		k.stages[i].update(c, e)
		k.stages[i].push(c, v)
	}
	if k.cfg.preEmph {
		v += (k.prev[c] * 31) >> 5
		k.prev[c] = v
	}
	return v
}

// Encode compresses interleaved 16-bit PCM (channels samples per frame).
func Encode(pcm []int16, channels, sampleRate int, preset Preset) ([]byte, error) {
	if channels < 1 || channels > maxChannels {
		return nil, fmt.Errorf("lac: %d channels (1-%d supported)", channels, maxChannels)
	}
	if sampleRate < 1 || int64(sampleRate) > math.MaxUint32 {
		return nil, fmt.Errorf("lac: sample rate %d", sampleRate)
	}
	if len(pcm)%channels != 0 {
		return nil, fmt.Errorf("lac: %d samples isn't a whole number of %d-channel frames", len(pcm), channels)
	}
	if len(pcm) > maxSamples {
		return nil, fmt.Errorf("lac: %d samples (max %d)", len(pcm), maxSamples)
	}
	cfg := presetConfig(preset, channels)
	out := appendHeader(make([]byte, 0, len(pcm)/2), cfg, channels, sampleRate, len(pcm)/channels, pcmCRC(pcm))
	k := newCascade(cfg, channels)
	e := newEncoder(out)
	for i := 0; i < len(pcm); i += channels {
		for c := range channels {
			k.encodeSample(e, c, int32(pcm[i+c]))
		}
	}
	return e.finish(), nil
}

// Format describes a stream's audio.
type Format struct {
	Channels   int
	SampleRate int
	Frames     int
}

// Decode decompresses a stream from Encode, verifying its checksum.
func Decode(data []byte) ([]int16, Format, error) {
	h, err := parseHeader(data)
	if err != nil {
		return nil, Format{}, err
	}
	f := h.format
	total := f.Frames * f.Channels
	// Grow as we go rather than trusting the header's length up front.
	pcm := make([]int16, 0, min(total, 1<<20))
	k := newCascade(h.cfg, f.Channels)
	d := newDecoder(data[h.size:])
	for len(pcm) < total {
		for c := range f.Channels {
			pcm = append(pcm, int16(k.decodeSample(d, c)))
		}
		if d.overrun() {
			return nil, Format{}, fmt.Errorf("%w: truncated payload", ErrCorrupt)
		}
	}
	if pcmCRC(pcm) != h.crc {
		return nil, Format{}, fmt.Errorf("%w: checksum mismatch", ErrCorrupt)
	}
	return pcm, f, nil
}

// ReadFormat returns a stream's format without decoding it.
func ReadFormat(data []byte) (Format, error) {
	h, err := parseHeader(data)
	return h.format, err
}

func pcmCRC(pcm []int16) uint32 {
	var buf [4096]byte
	crc := uint32(0)
	for i := 0; i < len(pcm); {
		n := 0
		for ; n+2 <= len(buf) && i < len(pcm); i++ {
			binary.LittleEndian.PutUint16(buf[n:], uint16(pcm[i]))
			n += 2
		}
		crc = crc32.Update(crc, crc32.IEEETable, buf[:n])
	}
	return crc
}

func appendHeader(b []byte, cfg config, ch, rate, frames int, crc uint32) []byte {
	b = append(b, magic...)
	b = append(b, version, byte(ch))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate))
	b = binary.LittleEndian.AppendUint64(b, uint64(frames))
	b = binary.LittleEndian.AppendUint32(b, crc)
	var pe byte
	if cfg.preEmph {
		pe = 1
	}
	b = append(b, pe)
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(cfg.mixMu))
	b = append(b, byte(len(cfg.stages)))
	for _, s := range cfg.stages {
		b = append(b, byte(s.kind))
		b = binary.LittleEndian.AppendUint16(b, uint16(s.order))
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(s.mu))
		b = append(b, byte(s.shift))
	}
	return b
}

type header struct {
	format Format
	crc    uint32
	cfg    config
	size   int
}

const fixedHeader = 3 + 1 + 1 + 4 + 8 + 4 + 1 + 8 + 1
const stageHeader = 1 + 2 + 8 + 1

func parseHeader(b []byte) (header, error) {
	var h header
	if len(b) < fixedHeader || string(b[:3]) != magic {
		return h, fmt.Errorf("%w: not a lac stream", ErrCorrupt)
	}
	if b[3] != version {
		return h, fmt.Errorf("lac: unsupported version %d", b[3])
	}
	ch := int(b[4])
	rate := binary.LittleEndian.Uint32(b[5:])
	frames := binary.LittleEndian.Uint64(b[9:])
	h.crc = binary.LittleEndian.Uint32(b[17:])
	h.cfg.preEmph = b[21] == 1
	h.cfg.mixMu = math.Float64frombits(binary.LittleEndian.Uint64(b[22:]))
	n := int(b[30])
	if ch < 1 || ch > maxChannels || rate == 0 || frames > maxSamples/uint64(ch) || b[21] > 1 || n > maxStages {
		return h, fmt.Errorf("%w: bad header", ErrCorrupt)
	}
	p := fixedHeader
	if len(b) < p+n*stageHeader {
		return h, fmt.Errorf("%w: truncated header", ErrCorrupt)
	}
	for range n {
		h.cfg.stages = append(h.cfg.stages, stageSpec{
			kind:  stageKind(b[p]),
			order: int(binary.LittleEndian.Uint16(b[p+1:])),
			mu:    math.Float64frombits(binary.LittleEndian.Uint64(b[p+3:])),
			shift: uint(b[p+11]),
		})
		p += stageHeader
	}
	if err := h.cfg.validate(); err != nil {
		return h, err
	}
	h.format = Format{Channels: ch, SampleRate: int(rate), Frames: int(frames)}
	h.size = p
	return h, nil
}
