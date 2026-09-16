package audioworker

// Thin aliases onto the shared, dependency-free internal/wav codec (also
// used by internal/voicerefs for WSOLA re-speed) - kept as unexported
// local names here just so generate.go/design.go/align.go/server.go don't
// need to spell out the import at every call site.

import "github.com/rhino1998/lectable/backend/internal/wav"

type wavClip = wav.Clip

func decodeWav(data []byte) (*wavClip, error) {
	return wav.Decode(data)
}

func encodeWav(samples []float32, sampleRate, channels int) ([]byte, error) {
	return wav.Encode(samples, sampleRate, channels)
}
