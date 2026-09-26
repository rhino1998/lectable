package audiocpp

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
)

// Latents is a model's continuous pre-decoder representation - a VAE's
// latents - as carried by an ArtifactLatents artifact. The layout is the
// same for every family: float32, time-major (Data[frame*Dim + channel]), so
// frames can be sliced and concatenated without knowing the model.
//
// Latents come out of a "codec" session given audio (encode), or out of
// generation with the request option return_latents=true (Result.Latents).
// They go back in via Request.AddLatents: a "codec" session decodes them to
// audio, and a family that accepts latents in place of audio input (Stable
// Audio's init_audio/inpaint_audio) uses them without re-encoding.
type Latents struct {
	Frames int
	Dim    int
	Data   []float32
	// HopSamples audio samples at SampleRate per frame.
	HopSamples int
	SampleRate int
	Family     string
	// Meta holds family-specific extras, passed through untouched. Latents
	// returned by generation carry what a codec decode needs to reproduce
	// that generation's audio exactly (Stable Audio: decode_seed,
	// decode_rng_offset, chunked_decode, valid_frames, duration_samples).
	Meta map[string]string
}

// FrameSeconds is one frame's duration.
func (l *Latents) FrameSeconds() float64 {
	if l.SampleRate <= 0 {
		return 0
	}
	return float64(l.HopSamples) / float64(l.SampleRate)
}

// Seconds is the duration Frames cover.
func (l *Latents) Seconds() float64 { return float64(l.Frames) * l.FrameSeconds() }

// Slice returns a copy of frames [from, to). The copy has no Meta: the
// decode-reproduction meta describes the whole original window, not a slice
// of it.
func (l *Latents) Slice(from, to int) (*Latents, error) {
	if from < 0 || to > l.Frames || from > to {
		return nil, fmt.Errorf("audiocpp: latents slice [%d, %d) out of range [0, %d)", from, to, l.Frames)
	}
	return &Latents{
		Frames:     to - from,
		Dim:        l.Dim,
		Data:       append([]float32(nil), l.Data[from*l.Dim:to*l.Dim]...),
		HopSamples: l.HopSamples,
		SampleRate: l.SampleRate,
		Family:     l.Family,
	}, nil
}

// reserved latents meta keys; everything else lands in Latents.Meta.
var latentsReserved = map[string]bool{
	"frames": true, "dim": true, "dtype": true, "layout": true,
	"hop_samples": true, "sample_rate": true, "family": true,
}

// Latents decodes an ArtifactLatents artifact.
func (a Artifact) Latents() (*Latents, error) {
	if a.Kind != ArtifactLatents {
		return nil, fmt.Errorf("audiocpp: artifact %q is kind %d, not latents", a.ID, a.Kind)
	}
	if dt := a.Meta["dtype"]; dt != "" && dt != "f32" {
		return nil, fmt.Errorf("audiocpp: latents dtype %q (want f32)", dt)
	}
	if lay := a.Meta["layout"]; lay != "" && lay != "time_major" {
		return nil, fmt.Errorf("audiocpp: latents layout %q (want time_major)", lay)
	}
	num := func(key string, required bool) (int, error) {
		v, ok := a.Meta[key]
		if !ok {
			if required {
				return 0, fmt.Errorf("audiocpp: latents artifact missing meta %q", key)
			}
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("audiocpp: latents meta %s=%q is not a non-negative integer", key, v)
		}
		return n, nil
	}
	l := &Latents{Family: a.Meta["family"], Meta: map[string]string{}}
	var err error
	if l.Frames, err = num("frames", true); err != nil {
		return nil, err
	}
	if l.Dim, err = num("dim", true); err != nil {
		return nil, err
	}
	if l.HopSamples, err = num("hop_samples", false); err != nil {
		return nil, err
	}
	if l.SampleRate, err = num("sample_rate", false); err != nil {
		return nil, err
	}
	if l.Dim == 0 || len(a.Payload) != 4*l.Frames*l.Dim {
		return nil, fmt.Errorf("audiocpp: latents payload is %d bytes, want 4 x %d frames x %d dim", len(a.Payload), l.Frames, l.Dim)
	}
	l.Data = make([]float32, l.Frames*l.Dim)
	for i := range l.Data {
		l.Data[i] = math.Float32frombits(binary.LittleEndian.Uint32(a.Payload[4*i:]))
	}
	for k, v := range a.Meta {
		if !latentsReserved[k] {
			l.Meta[k] = v
		}
	}
	return l, nil
}

// Latents returns every latents artifact the task produced, in order (one
// per batch item for generation).
func (res *Result) Latents() ([]*Latents, error) {
	arts, err := res.Artifacts()
	if err != nil {
		return nil, err
	}
	var out []*Latents
	for _, a := range arts {
		if a.Kind != ArtifactLatents {
			continue
		}
		l, err := a.Latents()
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// Artifact encodes l as an ArtifactLatents artifact.
func (l *Latents) Artifact() (Artifact, error) {
	if l.Dim <= 0 || l.Frames < 0 || len(l.Data) != l.Frames*l.Dim {
		return Artifact{}, fmt.Errorf("audiocpp: latents has %d values, want %d frames x %d dim", len(l.Data), l.Frames, l.Dim)
	}
	payload := make([]byte, 4*len(l.Data))
	for i, v := range l.Data {
		binary.LittleEndian.PutUint32(payload[4*i:], math.Float32bits(v))
	}
	meta := map[string]string{
		"frames":      strconv.Itoa(l.Frames),
		"dim":         strconv.Itoa(l.Dim),
		"dtype":       "f32",
		"layout":      "time_major",
		"hop_samples": strconv.Itoa(l.HopSamples),
		"sample_rate": strconv.Itoa(l.SampleRate),
		"family":      l.Family,
	}
	for k, v := range l.Meta {
		if !latentsReserved[k] {
			meta[k] = v
		}
	}
	return Artifact{Kind: ArtifactLatents, ID: "latents", Payload: payload, Meta: meta}, nil
}

// AddLatents attaches l to the request as a latents input artifact.
func (r *Request) AddLatents(l *Latents) error {
	a, err := l.Artifact()
	if err != nil {
		return err
	}
	idx, err := r.AddArtifact(a.Kind, a.ID, a.Payload)
	if err != nil {
		return err
	}
	for k, v := range a.Meta {
		r.SetArtifactMeta(idx, k, v)
	}
	return r.Err()
}
