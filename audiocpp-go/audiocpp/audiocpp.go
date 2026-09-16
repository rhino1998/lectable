// Package audiocpp is a cgo wrapper around audio.cpp's C ABI. The header
// (audiocpp.h) is vendored in this package's include/ directory, matching
// the version documented at https://github.com/0xShug0/audio.cpp's
// docs/c_api.md; the native library it declares (libaudiocpp.so) is not
// vendored and must be built from an audio.cpp checkout -- see this
// module's README for details.
//
// Every exported type mirrors a C ABI handle 1:1 (Registry -> audiocpp_registry,
// Model -> audiocpp_model, Session -> audiocpp_session, ...) and follows the
// same contract the C side documents:
//
//   - Handles hold their parents alive internally, so it is safe to Close a
//     Registry or Model before the Session/Result derived from it.
//   - Errors come back as *Error instead of an audiocpp_status.
//   - Anything borrowed from a Result (audio samples, text, artifact payloads)
//     is copied into plain Go values eagerly, so it stays valid after the
//     Result is closed.
//   - Every handle type has a Close method. Call it (typically via defer) when
//     you are done; Close on an already-closed or zero-value handle is a no-op.
//
// Quick start:
//
//	registry, err := audiocpp.NewRegistry("")
//	if err != nil { ... }
//	defer registry.Close()
//
//	model, err := registry.LoadModel("assets/framework/models/silero_vad", audiocpp.ModelConfig{FamilyHint: "silero_vad"}, nil)
//	if err != nil { ... }
//	defer model.Close()
//
//	session, err := model.Session("vad", "offline", audiocpp.BackendConfig{Backend: "cpu"}, nil)
//	if err != nil { ... }
//	defer session.Close()
//
//	clip, err := audiocpp.ReadWavFloat32("assets/resources/sample_16k.wav")
//	if err != nil { ... }
//
//	request := audiocpp.NewRequest()
//	defer request.Close()
//	request.SetAudio(clip.Samples, clip.SampleRate, clip.Channels)
//
//	result, err := session.Run(request)
//	if err != nil { ... }
//	defer result.Close()
//	segments, err := result.Segments()
//	if err != nil { ... }
//	for _, seg := range segments {
//		fmt.Println(seg.StartSample, seg.EndSample, seg.Confidence)
//	}
//
// # Building and linking
//
// The C API is off by default in audio.cpp's own CMake build; build it once
// from an audio.cpp checkout:
//
//	cmake -S . -B build -DAUDIOCPP_BUILD_C_API=ON
//	cmake --build build --target audiocpp
//
// That produces build/bin/libaudiocpp.so (.dylib on macOS, .dll on Windows).
// This package's cgo directives already add -I<this package>/include for
// the vendored header, but linking and runtime loading of the built library
// itself are not vendored: point cgo and the dynamic loader at your build
// directory before building or running anything that imports this package:
//
//	export CGO_LDFLAGS="-L/path/to/audio.cpp/build/bin"
//	export LD_LIBRARY_PATH="/path/to/audio.cpp/build/bin:$LD_LIBRARY_PATH"   # Linux
//	export DYLD_LIBRARY_PATH="/path/to/audio.cpp/build/bin:$DYLD_LIBRARY_PATH" # macOS
//
// go build/go test/go run all honor CGO_LDFLAGS; the *_LIBRARY_PATH variable
// is what lets the resulting binary find libaudiocpp at process start, same
// as any other shared library.
package audiocpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -laudiocpp
#include <stdlib.h>
#include <audiocpp.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Status mirrors audiocpp_status. Values match the C enum exactly.
type Status int

const (
	StatusOK                Status = 0
	StatusInvalidArgument   Status = 1
	StatusUnsupportedFamily Status = 2
	StatusLoadFailed        Status = 3
	StatusRuntime           Status = 4
	StatusOutOfMemory       Status = 5
	StatusOutOfRange        Status = 6
	StatusNotAvailable      Status = 7
)

// String returns audiocpp_status_string's name for the status.
func (s Status) String() string {
	return C.GoString(C.audiocpp_status_string(C.audiocpp_status(s)))
}

// Error is returned whenever a C ABI call returns anything other than
// AUDIOCPP_OK. It implements the error interface.
type Error struct {
	Status  Status
	Call    string
	Message string
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "(no detail)"
	}
	if e.Call != "" {
		return fmt.Sprintf("%s: %s: %s", e.Call, e.Status, msg)
	}
	return fmt.Sprintf("%s: %s", e.Status, msg)
}

func newError(status C.audiocpp_status, call string) *Error {
	return &Error{
		Status:  Status(status),
		Call:    call,
		Message: C.GoString(C.audiocpp_last_error()),
	}
}

// check converts a non-OK audiocpp_status into an *Error, capturing
// audiocpp_last_error() immediately (it is only valid until this thread's
// next C ABI call).
func check(status C.audiocpp_status, call string) error {
	if status == C.AUDIOCPP_OK {
		return nil
	}
	return newError(status, call)
}

// ABIVersionMajor is the major ABI version this package was written against
// (AUDIOCPP_ABI_VERSION_MAJOR at the time these bindings were generated). A
// loaded libaudiocpp reporting a different major version is incompatible;
// this is checked once in init.
const ABIVersionMajor = 0

// ABIVersion returns audiocpp_abi_version(): packed (major<<16)|(minor<<8)|patch.
func ABIVersion() uint32 {
	return uint32(C.audiocpp_abi_version())
}

// ABIVersionParts returns ABIVersion() split into (major, minor, patch).
func ABIVersionParts() (major, minor, patch int) {
	v := ABIVersion()
	return int(v >> 16 & 0xFFFF), int(v >> 8 & 0xFF), int(v & 0xFF)
}

// BuildVersion returns audio.cpp's own build version string, e.g. "0.2.1".
func BuildVersion() string {
	return C.GoString(C.audiocpp_build_version())
}

// TaskNames returns every task token this build accepts (audiocpp_task_name
// enumerated over audiocpp_task_count).
func TaskNames() []string {
	n := int(C.audiocpp_task_count())
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = C.GoString(C.audiocpp_task_name(C.size_t(i)))
	}
	return out
}

// TaskFromSpecName maps a model-spec task name ("music") to the ABI's own
// task token ("gen"). The second return is false when specTask names no task
// kind this build knows.
func TaskFromSpecName(specTask string) (string, bool) {
	cSpecTask := C.CString(specTask)
	defer C.free(unsafe.Pointer(cSpecTask))
	result := C.audiocpp_task_from_spec_name(cSpecTask)
	if result == nil {
		return "", false
	}
	return C.GoString(result), true
}

// optCString returns a C string for s, or NULL when s is empty -- matching
// the C ABI's convention that these identifier-shaped fields are NULL
// (unset) rather than a distinct empty string. The returned free func is
// always safe to call, including when it's a no-op for the NULL case.
func optCString(s string) (*C.char, func()) {
	if s == "" {
		return nil, func() {}
	}
	cs := C.CString(s)
	return cs, func() { C.free(unsafe.Pointer(cs)) }
}

func init() {
	major, _, _ := ABIVersionParts()
	if major != ABIVersionMajor {
		panic(fmt.Sprintf(
			"audiocpp: libaudiocpp reports ABI major version %d, this package was "+
				"written against major version %d; rebuild the library or update the bindings",
			major, ABIVersionMajor,
		))
	}
}
