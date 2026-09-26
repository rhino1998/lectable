// Package latents stores and manipulates a model's pre-decoder state
// (ttsproto.Latents: continuous latents or discrete codec codes) outside
// the worker: sidecar files next to the audio they encode, slicing and
// concatenating along time.
//
// Sidecar format (".lat"): magic "LLAT", version byte, a uvarint-length
// JSON header (fileHeader), then the payload in the header's storage dtype,
// time-major:
//   - "f32"/"f16": latents, little-endian.
//   - "i32"/"i16": codes, little-endian.
//   - "u<N>" (1-31): codes bit-packed at N bits each, LSB-first - what
//     codes default to, N being the codebook size's bit width (10 for
//     Higgs). Codes are near-random, so this beats zstd/xz on i16.
//
// Everything but f16 round-trips ttsproto.Latents exactly; f16 is lossy
// (~2e-4 relative), for conditioning-only uses like music seeds.
package latents

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// Kind values for ttsproto.Latents.Kind.
const (
	KindLatents = "latents"
	KindCodes   = "codes"
)

// Ext is a sidecar's file extension.
const Ext = ".lat"

const (
	magic   = "LLAT"
	version = 1
)

// Sidecar is the latents file kept beside audio file path (its extension
// replaced by Ext).
func Sidecar(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + Ext
}

type fileHeader struct {
	Kind         string            `json:"kind"`
	Frames       int               `json:"frames"`
	Dim          int               `json:"dim"`
	CodebookSize int               `json:"codebookSize,omitempty"`
	HopSamples   int               `json:"hopSamples"`
	SampleRate   int               `json:"sampleRate"`
	Family       string            `json:"family"`
	DType        string            `json:"dtype"`
	Meta         map[string]string `json:"meta,omitempty"`
}

// Validate checks l's shape against its payload.
func Validate(l *ttsproto.Latents) error {
	if l == nil {
		return errors.New("latents: nil")
	}
	if l.Kind != KindLatents && l.Kind != KindCodes {
		return fmt.Errorf("latents: unknown kind %q", l.Kind)
	}
	if l.Frames < 0 || l.Dim <= 0 || len(l.Data) != 4*l.Frames*l.Dim {
		return fmt.Errorf("latents: %d bytes for %d frames x %d", len(l.Data), l.Frames, l.Dim)
	}
	return nil
}

// Encode serializes l as a sidecar in dtype ("" picks the exact one:
// f32 for latents, i16 for codes that fit, else i32).
func Encode(l *ttsproto.Latents, dtype string) ([]byte, error) {
	if err := Validate(l); err != nil {
		return nil, err
	}
	if dtype == "" {
		dtype = "f32"
		if l.Kind == KindCodes {
			dtype = "i32"
			if l.CodebookSize > 1 && l.CodebookSize <= 1<<31 {
				dtype = "u" + strconv.Itoa(bits.Len(uint(l.CodebookSize-1)))
			}
		}
	}
	n := l.Frames * l.Dim
	var payload []byte
	switch {
	case l.Kind == KindLatents && dtype == "f32", l.Kind == KindCodes && dtype == "i32":
		payload = l.Data
	case l.Kind == KindLatents && dtype == "f16":
		payload = make([]byte, 2*n)
		for i := range n {
			f := math.Float32frombits(binary.LittleEndian.Uint32(l.Data[4*i:]))
			binary.LittleEndian.PutUint16(payload[2*i:], toFloat16(f))
		}
	case l.Kind == KindCodes && dtype == "i16":
		payload = make([]byte, 2*n)
		for i := range n {
			v := int32(binary.LittleEndian.Uint32(l.Data[4*i:]))
			if v < math.MinInt16 || v > math.MaxInt16 {
				return nil, fmt.Errorf("latents: code %d doesn't fit i16", v)
			}
			binary.LittleEndian.PutUint16(payload[2*i:], uint16(int16(v)))
		}
	case l.Kind == KindCodes && packedBits(dtype) > 0:
		width := packedBits(dtype)
		codes := make([]uint32, n)
		for i := range n {
			v := binary.LittleEndian.Uint32(l.Data[4*i:])
			if v>>width != 0 {
				return nil, fmt.Errorf("latents: code %d doesn't fit %d bits", int32(v), width)
			}
			codes[i] = v
		}
		payload = packBits(codes, width)
	default:
		return nil, fmt.Errorf("latents: can't store %s as %s", l.Kind, dtype)
	}
	hdr, err := json.Marshal(fileHeader{
		Kind: l.Kind, Frames: l.Frames, Dim: l.Dim, CodebookSize: l.CodebookSize,
		HopSamples: l.HopSamples, SampleRate: l.SampleRate, Family: l.Family,
		DType: dtype, Meta: l.Meta,
	})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString(magic)
	buf.WriteByte(version)
	buf.Write(binary.AppendUvarint(nil, uint64(len(hdr))))
	buf.Write(hdr)
	buf.Write(payload)
	return buf.Bytes(), nil
}

