package audiocpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -laudiocpp
#include <stdlib.h>
#include <audiocpp.h>
*/
import "C"

import "unsafe"

// AudioBuffer is interleaved float32 PCM copied out of a Result.
type AudioBuffer struct {
	Samples    []float32 // interleaved
	SampleRate int
	Channels   int
}

// Frames returns the per-channel sample count.
func (a AudioBuffer) Frames() int {
	if a.Channels == 0 {
		return 0
	}
	return len(a.Samples) / a.Channels
}

// TextResult is text output copied out of a Result.
type TextResult struct {
	Text     string
	Language string
}

// Segment is one entry from Result.Segments.
type Segment struct {
	StartSample int64
	EndSample   int64
	Confidence  float32
	Text        string
}

// SpeakerTurn is one entry from Result.SpeakerTurns.
type SpeakerTurn struct {
	StartSample int64
	EndSample   int64
	SpeakerID   string
	Confidence  float32
	Text        string
}

// Word is one entry from Result.Words.
type Word struct {
	Text        string
	StartSample int64
	EndSample   int64
	Confidence  float32
}

// NamedAudio is one named audio stream from Result.NamedAudioList, for
// families that emit more than one (source separation, multi-speaker TTS).
type NamedAudio struct {
	ID         string
	Samples    []float32
	SampleRate int
	Channels   int
}

// Artifact is one opaque payload from Result.Artifacts.
type Artifact struct {
	Kind    ArtifactKind
	ID      string
	Payload []byte
	Meta    map[string]string
}

// Result wraps audiocpp_result. A Result obtained from a StreamEvent is a
// borrowed view (owned=false internally): Close on it is a documented no-op
// that must not release the event's own memory.
type Result struct {
	handle *C.audiocpp_result
	owned  bool
}

// Close releases the underlying audiocpp_result, unless this Result is a
// borrowed view onto a StreamEvent (see StreamEvent.Result). Safe to call
// more than once, and on a nil *Result.
func (res *Result) Close() {
	if res == nil || res.handle == nil {
		return
	}
	if res.owned {
		C.audiocpp_result_free(res.handle)
	}
	res.handle = nil
}

// Audio returns the result's audio output, or (nil, nil) when the task
// produced no audio.
func (res *Result) Audio() (*AudioBuffer, error) {
	var samplesPtr *C.float
	var frames C.size_t
	var rate, channels C.int
	status := C.audiocpp_result_audio(res.handle, &samplesPtr, &frames, &rate, &channels)
	if Status(status) == StatusNotAvailable {
		return nil, nil
	}
	if err := check(status, "audiocpp_result_audio"); err != nil {
		return nil, err
	}
	n := int(frames) * int(channels)
	return &AudioBuffer{
		Samples:    copyFloats(samplesPtr, n),
		SampleRate: int(rate),
		Channels:   int(channels),
	}, nil
}

// Text returns the result's text output, or (nil, nil) when the task
// produced no text.
func (res *Result) Text() (*TextResult, error) {
	var textPtr, langPtr *C.char
	status := C.audiocpp_result_text(res.handle, &textPtr, &langPtr)
	if Status(status) == StatusNotAvailable {
		return nil, nil
	}
	if err := check(status, "audiocpp_result_text"); err != nil {
		return nil, err
	}
	return &TextResult{Text: C.GoString(textPtr), Language: C.GoString(langPtr)}, nil
}

// Segments returns every segment (VAD/ASR-style spans) in the result.
func (res *Result) Segments() ([]Segment, error) {
	n := int(C.audiocpp_result_segment_count(res.handle))
	out := make([]Segment, 0, n)
	var start, end C.int64_t
	var confidence C.float
	var text *C.char
	for i := 0; i < n; i++ {
		if err := check(
			C.audiocpp_result_segment(res.handle, C.size_t(i), &start, &end, &confidence, &text),
			"audiocpp_result_segment",
		); err != nil {
			return nil, err
		}
		out = append(out, Segment{
			StartSample: int64(start),
			EndSample:   int64(end),
			Confidence:  float32(confidence),
			Text:        C.GoString(text),
		})
	}
	return out, nil
}

