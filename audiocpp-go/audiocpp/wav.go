package audiocpp

// Tiny dependency-free WAV reader/writer, for feeding audio.cpp requests
// from a file. audio.cpp's own C API takes float PCM and has no opinion on
// containers, so this is a convenience, not part of the ABI surface -- it
// mirrors bindings/python/audiocpp/wav.py.

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// WavClip is interleaved float32 PCM in [-1, 1] read from a WAV file.
type WavClip struct {
	Samples    []float32 // interleaved
	SampleRate int
	Channels   int
}

// Frames returns the per-channel sample count.
func (c WavClip) Frames() int {
	if c.Channels == 0 {
		return 0
	}
	return len(c.Samples) / c.Channels
}

// ReadWavFloat32 reads a 16-bit PCM WAV file into interleaved float32
// samples in [-1, 1].
func ReadWavFloat32(path string) (*WavClip, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var riffHeader [12]byte
	if _, err := io.ReadFull(f, riffHeader[:]); err != nil {
		return nil, fmt.Errorf("%s: reading RIFF header: %w", path, err)
	}
	if string(riffHeader[0:4]) != "RIFF" || string(riffHeader[8:12]) != "WAVE" {
		return nil, fmt.Errorf("%s: not a RIFF/WAVE file", path)
	}

	var (
		haveFmt              bool
		channels, sampleRate int
		bitsPerSample        int
		pcm                  []byte
	)

	for {
		var chunkHeader [8]byte
		if _, err := io.ReadFull(f, chunkHeader[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, fmt.Errorf("%s: reading chunk header: %w", path, err)
		}
		chunkID := string(chunkHeader[0:4])
		chunkSize := binary.LittleEndian.Uint32(chunkHeader[4:8])

		switch chunkID {
		case "fmt ":
			body := make([]byte, chunkSize)
			if _, err := io.ReadFull(f, body); err != nil {
				return nil, fmt.Errorf("%s: reading fmt chunk: %w", path, err)
			}
			if len(body) < 16 {
				return nil, fmt.Errorf("%s: fmt chunk too small", path)
			}
			audioFormat := binary.LittleEndian.Uint16(body[0:2])
			channels = int(binary.LittleEndian.Uint16(body[2:4]))
			sampleRate = int(binary.LittleEndian.Uint32(body[4:8]))
			bitsPerSample = int(binary.LittleEndian.Uint16(body[14:16]))
			// 1 = PCM, 0xFFFE = WAVE_FORMAT_EXTENSIBLE (still PCM here since
			// we only accept 16-bit integer samples below).
			if audioFormat != 1 && audioFormat != 0xFFFE {
				return nil, fmt.Errorf("%s: unsupported WAV audio format %d, only PCM is supported", path, audioFormat)
			}
			haveFmt = true
		case "data":
			body := make([]byte, chunkSize)
			if _, err := io.ReadFull(f, body); err != nil {
				return nil, fmt.Errorf("%s: reading data chunk: %w", path, err)
			}
			pcm = body
		default:
			if _, err := f.Seek(int64(chunkSize), io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("%s: skipping %q chunk: %w", path, chunkID, err)
			}
		}
		if chunkSize%2 == 1 {
			// Chunks are word-aligned; a odd-sized chunk has one pad byte.
			if _, err := f.Seek(1, io.SeekCurrent); err != nil {
				break
			}
		}
	}

	if !haveFmt {
		return nil, fmt.Errorf("%s: missing fmt chunk", path)
	}
	if pcm == nil {
		return nil, fmt.Errorf("%s: missing data chunk", path)
	}
	if bitsPerSample != 16 {
		return nil, fmt.Errorf("%s: only 16-bit PCM WAV is supported, got %d-bit", path, bitsPerSample)
	}

	n := len(pcm) / 2
	samples := make([]float32, n)
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(pcm[i*2 : i*2+2]))
		samples[i] = float32(v) / 32768.0
	}

	return &WavClip{Samples: samples, SampleRate: sampleRate, Channels: channels}, nil
}

// WriteWavFloat32 writes interleaved float32 samples in [-1, 1] out as
// 16-bit PCM WAV, clipping anything outside that range.
func WriteWavFloat32(path string, samples []float32, sampleRate, channels int) error {
	if channels <= 0 {
		return fmt.Errorf("%s: channels must be positive, got %d", path, channels)
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

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

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

	if _, err := f.Write(header); err != nil {
		return err
	}
	if _, err := f.Write(pcm); err != nil {
		return err
	}
	return nil
}
