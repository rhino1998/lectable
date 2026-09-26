package audiocpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -laudiocpp
#include <stdlib.h>
#include <audiocpp.h>
*/
import "C"

import "unsafe"

// ArtifactKind mirrors audiocpp_artifact_kind -- opaque payloads a family
// produces or consumes (speaker embeddings, acoustic tokens, MIDI,
// diarization state).
type ArtifactKind int

const (
	ArtifactSpeakerEmbedding    ArtifactKind = 0
	ArtifactStyleEmbedding      ArtifactKind = 1
	ArtifactPromptEmbedding     ArtifactKind = 2
	ArtifactAcousticTokens      ArtifactKind = 3
	ArtifactMIDI                ArtifactKind = 4
	ArtifactTranscriptAlignment ArtifactKind = 5
	ArtifactDiarizationState    ArtifactKind = 6
	ArtifactVADState            ArtifactKind = 7
	ArtifactCustom              ArtifactKind = 8
	// ArtifactLatents is a model's continuous pre-decoder representation;
	// see Latents.
	ArtifactLatents ArtifactKind = 9
)

// Request wraps audiocpp_request. Setters return the Request so calls can be
// chained; each reports an error rather than panicking so callers who need
// to distinguish which setter failed still can (check err after the chain,
// same as any other Go builder).
type Request struct {
	handle *C.audiocpp_request
	err    error // first error encountered by a chained setter, if any
}

// NewRequest creates an empty request.
func NewRequest() *Request {
	handle := C.audiocpp_request_create()
	return &Request{handle: handle}
}

// Close releases the underlying audiocpp_request. Safe to call more than
// once, and on a nil *Request.
func (r *Request) Close() {
	if r == nil || r.handle == nil {
		return
	}
	C.audiocpp_request_free(r.handle)
	r.handle = nil
}

// Err returns the first error a chained setter encountered, or nil.
func (r *Request) Err() error {
	return r.err
}

func (r *Request) fail(err error) *Request {
	if r.err == nil {
		r.err = err
	}
	return r
}