// Decode parses a sidecar back into f32/i32 ttsproto.Latents.
func Decode(data []byte) (*ttsproto.Latents, error) {
	if len(data) < len(magic)+1 || string(data[:len(magic)]) != magic {
		return nil, errors.New("latents: not a latents file")
	}
	if data[len(magic)] != version {
		return nil, fmt.Errorf("latents: unsupported version %d", data[len(magic)])
	}
	rest := data[len(magic)+1:]
	hlen, n := binary.Uvarint(rest)
	if n <= 0 || uint64(len(rest)-n) < hlen {
		return nil, errors.New("latents: truncated header")
	}
	var h fileHeader
	if err := json.Unmarshal(rest[n:n+int(hlen)], &h); err != nil {
		return nil, fmt.Errorf("latents: header: %w", err)
	}
	payload := rest[n+int(hlen):]
	count := h.Frames * h.Dim
	if h.Frames < 0 || h.Dim <= 0 {
		return nil, errors.New("latents: bad shape")
	}
	l := &ttsproto.Latents{
		Kind: h.Kind, Frames: h.Frames, Dim: h.Dim, CodebookSize: h.CodebookSize,
		HopSamples: h.HopSamples, SampleRate: h.SampleRate, Family: h.Family, Meta: h.Meta,
		Data: make([]byte, 4*count),
	}
	if b := packedBits(h.DType); b > 0 {
		if len(payload) != (count*b+7)/8 {
			return nil, fmt.Errorf("latents: %d payload bytes for %d x %s", len(payload), count, h.DType)
		}
		for i, v := range unpackBits(payload, b, count) {
			binary.LittleEndian.PutUint32(l.Data[4*i:], v)
		}
		return l, Validate(l)
	}
	width := map[string]int{"f32": 4, "i32": 4, "f16": 2, "i16": 2}[h.DType]
	if width == 0 || len(payload) != width*count {
		return nil, fmt.Errorf("latents: %d payload bytes for %d x %s", len(payload), count, h.DType)
	}
	switch h.DType {
	case "f32", "i32":
		copy(l.Data, payload)
	case "f16":
		for i := range count {
			f := fromFloat16(binary.LittleEndian.Uint16(payload[2*i:]))
			binary.LittleEndian.PutUint32(l.Data[4*i:], math.Float32bits(f))
		}
	case "i16":
		for i := range count {
			binary.LittleEndian.PutUint32(l.Data[4*i:], uint32(int32(int16(binary.LittleEndian.Uint16(payload[2*i:])))))
		}
	}
	return l, Validate(l)
}

// WriteFile stores l at path (atomically, via a temp file and rename).
func WriteFile(path string, l *ttsproto.Latents, dtype string) error {
	data, err := Encode(l, dtype)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes 0600; match the rest of the data dir.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadFile loads a sidecar.
func ReadFile(path string) (*ttsproto.Latents, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

// Slice copies frames [from, to). The copy drops Meta, which describes the
// whole original window (decode_*, chunk_frames), not a slice of it.
func Slice(l *ttsproto.Latents, from, to int) (*ttsproto.Latents, error) {
	if err := Validate(l); err != nil {
		return nil, err
	}
	if from < 0 || to > l.Frames || from > to {
		return nil, fmt.Errorf("latents: slice [%d, %d) out of [0, %d)", from, to, l.Frames)
	}
	out := *l
	out.Frames = to - from
	out.Data = append([]byte(nil), l.Data[4*from*l.Dim:4*to*l.Dim]...)
	out.Meta = nil
	return &out, nil
}

// ChunkFrames is l's per-chunk frame counts (Meta "chunk_frames"), or one
// chunk of every frame.
func ChunkFrames(l *ttsproto.Latents) ([]int, error) {
	list := l.Meta["chunk_frames"]
	if list == "" {
		return []int{l.Frames}, nil
	}
	var out []int
	total := 0
	for part := range strings.SplitSeq(list, ",") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("latents: bad chunk_frames %q", list)
		}
		out = append(out, n)
		total += n
	}
	if total != l.Frames {
		return nil, fmt.Errorf("latents: chunk_frames %q doesn't sum to %d frames", list, l.Frames)
	}
	return out, nil
}

