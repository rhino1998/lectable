package audioworker

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// Conversions between audiocpp's latents/codes artifacts and the wire type.
// Every ttsproto.Latents this worker hands out is stamped with Meta
// "codec_model" (modelIdentity of the checkpoint that produced it), so a
// caller's cached copy can be checked against whatever model is loaded
// later - see GenerateRequest.ReferenceCodes.

func wireFromLatents(l *audiocpp.Latents, codecModel string) *ttsproto.Latents {
	data := make([]byte, 4*len(l.Data))
	for i, v := range l.Data {
		binary.LittleEndian.PutUint32(data[4*i:], math.Float32bits(v))
	}
	return &ttsproto.Latents{
		Kind: latents.KindLatents, Frames: l.Frames, Dim: l.Dim,
		HopSamples: l.HopSamples, SampleRate: l.SampleRate, Family: l.Family,
		Data: data, Meta: withCodecModel(l.Meta, codecModel),
	}
}

func wireFromCodes(c *audiocpp.Codes, codecModel string) *ttsproto.Latents {
	data := make([]byte, 4*len(c.Data))
	for i, v := range c.Data {
		binary.LittleEndian.PutUint32(data[4*i:], uint32(v))
	}
	return &ttsproto.Latents{
		Kind: latents.KindCodes, Frames: c.Frames, Dim: c.Codebooks, CodebookSize: c.CodebookSize,
		HopSamples: c.HopSamples, SampleRate: c.SampleRate, Family: c.Family,
		Data: data, Meta: withCodecModel(c.Meta, codecModel),
	}
}

func withCodecModel(meta map[string]string, codecModel string) map[string]string {
	out := make(map[string]string, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	if codecModel != "" {
		out["codec_model"] = codecModel
	}
	return out
}

// latentsFromWire is the inverse for continuous latents. codec_model is
// this backend's bookkeeping, not audio.cpp's, so it isn't passed down.
func latentsFromWire(l *ttsproto.Latents) (*audiocpp.Latents, error) {
	if err := latents.Validate(l); err != nil {
		return nil, err
	}
	if l.Kind != latents.KindLatents {
		return nil, fmt.Errorf("want continuous latents, got %q", l.Kind)
	}
	out := &audiocpp.Latents{
		Frames: l.Frames, Dim: l.Dim, HopSamples: l.HopSamples, SampleRate: l.SampleRate,
		Family: l.Family, Data: make([]float32, l.Frames*l.Dim), Meta: map[string]string{},
	}
	for i := range out.Data {
		out.Data[i] = math.Float32frombits(binary.LittleEndian.Uint32(l.Data[4*i:]))
	}
	for k, v := range l.Meta {
		if k != "codec_model" {
			out.Meta[k] = v
		}
	}
	return out, nil
}

func codesFromWire(l *ttsproto.Latents) (*audiocpp.Codes, error) {
	if err := latents.Validate(l); err != nil {
		return nil, err
	}
	if l.Kind != latents.KindCodes {
		return nil, fmt.Errorf("want codes, got %q", l.Kind)
	}
	out := &audiocpp.Codes{
		Frames: l.Frames, Codebooks: l.Dim, CodebookSize: l.CodebookSize, HopSamples: l.HopSamples,
		SampleRate: l.SampleRate, Family: l.Family, Data: make([]int32, l.Frames*l.Dim), Meta: map[string]string{},
	}
	for i := range out.Data {
		out.Data[i] = int32(binary.LittleEndian.Uint32(l.Data[4*i:]))
	}
	for k, v := range l.Meta {
		if k != "codec_model" {
			out.Meta[k] = v
		}
	}
	return out, nil
}

var modelIdentities sync.Map // path -> string

// modelIdentity names the checkpoint at path well enough to notice it
// being replaced: "<base name>:<size>" - the same on every server holding
// the same file, which a transferred book's latents rely on. Cached per
// process - a checkpoint swapped under a running worker isn't picked up
// until the worker restarts anyway.
func modelIdentity(path string) string {
	if v, ok := modelIdentities.Load(path); ok {
		return v.(string)
	}
	id := path
	if st, err := os.Stat(path); err == nil {
		id = st.Name() + ":" + strconv.FormatInt(st.Size(), 10)
	}
	modelIdentities.Store(path, id)
	return id
}

// sameModel compares two modelIdentity stamps by name and size only -
// stamps written by the first version of this code also carried a
// (machine-specific) mtime field, ignored here so they stay valid.
func sameModel(a, b string) bool {
	trim := func(s string) string {
		if parts := strings.SplitN(s, ":", 3); len(parts) == 3 {
			return parts[0] + ":" + parts[1]
		}
		return s
	}
	return a != "" && trim(a) == trim(b)
}

// familyHasLatents reports whether a clone family returns its decoder
// input (audio.cpp's return_latents): Higgs codes, PocketTTS latents.
func familyHasLatents(family string) bool {
	return family == "higgs_audio_tts" || family == "pocket_tts"
}
