// Package oggopus encodes WAV clips as Ogg Opus (RFC 7845) - the format
// every served narration, SFX and music clip is stored in (see
// audiopath.WriteClip). Pure Go: the Opus encoder is
// github.com/kazzmir/opus-go (libopus transpiled to Go), so cmd/server
// still links nothing beyond DuckDB. The Ogg framing is done here rather
// than with that module's own writer, which puts every 20ms packet on its
// own page (~11kbps of page headers - a third of a speech clip's bitrate);
// its wav2oggopus command also isn't a model to follow, since it never
// drains the encoder's lookahead (losing up to 6.5ms off the end).
package oggopus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"runtime"

	"github.com/kazzmir/opus-go/opus"

	"github.com/rhino1998/lectable/backend/internal/wav"
)

// Bitrates are per clip, VBR. Narration is mono speech from 24kHz models,
// where 48kbps is transparent; SFX and music are 44.1kHz stereo.
const (
	monoBitrate   = 48000
	stereoBitrate = 128000
)

// granuleRate is the rate Ogg Opus granule positions and pre-skip are
// always counted in, whatever the input rate (RFC 7845 section 4).
const granuleRate = 48000

// maxPageGranules bounds one Ogg page's audio to one second - opusenc's
// own default: page overhead stays negligible while seeking (which
// lands on page boundaries) stays fine-grained.
const maxPageGranules = granuleRate

// EncodeWAV re-encodes 16-bit PCM WAV bytes as Ogg Opus.
func EncodeWAV(data []byte) ([]byte, error) {
	clip, err := wav.Decode(data)
	if err != nil {
		return nil, err
	}
	return Encode(clip)
}

// Encode encodes clip as Ogg Opus. A clip at a rate Opus doesn't accept
// directly (anything but 8/12/16/24/48kHz, e.g. Stable Audio's 44.1kHz)
// is resampled to 48kHz first. Decoded length matches clip's exactly: the
// encoder's lookahead is signalled as pre-skip and the padding in the
// final frame is trimmed by the last page's granule position.
func Encode(clip *wav.Clip) ([]byte, error) {
	channels := clip.Channels
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("oggopus: %d channels unsupported (mono or stereo only)", channels)
	}
	rate := clip.SampleRate
	samples := clip.Samples
	if !opusRate(rate) {
		samples = resample(samples, channels, rate, granuleRate)
		rate = granuleRate
	}
	frames := len(samples) / channels
	scale := granuleRate / rate

	// Speech gets SILK/hybrid here, which needs the local opus-go fork
	// (go.mod replace): v1.4.0's libc shim zero-extends signed->unsigned
	// conversions, which corrupts every SILK frame - see
	// TestSpeechSurvivesSILK.
	enc, err := opus.NewEncoder(rate, channels, opus.ApplicationAudio)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	bitrate := monoBitrate
	if channels == 2 {
		bitrate = stereoBitrate
	}
	if err := enc.SetBitrate(bitrate); err != nil {
		return nil, err
	}
	if err := enc.SetVBR(true); err != nil {
		return nil, err
	}
	if err := enc.SetComplexity(10); err != nil {
		return nil, err
	}
	// Reported at the encoder's own rate (libopus's OPUS_GET_LOOKAHEAD
	// returns Fs-relative samples, despite the wrapper's doc comment).
	lookahead, err := enc.Lookahead()
	if err != nil {
		return nil, err
	}
	preSkip := uint64(lookahead * scale)
	total := uint64(frames * scale)

	var out bytes.Buffer
	w := &pageWriter{out: &out, serial: serialFor(samples)}
	w.writeHeaderPage(opusHead(channels, uint16(preSkip), clip.SampleRate), true)
	w.writeHeaderPage(opusTags(), false)

	// Feed lookahead samples of trailing silence past the end, so the
	// encoder's delay line flushes the clip's real last samples out.
	frameSize := rate / 50 // 20ms
	pcm := make([]int16, frameSize*channels)
	packet := make([]byte, 4000)
	// opus-go v1.4.0 hands the transpiled encoder uintptrs into these
	// buffers, so escape analysis would keep them on the goroutine stack -
	// and the encoder's own deep calls can grow (move) that stack mid-call,
	// leaving it writing packets into the stack's old copy (observed: whole
	// packets of zeros). Pinning forces them onto the heap, which never
	// moves. The local opus-go (go.mod replace) copies through its own
	// memory instead, so this is only belt and braces until that's
	// released upstream.
	var pinner runtime.Pinner
	defer pinner.Unpin()
	pinner.Pin(&pcm[0])
	pinner.Pin(&packet[0])
	encodedFrames := 0
	for encodedFrames < frames+lookahead {
		for i := range pcm {
			idx := encodedFrames*channels + i
			if idx < len(samples) {
				pcm[i] = toInt16(samples[idx])
			} else {
				pcm[i] = 0
			}
		}
		n, err := enc.Encode(pcm, frameSize, packet)
		if err != nil {
			return nil, err
		}
		encodedFrames += frameSize
		last := encodedFrames >= frames+lookahead
		// A page's granule counts every sample decodable through it,
		// pre-skip included; the last one instead end-trims to the
		// clip's real length (always <= what's decodable, since the
		// loop ran past frames+lookahead).
		granule := uint64(encodedFrames * scale)
		if last {
			granule = preSkip + total
		}
		w.addPacket(packet[:n], granule, last)
	}
	return out.Bytes(), nil
}

func opusRate(rate int) bool {
	switch rate {
	case 8000, 12000, 16000, 24000, 48000:
		return true
	}
	return false
}

func toInt16(v float32) int16 {
	s := v * 32767
	if s > 32767 {
		return 32767
	}
	if s < -32768 {
		return -32768
	}
	return int16(s)
}

// serialFor derives the stream serial from the audio itself: files are
// single-stream, so any value works, and a deterministic one keeps
// encoding reproducible.
func serialFor(samples []float32) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(samples) && i < 4096; i++ {
		h = (h ^ uint32(int32(samples[i]*32767))) * 16777619
	}
	return h
}

// opusHead is the identification header (RFC 7845 section 5.1).
// inputRate is informational only: players always decode at 48kHz.
func opusHead(channels int, preSkip uint16, inputRate int) []byte {
	b := make([]byte, 19)
	copy(b, "OpusHead")
	b[8] = 1 // version
	b[9] = byte(channels)
	binary.LittleEndian.PutUint16(b[10:], preSkip)
	binary.LittleEndian.PutUint32(b[12:], uint32(inputRate))
	// output gain 0, channel mapping family 0 (mono/stereo)
	return b
}

// opusTags is the comment header (RFC 7845 section 5.2), with no comments.
func opusTags() []byte {
	const vendor = "lectable"
	b := make([]byte, 8+4+len(vendor)+4)
	copy(b, "OpusTags")
	binary.LittleEndian.PutUint32(b[8:], uint32(len(vendor)))
	copy(b[12:], vendor)
	return b
}
