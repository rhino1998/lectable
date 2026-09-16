// Package wav extracts basic facts (duration) from PCM WAV file bytes, and
// (Decode/Encode) provides a small dependency-free in-memory 16-bit PCM WAV
// codec - the one shared codec every non-cgo Go package needing WAV bytes
// uses (internal/voicerefs for WSOLA re-speed, internal/audioworker's own
// worker process). Deliberately duplicated by audiocpp-go/audiocpp/wav.go
// rather than shared with it: that package's ReadWavFloat32/
// WriteWavFloat32 are file-path-based and live in a separate Go module
// this one shouldn't depend on just for WAV I/O.
package wav

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

func Duration(data []byte) (time.Duration, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, fmt.Errorf("not a RIFF/WAVE file")
	}

	var sampleRate uint32
	var numChannels uint16
	var bitsPerSample uint16
	var dataSize uint32
	haveFmt := false

	offset := 12
	for offset+8 <= len(data) {
		chunkID := string(data[offset : offset+4])
		chunkSize := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		body := offset + 8

		switch chunkID {
		case "fmt ":
			if body+16 > len(data) {
				return 0, fmt.Errorf("truncated fmt chunk")
			}
			numChannels = binary.LittleEndian.Uint16(data[body+2 : body+4])
			sampleRate = binary.LittleEndian.Uint32(data[body+4 : body+8])
			bitsPerSample = binary.LittleEndian.Uint16(data[body+14 : body+16])
			haveFmt = true
		case "data":
			dataSize = chunkSize
		}

		advance := int(chunkSize)
		if advance%2 == 1 {
			advance++ // chunks are word-aligned
		}
		offset = body + advance
	}

	if !haveFmt || sampleRate == 0 || numChannels == 0 || bitsPerSample == 0 {
		return 0, fmt.Errorf("missing or invalid fmt chunk")
	}
	bytesPerSecond := float64(sampleRate) * float64(numChannels) * float64(bitsPerSample) / 8
	if bytesPerSecond == 0 {
		return 0, fmt.Errorf("invalid fmt chunk")
	}
	seconds := float64(dataSize) / bytesPerSecond
	return time.Duration(seconds * float64(time.Second)), nil
}

// Clip is interleaved float32 PCM in [-1, 1], decoded from a 16-bit PCM WAV.
type Clip struct {
	Samples    []float32 // interleaved
	SampleRate int
	Channels   int
}

// Decode parses 16-bit PCM WAV bytes into interleaved float32 samples.
func Decode(data []byte) (*Clip, error) {
	r := bytes.NewReader(data)

	var riffHeader [12]byte
	if _, err := io.ReadFull(r, riffHeader[:]); err != nil {
		return nil, fmt.Errorf("reading RIFF header: %w", err)
	}
	if string(riffHeader[0:4]) != "RIFF" || string(riffHeader[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a RIFF/WAVE file")
	}

	var (
		haveFmt              bool
		channels, sampleRate int
		bitsPerSample        int
		pcm                  []byte
	)

	for {
		var chunkHeader [8]byte
		if _, err := io.ReadFull(r, chunkHeader[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, fmt.Errorf("reading chunk header: %w", err)
		}
		chunkID := string(chunkHeader[0:4])
		chunkSize := binary.LittleEndian.Uint32(chunkHeader[4:8])

		switch chunkID {
		case "fmt ":
			body := make([]byte, chunkSize)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, fmt.Errorf("reading fmt chunk: %w", err)
			}
			if len(body) < 16 {
				return nil, fmt.Errorf("fmt chunk too small")
			}
			audioFormat := binary.LittleEndian.Uint16(body[0:2])
			channels = int(binary.LittleEndian.Uint16(body[2:4]))
			sampleRate = int(binary.LittleEndian.Uint32(body[4:8]))
			bitsPerSample = int(binary.LittleEndian.Uint16(body[14:16]))
			if audioFormat != 1 && audioFormat != 0xFFFE {
				return nil, fmt.Errorf("unsupported WAV audio format %d, only PCM is supported", audioFormat)
			}
			haveFmt = true
		case "data":
			body := make([]byte, chunkSize)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, fmt.Errorf("reading data chunk: %w", err)
			}
			pcm = body
		default:
			if _, err := r.Seek(int64(chunkSize), io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("skipping %q chunk: %w", chunkID, err)
			}
		}
		if chunkSize%2 == 1 {
			if _, err := r.Seek(1, io.SeekCurrent); err != nil {
				break
			}
		}
	}

	if !haveFmt {
		return nil, fmt.Errorf("missing fmt chunk")
	}
	if pcm == nil {
		return nil, fmt.Errorf("missing data chunk")
	}
	if bitsPerSample != 16 {
		return nil, fmt.Errorf("only 16-bit PCM WAV is supported, got %d-bit", bitsPerSample)
	}

	n := len(pcm) / 2
	samples := make([]float32, n)
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(pcm[i*2 : i*2+2]))
		samples[i] = float32(v) / 32768.0
	}

	return &Clip{Samples: samples, SampleRate: sampleRate, Channels: channels}, nil
}

// Encode writes interleaved float32 samples in [-1, 1] out as 16-bit PCM
// WAV bytes, clipping anything outside that range.
func Encode(samples []float32, sampleRate, channels int) ([]byte, error) {
	if channels <= 0 {
		return nil, fmt.Errorf("channels must be positive, got %d", channels)
	}

	pcm := make([]byte, len(samples)*2)
	for i, s := range samples {
		if s > 1.0 {
			s = 1.0
		} else if s < -1.0 {
			s = -1.0
		}
		v := int16(s * 32767.0)
		binary.LittleEndian.PutUint16(pcm[i*2:i*2+2], uint16(v))
	}

	byteRate := sampleRate * channels * 2
	blockAlign := channels * 2
	dataSize := len(pcm)
	riffSize := 36 + dataSize

	buf := new(bytes.Buffer)
	buf.Grow(44 + dataSize)

	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(riffSize))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(header[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(header[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(header[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(header[34:36], 16) // bits per sample
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataSize))

	buf.Write(header)
	buf.Write(pcm)
	return buf.Bytes(), nil
}

// Concat joins a and b's own raw samples into one Clip, in order - used by
// jobs.Manager.generateCloneSplit to stitch a paragraph's two
// independently-generated halves back into one continuous clip after
// splitting it for generation (see that function's own doc comment). Both
// clips must share the same sample rate and channel count - true for
// every pair a caller here ever joins, since both come from the same
// clone call/model - returns an error rather than silently resampling or
// interleaving mismatched clips together.
func Concat(a, b *Clip) (*Clip, error) {
	if a.SampleRate != b.SampleRate {
		return nil, fmt.Errorf("sample rate mismatch: %d vs %d", a.SampleRate, b.SampleRate)
	}
	if a.Channels != b.Channels {
		return nil, fmt.Errorf("channel count mismatch: %d vs %d", a.Channels, b.Channels)
	}
	samples := make([]float32, 0, len(a.Samples)+len(b.Samples))
	samples = append(samples, a.Samples...)
	samples = append(samples, b.Samples...)
	return &Clip{Samples: samples, SampleRate: a.SampleRate, Channels: a.Channels}, nil
}
