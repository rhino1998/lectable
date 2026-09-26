package lac

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// EncodeWAV compresses a 16-bit PCM WAV file. Only the audio is kept:
// DecodeWAV writes a canonical 44-byte header and drops any other chunks.
func EncodeWAV(wav []byte, preset Preset) ([]byte, error) {
	pcm, ch, rate, err := parseWAV(wav)
	if err != nil {
		return nil, err
	}
	return Encode(pcm, ch, rate, preset)
}

// DecodeWAV decompresses a stream into a canonical 16-bit PCM WAV file.
func DecodeWAV(data []byte) ([]byte, error) {
	pcm, f, err := Decode(data)
	if err != nil {
		return nil, err
	}
	n := 2 * len(pcm)
	b := make([]byte, 44+n)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+n))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(b[24:], uint32(f.SampleRate))
	binary.LittleEndian.PutUint32(b[28:], uint32(f.SampleRate*f.Channels*2))
	binary.LittleEndian.PutUint16(b[32:], uint16(f.Channels*2))
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(n))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[44+2*i:], uint16(v))
	}
	return b, nil
}

func parseWAV(b []byte) (pcm []int16, channels, rate int, err error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, 0, errors.New("lac: not a WAV file")
	}
	var bits int
	for p := 12; p+8 <= len(b); {
		id := string(b[p : p+4])
		n := int(binary.LittleEndian.Uint32(b[p+4:]))
		body := b[p+8:]
		if n > len(body) {
			n = len(body)
		}
		switch id {
		case "fmt ":
			if n < 16 {
				return nil, 0, 0, errors.New("lac: short fmt chunk")
			}
			tag := binary.LittleEndian.Uint16(body)
			channels = int(binary.LittleEndian.Uint16(body[2:]))
			rate = int(binary.LittleEndian.Uint32(body[4:]))
			bits = int(binary.LittleEndian.Uint16(body[14:]))
			if tag == 0xFFFE && n >= 26 { // WAVE_FORMAT_EXTENSIBLE: subformat GUID's first 2 bytes
				tag = binary.LittleEndian.Uint16(body[24:])
			}
			if tag != 1 || bits != 16 {
				return nil, 0, 0, fmt.Errorf("lac: WAV format %d/%d-bit (16-bit PCM only)", tag, bits)
			}
		case "data":
			if channels == 0 {
				return nil, 0, 0, errors.New("lac: data chunk before fmt")
			}
			n -= n % (2 * channels)
			pcm = make([]int16, n/2)
			for i := range pcm {
				pcm[i] = int16(binary.LittleEndian.Uint16(body[2*i:]))
			}
			return pcm, channels, rate, nil
		}
		p += 8 + n + n&1
	}
	return nil, 0, 0, errors.New("lac: no data chunk")
}