// SpeakerTurns returns every diarization turn in the result.
func (res *Result) SpeakerTurns() ([]SpeakerTurn, error) {
	n := int(C.audiocpp_result_speaker_turn_count(res.handle))
	out := make([]SpeakerTurn, 0, n)
	var start, end C.int64_t
	var speaker, text *C.char
	var confidence C.float
	for i := 0; i < n; i++ {
		if err := check(
			C.audiocpp_result_speaker_turn(res.handle, C.size_t(i), &start, &end, &speaker, &confidence, &text),
			"audiocpp_result_speaker_turn",
		); err != nil {
			return nil, err
		}
		out = append(out, SpeakerTurn{
			StartSample: int64(start),
			EndSample:   int64(end),
			SpeakerID:   C.GoString(speaker),
			Confidence:  float32(confidence),
			Text:        C.GoString(text),
		})
	}
	return out, nil
}

// Words returns every word-level alignment in the result.
func (res *Result) Words() ([]Word, error) {
	n := int(C.audiocpp_result_word_count(res.handle))
	out := make([]Word, 0, n)
	var text *C.char
	var start, end C.int64_t
	var confidence C.float
	for i := 0; i < n; i++ {
		if err := check(
			C.audiocpp_result_word(res.handle, C.size_t(i), &text, &start, &end, &confidence),
			"audiocpp_result_word",
		); err != nil {
			return nil, err
		}
		out = append(out, Word{
			Text:        C.GoString(text),
			StartSample: int64(start),
			EndSample:   int64(end),
			Confidence:  float32(confidence),
		})
	}
	return out, nil
}

// NamedAudioList returns every named audio stream in the result.
func (res *Result) NamedAudioList() ([]NamedAudio, error) {
	n := int(C.audiocpp_result_named_audio_count(res.handle))
	out := make([]NamedAudio, 0, n)
	var id *C.char
	var samplesPtr *C.float
	var frames C.size_t
	var rate, channels C.int
	for i := 0; i < n; i++ {
		if err := check(
			C.audiocpp_result_named_audio(res.handle, C.size_t(i), &id, &samplesPtr, &frames, &rate, &channels),
			"audiocpp_result_named_audio",
		); err != nil {
			return nil, err
		}
		out = append(out, NamedAudio{
			ID:         C.GoString(id),
			Samples:    copyFloats(samplesPtr, int(frames)*int(channels)),
			SampleRate: int(rate),
			Channels:   int(channels),
		})
	}
	return out, nil
}

// Artifacts returns every artifact the task produced (TaskResult's single
// artifact_output, if any, followed by its output_artifacts list).
func (res *Result) Artifacts() ([]Artifact, error) {
	n := int(C.audiocpp_result_artifact_count(res.handle))
	out := make([]Artifact, 0, n)
	var kind C.audiocpp_artifact_kind
	var id *C.char
	var payloadPtr unsafe.Pointer
	var payloadBytes C.size_t
	for i := 0; i < n; i++ {
		if err := check(
			C.audiocpp_result_artifact(res.handle, C.size_t(i), &kind, &id, &payloadPtr, &payloadBytes),
			"audiocpp_result_artifact",
		); err != nil {
			return nil, err
		}
		var payload []byte
		if payloadPtr != nil && payloadBytes > 0 {
			payload = C.GoBytes(payloadPtr, C.int(payloadBytes))
		}

		metaCount := int(C.audiocpp_result_artifact_meta_count(res.handle, C.size_t(i)))
		meta := make(map[string]string, metaCount)
		var key, value *C.char
		for j := 0; j < metaCount; j++ {
			if err := check(
				C.audiocpp_result_artifact_meta(res.handle, C.size_t(i), C.size_t(j), &key, &value),
				"audiocpp_result_artifact_meta",
			); err != nil {
				return nil, err
			}
			meta[C.GoString(key)] = C.GoString(value)
		}

		out = append(out, Artifact{
			Kind:    ArtifactKind(kind),
			ID:      C.GoString(id),
			Payload: payload,
			Meta:    meta,
		})
	}
	return out, nil
}

// copyFloats copies n float32s out of a borrowed C float array into a
// freshly-allocated Go slice.
func copyFloats(ptr *C.float, n int) []float32 {
	if n <= 0 || ptr == nil {
		return nil
	}
	src := unsafe.Slice((*float32)(unsafe.Pointer(ptr)), n)
	out := make([]float32, n)
	copy(out, src)
	return out
}