// SetText sets the request text and, when language is non-empty, both the
// transcript language and options["language"] -- mirroring what
// audiocpp_cli's --language does. Some families read only the option, so the
// two travel together by default; use SetTextLanguage to set the transcript
// language alone.
func (r *Request) SetText(text, language string) *Request {
	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))
	cLanguage, freeLanguage := optCString(language)
	defer freeLanguage()
	if err := check(
		C.audiocpp_request_set_text(r.handle, cText, cLanguage),
		"audiocpp_request_set_text",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetTextLanguage sets the transcript language without touching
// options["language"].
func (r *Request) SetTextLanguage(language string) *Request {
	cLanguage := C.CString(language)
	defer C.free(unsafe.Pointer(cLanguage))
	if err := check(
		C.audiocpp_request_set_text_language(r.handle, cLanguage),
		"audiocpp_request_set_text_language",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetAudio sets the request's input audio. samples is interleaved PCM,
// copied into the request by the C API, so it need not outlive this call.
func (r *Request) SetAudio(samples []float32, sampleRate, channels int) *Request {
	frames := framesFor(len(samples), channels)
	if err := check(
		C.audiocpp_request_set_audio(r.handle, cFloatPtr(samples), C.size_t(frames), C.int(sampleRate), C.int(channels)),
		"audiocpp_request_set_audio",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetVoiceAudio sets a speaker reference clip for cloning/conversion
// families (see Model.SupportsSpeakerReference).
func (r *Request) SetVoiceAudio(samples []float32, sampleRate, channels int) *Request {
	frames := framesFor(len(samples), channels)
	if err := check(
		C.audiocpp_request_set_voice_audio(r.handle, cFloatPtr(samples), C.size_t(frames), C.int(sampleRate), C.int(channels)),
		"audiocpp_request_set_voice_audio",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetVoiceID references a previously cached voice by id in place of a raw
// reference clip.
func (r *Request) SetVoiceID(cachedVoiceID string) *Request {
	cID := C.CString(cachedVoiceID)
	defer C.free(unsafe.Pointer(cID))
	if err := check(
		C.audiocpp_request_set_voice_id(r.handle, cID),
		"audiocpp_request_set_voice_id",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetStyleLanguage sets --style-language, for families that advertise style
// conditioning (see Model.SupportsStyleCondition).
func (r *Request) SetStyleLanguage(language string) *Request {
	cLanguage := C.CString(language)
	defer C.free(unsafe.Pointer(cLanguage))
	if err := check(
		C.audiocpp_request_set_style_language(r.handle, cLanguage),
		"audiocpp_request_set_style_language",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetEmotion sets --emotion.
func (r *Request) SetEmotion(emotion string) *Request {
	cEmotion := C.CString(emotion)
	defer C.free(unsafe.Pointer(cEmotion))
	if err := check(
		C.audiocpp_request_set_emotion(r.handle, cEmotion),
		"audiocpp_request_set_emotion",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetSpeakingRate sets --speaking-rate.
func (r *Request) SetSpeakingRate(rate float32) *Request {
	if err := check(
		C.audiocpp_request_set_speaking_rate(r.handle, C.float(rate)),
		"audiocpp_request_set_speaking_rate",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetPitchShift sets --pitch-shift (semitones).
func (r *Request) SetPitchShift(semitones float32) *Request {
	if err := check(
		C.audiocpp_request_set_pitch_shift(r.handle, C.float(semitones)),
		"audiocpp_request_set_pitch_shift",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetEnergyScale sets --energy-scale.
func (r *Request) SetEnergyScale(scale float32) *Request {
	if err := check(
		C.audiocpp_request_set_energy_scale(r.handle, C.float(scale)),
		"audiocpp_request_set_energy_scale",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetStyleTag sets --style-tag key=value.
func (r *Request) SetStyleTag(key, value string) *Request {
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	cValue := C.CString(value)
	defer C.free(unsafe.Pointer(cValue))
	if err := check(
		C.audiocpp_request_set_style_tag(r.handle, cKey, cValue),
		"audiocpp_request_set_style_tag",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// AddArtifact attaches an opaque artifact payload to the request (copied, so
// payload need not outlive the call) and returns its index, for use with
// SetArtifactMeta.
func (r *Request) AddArtifact(kind ArtifactKind, id string, payload []byte) (int, error) {
	cID := C.CString(id)
	defer C.free(unsafe.Pointer(cID))

	var payloadPtr unsafe.Pointer
	if len(payload) > 0 {
		payloadPtr = unsafe.Pointer(&payload[0])
	}

	var index C.size_t
	if err := check(
		C.audiocpp_request_add_artifact(r.handle, C.audiocpp_artifact_kind(kind), cID, payloadPtr, C.size_t(len(payload)), &index),
		"audiocpp_request_add_artifact",
	); err != nil {
		return 0, err
	}
	return int(index), nil
}

// SetArtifactMeta attaches a key/value metadata pair to the artifact at
// index (as returned by AddArtifact).
func (r *Request) SetArtifactMeta(index int, key, value string) *Request {
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	cValue := C.CString(value)
	defer C.free(unsafe.Pointer(cValue))
	if err := check(
		C.audiocpp_request_set_artifact_meta(r.handle, C.size_t(index), cKey, cValue),
		"audiocpp_request_set_artifact_meta",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetOption sets a single-valued request option (--request-option key=value).
// Enumerate what a model accepts with Model.Options(OptionScopeRequest).
func (r *Request) SetOption(key, value string) *Request {
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	cValue := C.CString(value)
	defer C.free(unsafe.Pointer(cValue))
	if err := check(
		C.audiocpp_request_set_option(r.handle, cKey, cValue),
		"audiocpp_request_set_option",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// SetOptionArray sets a list-valued request option. A second call with the
// same key replaces the list rather than appending. values may be empty,
// which sets an empty list -- distinct from never setting the key.
func (r *Request) SetOptionArray(key string, values []string) *Request {
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))

	n := len(values)
	cValues := make([]*C.char, n)
	for i, v := range values {
		cValues[i] = C.CString(v)
	}
	defer func() {
		for _, cv := range cValues {
			C.free(unsafe.Pointer(cv))
		}
	}()

	var valuesPtr **C.char
	if n > 0 {
		valuesPtr = &cValues[0]
	}
	if err := check(
		C.audiocpp_request_set_option_array(r.handle, cKey, valuesPtr, C.size_t(n)),
		"audiocpp_request_set_option_array",
	); err != nil {
		return r.fail(err)
	}
	return r
}

// framesFor derives a per-channel frame count from an interleaved sample
// count, matching the C ABI's "frames is per-channel" convention.
func framesFor(sampleCount, channels int) int {
	if channels <= 0 {
		return 0
	}
	return sampleCount / channels
}

// cFloatPtr returns a pointer to samples' backing array, or NULL for an
// empty slice. Valid only for the duration of the cgo call it's passed to.
func cFloatPtr(samples []float32) *C.float {
	if len(samples) == 0 {
		return nil
	}
	return (*C.float)(unsafe.Pointer(&samples[0]))
}
