package audiocpp

import (
	"encoding/binary"
	"fmt"
	"strconv"
)

// Codes is a model's discrete pre-decoder representation - a neural codec's
// codes - as carried by an ArtifactAcousticTokens artifact: int32,
// time-major (Data[frame*Codebooks + codebook]), the same conventions as
// Latents. The artifact ID says what the codes are: "codes" for generated or
// encoded audio, "reference_codes" for a cloning reference.
//
// Families with a discrete codec (Higgs) return Codes where a continuous one
// returns Latents: from a "codec" session given audio, or from generation
// with return_latents=true. Higgs also returns its cloning reference's codes
// with return_reference_codes=true, and takes them back via
// AddCodes(c, "reference_codes") to skip re-encoding the reference.
type Codes struct {
	Frames       int
	Codebooks    int
	CodebookSize int // 0 when unknown; otherwise every code is below it
	Data         []int32
	HopSamples   int
	SampleRate   int
	Family       string
	// Meta holds family extras, passed through untouched (Higgs:
	// chunk_frames, the per-text-chunk frame counts a decode needs).
	Meta map[string]string
}

// FrameSeconds is one frame's duration.
func (c *Codes) FrameSeconds() float64 {
	if c.SampleRate <= 0 {
		return 0
	}
	return float64(c.HopSamples) / float64(c.SampleRate)
}

// Seconds is the duration Frames cover.
func (c *Codes) Seconds() float64 { return float64(c.Frames) * c.FrameSeconds() }

var codesReserved = map[string]bool{
	"frames": true, "codebooks": true, "codebook_size": true, "dtype": true, "layout": true,
	"hop_samples": true, "sample_rate": true, "family": true,
}

// Codes decodes an ArtifactAcousticTokens artifact.
func (a Artifact) Codes() (*Codes, error) {
	if a.Kind != ArtifactAcousticTokens {
		return nil, fmt.Errorf("audiocpp: artifact %q is kind %d, not acoustic tokens", a.ID, a.Kind)
	}
	if dt := a.Meta["dtype"]; dt != "" && dt != "i32" {
		return nil, fmt.Errorf("audiocpp: codes dtype %q (want i32)", dt)
	}
	if lay := a.Meta["layout"]; lay != "" && lay != "time_major" {
		return nil, fmt.Errorf("audiocpp: codes layout %q (want time_major)", lay)
	}
	num := func(key string, required bool) (int, error) {
		v, ok := a.Meta[key]
		if !ok {
			if required {
				return 0, fmt.Errorf("audiocpp: codes artifact missing meta %q", key)
			}
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("audiocpp: codes meta %s=%q is not a non-negative integer", key, v)
		}
		return n, nil
	}
	c := &Codes{Family: a.Meta["family"], Meta: map[string]string{}}
	var err error
	for _, f := range []struct {
		key      string
		dst      *int
		required bool
	}{
		{"frames", &c.Frames, true}, {"codebooks", &c.Codebooks, true}, {"codebook_size", &c.CodebookSize, false},
		{"hop_samples", &c.HopSamples, false}, {"sample_rate", &c.SampleRate, false},
	} {
		if *f.dst, err = num(f.key, f.required); err != nil {
			return nil, err
		}
	}
	if c.Codebooks == 0 || len(a.Payload) != 4*c.Frames*c.Codebooks {
		return nil, fmt.Errorf("audiocpp: codes payload is %d bytes, want 4 x %d frames x %d codebooks", len(a.Payload), c.Frames, c.Codebooks)
	}
	c.Data = make([]int32, c.Frames*c.Codebooks)
	for i := range c.Data {
		c.Data[i] = int32(binary.LittleEndian.Uint32(a.Payload[4*i:]))
	}
	for k, v := range a.Meta {
		if !codesReserved[k] {
			c.Meta[k] = v
		}
	}
	return c, nil
}

// Artifact encodes c as an ArtifactAcousticTokens artifact with the given ID.
func (c *Codes) Artifact(id string) (Artifact, error) {
	if c.Codebooks <= 0 || c.Frames < 0 || len(c.Data) != c.Frames*c.Codebooks {
		return Artifact{}, fmt.Errorf("audiocpp: codes has %d values, want %d frames x %d codebooks", len(c.Data), c.Frames, c.Codebooks)
	}
	payload := make([]byte, 4*len(c.Data))
	for i, v := range c.Data {
		binary.LittleEndian.PutUint32(payload[4*i:], uint32(v))
	}
	meta := map[string]string{
		"frames":        strconv.Itoa(c.Frames),
		"codebooks":     strconv.Itoa(c.Codebooks),
		"codebook_size": strconv.Itoa(c.CodebookSize),
		"dtype":         "i32",
		"layout":        "time_major",
		"hop_samples":   strconv.Itoa(c.HopSamples),
		"sample_rate":   strconv.Itoa(c.SampleRate),
		"family":        c.Family,
	}
	for k, v := range c.Meta {
		if !codesReserved[k] {
			meta[k] = v
		}
	}
	return Artifact{Kind: ArtifactAcousticTokens, ID: id, Payload: payload, Meta: meta}, nil
}

// Codes returns the task's acoustic-token artifact with this ID, or nil if
// there is none.
func (res *Result) Codes(id string) (*Codes, error) {
	arts, err := res.Artifacts()
	if err != nil {
		return nil, err
	}
	for _, a := range arts {
		if a.Kind == ArtifactAcousticTokens && a.ID == id {
			return a.Codes()
		}
	}
	return nil, nil
}

// AddCodes attaches c to the request as an acoustic-token artifact with this
// ID ("codes" to decode, "reference_codes" as a cloning reference).
func (r *Request) AddCodes(c *Codes, id string) error {
	a, err := c.Artifact(id)
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
