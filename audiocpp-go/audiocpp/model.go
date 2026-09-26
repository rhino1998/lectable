package audiocpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -laudiocpp
#include <stdlib.h>
#include <audiocpp.h>
*/
import "C"

import "unsafe"

// Model wraps audiocpp_model (-> engine::runtime::ILoadedVoiceModel).
type Model struct {
	handle   *C.audiocpp_model
	registry *Registry // keeps the registry referenced (and alive) for as long as the Model is
}

// Close releases the underlying audiocpp_model. Sessions created from it keep
// working until they are themselves closed. Safe to call more than once, and
// on a nil *Model.
func (m *Model) Close() {
	if m == nil || m.handle == nil {
		return
	}
	C.audiocpp_model_free(m.handle)
	m.handle = nil
}

// Family returns the loaded model's family id, e.g. "qwen3_tts".
func (m *Model) Family() string {
	return C.GoString(C.audiocpp_model_family(m.handle))
}

// Description returns a human-readable description of the loaded model.
func (m *Model) Description() string {
	return C.GoString(C.audiocpp_model_description(m.handle))
}

// Supports reports whether the model supports the given task
// ("vad", "asr", "diar", "sep", "gen", "tts", "clon", "vc", "s2s", "align",
// "vdes", "spk", "svc", "midi", "codec" -- see TaskNames) in the given mode
// ("offline" or "streaming"). An unrecognized task or mode also reports
// false; validate against TaskNames first if that distinction matters.
func (m *Model) Supports(task, mode string) bool {
	cTask := C.CString(task)
	defer C.free(unsafe.Pointer(cTask))
	cMode := C.CString(mode)
	defer C.free(unsafe.Pointer(cMode))
	return C.audiocpp_model_supports(m.handle, cTask, cMode) != 0
}

// SupportsTimestamps reports whether results from this model carry
// segment/word timestamps.
func (m *Model) SupportsTimestamps() bool {
	return C.audiocpp_model_supports_timestamps(m.handle) != 0
}

// SupportsSpeakerReference reports whether this model accepts a speaker
// reference clip (Request.SetVoiceAudio / SetVoiceID) for cloning/conversion.
func (m *Model) SupportsSpeakerReference() bool {
	return C.audiocpp_model_supports_speaker_reference(m.handle) != 0
}

// SupportsStyleCondition reports whether this model accepts style/emotion
// conditioning (Request.SetEmotion, SetStyleTag, etc).
func (m *Model) SupportsStyleCondition() bool {
	return C.audiocpp_model_supports_style_condition(m.handle) != 0
}

// Languages lists the languages this model declares. Note that these are
// per-family spellings, not necessarily ISO codes -- e.g. Qwen3 TTS wants
// "english", not "en".
func (m *Model) Languages() []string {
	n := int(C.audiocpp_model_language_count(m.handle))
	out := make([]string, n)
	var lang *C.char
	for i := 0; i < n; i++ {
		if err := check(
			C.audiocpp_model_language(m.handle, C.size_t(i), &lang),
			"audiocpp_model_language",
		); err != nil {
			out[i] = ""
			continue
		}
		out[i] = C.GoString(lang)
	}
	return out
}

// OptionScope selects which of a model's declared option maps to enumerate,
// mirroring audiocpp_option_scope.
type OptionScope int

const (
	OptionScopeRequest OptionScope = 0
	OptionScopeSession OptionScope = 1
	OptionScopeLoad    OptionScope = 2
)

// ModelOption describes one entry from a model's declared option surface
// (ModelInspection::cli on the C++ side), letting a caller enumerate what a
// family accepts instead of hardcoding per-family knowledge.
type ModelOption struct {
	Name         string
	ValueName    string
	Description  string
	DefaultValue string
	MinValue     string
	MaxValue     string
	Required     bool
}

// Options enumerates the model's declared options in the given scope.
func (m *Model) Options(scope OptionScope) ([]ModelOption, error) {
	n := int(C.audiocpp_model_option_count(m.handle, C.audiocpp_option_scope(scope)))
	out := make([]ModelOption, 0, n)
	var (
		name, valueName, description, defaultValue, minValue, maxValue *C.char
		required                                                       C.int
	)
	for i := 0; i < n; i++ {
		if err := check(
			C.audiocpp_model_option(
				m.handle, C.audiocpp_option_scope(scope), C.size_t(i),
				&name, &valueName, &description, &defaultValue, &minValue, &maxValue, &required,
			),
			"audiocpp_model_option",
		); err != nil {
			return nil, err
		}
		out = append(out, ModelOption{
			Name:         C.GoString(name),
			ValueName:    C.GoString(valueName),
			Description:  C.GoString(description),
			DefaultValue: C.GoString(defaultValue),
			MinValue:     C.GoString(minValue),
			MaxValue:     C.GoString(maxValue),
			Required:     required != 0,
		})
	}
	return out, nil
}

// BackendConfig selects the compute backend a Session runs on, mirroring
// audiocpp_backend_config. Backend is one of "cpu", "cuda", "hip"/"rocm",
// "vulkan", "metal", "best", or empty for "cpu". Device and Threads mirror
// --device / --threads; the zero value (device 0, 1 thread) is a valid CPU
// configuration.
type BackendConfig struct {
	Backend string
	Device  int
	Threads int
}

// Session creates a task session for this model. task and mode take the same
// spellings as --task/--mode (see TaskNames and Model.Supports). options
// corresponds to --session-option and may be nil.
func (m *Model) Session(task, mode string, backend BackendConfig, options *Options) (*Session, error) {
	cTask := C.CString(task)
	defer C.free(unsafe.Pointer(cTask))
	cMode := C.CString(mode)
	defer C.free(unsafe.Pointer(cMode))

	cBackendName, freeBackendName := optCString(backend.Backend)
	defer freeBackendName()
	cBackend := C.audiocpp_backend_config{
		backend: cBackendName,
		device:  C.int(backend.Device),
		threads: C.int(backend.Threads),
	}

	var handle *C.audiocpp_session
	if err := check(
		C.audiocpp_session_create(m.handle, cTask, cMode, &cBackend, options.cOptions(), &handle),
		"audiocpp_session_create",
	); err != nil {
		return nil, err
	}
	return &Session{handle: handle, model: m}, nil
}
