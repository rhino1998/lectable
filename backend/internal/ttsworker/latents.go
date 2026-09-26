package ttsworker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// Audio is a generation's WAV plus, where the model has one, its decoder
// input (Higgs codes, PocketTTS/Stable Audio latents) - decoding Latents
// reproduces WAV exactly on the worker's backend. Code that changes WAV's
// samples must drop Latents: they describe WAV as generated, nothing else.
type Audio struct {
	WAV     []byte
	Latents *ttsproto.Latents
}

// GenerateChunkedAudio is GenerateChunked returning the clip's latents too,
// and the path that uses the reference-code cache (Config.RefCodesDir): a
// Higgs reference's encoding is cached by content (clone model, transcript,
// reference bytes) and sent back instead of being re-encoded. The
// reference audio always goes too - the worker falls back to it when the
// cached codes came from a different checkpoint, and replies with fresh
// ones, which replace the stale entry.
func (m *Manager) GenerateChunkedAudio(ctx context.Context, text, cloneModel string, refAudio []byte, refText, language, instruct string, textChunkSize int) (Audio, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	cachePath := m.refCodesPath(cloneModel, refText, refAudio)
	var cached *ttsproto.Latents
	if cachePath != "" {
		if l, err := latents.ReadFile(cachePath); err == nil {
			cached = l
		}
	}
	start := time.Now()
	var resp ttsproto.GenerateResponse
	err := m.do(ctx, http.MethodPost, "/generate", ttsproto.GenerateRequest{
		Text:           text,
		CloneModel:     cloneModel,
		RefAudioBase64: base64.StdEncoding.EncodeToString(refAudio),
		RefText:        refText,
		Language:       language,
		Instruct:       instruct,
		TextChunkSize:  textChunkSize,
		ReturnLatents:  true,
		ReferenceCodes: cached,
	}, &resp)
	generateDuration.WithLabelValues(cloneModel, outcome(err)).Observe(time.Since(start).Seconds())
	if err != nil {
		return Audio{}, err
	}
	if d, derr := wav.Duration(resp.WAV); derr == nil {
		generatedAudio.WithLabelValues(cloneModel).Add(d.Seconds())
	}
	if resp.ReferenceCodes != nil && cachePath != "" {
		err := os.MkdirAll(filepath.Dir(cachePath), 0o755)
		if err == nil {
			err = latents.WriteFile(cachePath, resp.ReferenceCodes, "")
		}
		if err != nil {
			log.Printf("ttsworker: cache reference codes: %v", err)
		}
	}
	return Audio{WAV: resp.WAV, Latents: resp.Latents}, nil
}

// refCodesPath is the cache file for one (clone model, transcript,
// reference clip), or "" with the cache disabled.
func (m *Manager) refCodesPath(cloneModel, refText string, refAudio []byte) string {
	if m.cfg.RefCodesDir == "" || len(refAudio) == 0 {
		return ""
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(cloneModel), []byte(refText), refAudio} {
		var n [8]byte
		for i, l := 0, len(part); i < 8; i, l = i+1, l>>8 {
			n[i] = byte(l)
		}
		h.Write(n[:])
		h.Write(part)
	}
	return filepath.Join(m.cfg.RefCodesDir, hex.EncodeToString(h.Sum(nil))+latents.Ext)
}

// StableAudioMediumAudio is StableAudioMedium with latents: req's
// ReturnLatents is set, and the reply's latents (the whole generated
// window) come back with the WAV. req.InitLatents may stand in for
// req.InitAudioBase64.
func (m *Manager) StableAudioMediumAudio(ctx context.Context, req ttsproto.StableAudioRequest) (Audio, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	req.ReturnLatents = true
	var resp ttsproto.StableAudioResponse
	if err := m.do(ctx, http.MethodPost, "/stable-audio-medium", req, &resp); err != nil {
		return Audio{}, err
	}
	return Audio{WAV: resp.WAV, Latents: resp.Latents}, nil
}

// CodecEncode encodes a WAV clip with family's standalone codec
// ("stable_audio_medium") - see audioworker.Worker.CodecEncode.
func (m *Manager) CodecEncode(ctx context.Context, family string, wavBytes []byte) (*ttsproto.Latents, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	var out ttsproto.Latents
	err := m.do(ctx, http.MethodPost, "/codec-encode", ttsproto.CodecEncodeRequest{
		Family:      family,
		AudioBase64: base64.StdEncoding.EncodeToString(wavBytes),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// StableAudioMediumLatents is StableAudioMediumAudio's result unpacked -
// the shape internal/musicgen's LatentsBackend takes.
func (m *Manager) StableAudioMediumLatents(ctx context.Context, req ttsproto.StableAudioRequest) ([]byte, *ttsproto.Latents, error) {
	a, err := m.StableAudioMediumAudio(ctx, req)
	return a.WAV, a.Latents, err
}

// CodecDecode decodes latents with family's standalone codec back to WAV -
// see audioworker.Worker.CodecDecode.
func (m *Manager) CodecDecode(ctx context.Context, family string, lat *ttsproto.Latents) ([]byte, error) {
	m.restartGate.RLock()
	defer m.restartGate.RUnlock()
	jobID := m.beginJob()
	defer m.endJob(jobID)

	var out []byte
	err := m.do(ctx, http.MethodPost, "/codec-decode", ttsproto.CodecDecodeRequest{Family: family, Latents: lat}, &out)
	return out, err
}