// Concat joins independently-decoded pieces in order (e.g. a paragraph
// generated in two halves whose audio was concatenated): each piece's
// chunks stay separate chunks, so decoding the result reproduces the
// concatenated audio. Pieces must share kind, family, shape and rate, and
// carry no other meta that a join would falsify.
func Concat(pieces ...*ttsproto.Latents) (*ttsproto.Latents, error) {
	if len(pieces) == 0 {
		return nil, errors.New("latents: nothing to concatenate")
	}
	first := pieces[0]
	out := &ttsproto.Latents{
		Kind: first.Kind, Dim: first.Dim, CodebookSize: first.CodebookSize,
		HopSamples: first.HopSamples, SampleRate: first.SampleRate, Family: first.Family,
		Meta: map[string]string{},
	}
	var chunks []string
	for _, p := range pieces {
		if err := Validate(p); err != nil {
			return nil, err
		}
		if p.Kind != out.Kind || p.Dim != out.Dim || p.Family != out.Family || p.HopSamples != out.HopSamples || p.SampleRate != out.SampleRate {
			return nil, errors.New("latents: pieces don't match")
		}
		for k, v := range p.Meta {
			switch k {
			case "chunk_frames":
			case "normalized", "codec_model":
				if prev, ok := out.Meta[k]; ok && prev != v {
					return nil, fmt.Errorf("latents: pieces disagree on %s", k)
				}
				out.Meta[k] = v
			default:
				return nil, fmt.Errorf("latents: can't concatenate pieces carrying %q meta", k)
			}
		}
		cf, err := ChunkFrames(p)
		if err != nil {
			return nil, err
		}
		for _, n := range cf {
			chunks = append(chunks, strconv.Itoa(n))
		}
		out.Frames += p.Frames
		out.Data = append(out.Data, p.Data...)
	}
	out.Meta["chunk_frames"] = strings.Join(chunks, ",")
	return out, nil
}

// toFloat16 rounds f to the nearest IEEE half (ties to even).
func toFloat16(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	exp := int((b >> 23) & 0xff)
	mant := b & 0x7fffff
	switch {
	case exp == 0xff: // inf/nan
		if mant != 0 {
			return sign | 0x7e00
		}
		return sign | 0x7c00
	case exp-127+15 >= 0x1f: // overflow
		return sign | 0x7c00
	case exp-127+15 <= 0: // subnormal or zero
		shift := uint(14 - (exp - 127 + 15))
		if shift > 24 {
			return sign
		}
		m := (mant | 0x800000) >> shift
		rem := (mant | 0x800000) & (1<<shift - 1)
		half := uint32(1) << (shift - 1)
		if rem > half || (rem == half && m&1 == 1) {
			m++
		}
		return sign | uint16(m)
	}
	h := uint32(exp-127+15)<<10 | mant>>13
	rem := mant & 0x1fff
	if rem > 0x1000 || (rem == 0x1000 && h&1 == 1) {
		h++ // may carry into the exponent, which is still correct
	}
	return sign | uint16(h)
}

func fromFloat16(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x3ff)
	switch exp {
	case 0x1f:
		return math.Float32frombits(sign | 0x7f800000 | mant<<13)
	case 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		f := float32(mant) / 1024 / 16384 // 2^-14 * mant/1024
		if sign != 0 {
			return -f
		}
		return f
	}
	return math.Float32frombits(sign | (exp+127-15)<<23 | mant<<13)
}

// packedBits is N for a "u<N>" dtype, else 0.
func packedBits(dtype string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(dtype, "u"))
	if !strings.HasPrefix(dtype, "u") || err != nil || n < 1 || n > 31 {
		return 0
	}
	return n
}

// packBits stores each value's low width bits back to back, LSB-first.
func packBits(values []uint32, width int) []byte {
	out := make([]byte, (len(values)*width+7)/8)
	var acc uint64
	nbits, pos := 0, 0
	for _, v := range values {
		acc |= uint64(v) << nbits
		nbits += width
		for nbits >= 8 {
			out[pos] = byte(acc)
			pos++
			acc >>= 8
			nbits -= 8
		}
	}
	if nbits > 0 {
		out[pos] = byte(acc)
	}
	return out
}

func unpackBits(data []byte, width, count int) []uint32 {
	out := make([]uint32, count)
	var acc uint64
	nbits, pos := 0, 0
	mask := uint64(1)<<width - 1
	for i := range out {
		for nbits < width {
			acc |= uint64(data[pos]) << nbits
			pos++
			nbits += 8
		}
		out[i] = uint32(acc & mask)
		acc >>= width
		nbits -= width
	}
	return out
}

// CodecFamily is the worker codec (ttsworker.Manager.CodecDecode's family)
// that decodes l: its model family, with Stable Audio's naming the Medium
// checkpoint the backend renders with.
func CodecFamily(l *ttsproto.Latents) string {
	if l.Family == "stable_audio" {
		return "stable_audio_medium"
	}
	return l.Family
}
