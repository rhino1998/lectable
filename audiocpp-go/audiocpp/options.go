package audiocpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -laudiocpp
#include <stdlib.h>
#include <audiocpp.h>
*/
import "C"

import "unsafe"

// Options wraps audiocpp_options: a string->string map corresponding to
// --load-option / --session-option / --request-option depending on where
// it's used. The zero value is not usable; create one with NewOptions.
type Options struct {
	handle *C.audiocpp_options
}

// NewOptions creates an Options handle, optionally pre-populated from values.
// Call Close when done with it (LoadModel/Session copy what they need out of
// it during the call, so it's safe to Close immediately afterward).
func NewOptions(values map[string]string) (*Options, error) {
	handle := C.audiocpp_options_create()
	if handle == nil {
		return nil, &Error{Status: StatusOutOfMemory, Call: "audiocpp_options_create", Message: "allocation failed"}
	}
	o := &Options{handle: handle}
	for k, v := range values {
		if err := o.Set(k, v); err != nil {
			o.Close()
			return nil, err
		}
	}
	return o, nil
}

// Set assigns one key/value pair, overwriting any previous value for key.
func (o *Options) Set(key, value string) error {
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	cValue := C.CString(value)
	defer C.free(unsafe.Pointer(cValue))
	return check(C.audiocpp_options_set(o.handle, cKey, cValue), "audiocpp_options_set")
}

// Close releases the underlying audiocpp_options. Safe to call more than
// once, and safe to call on a nil *Options.
func (o *Options) Close() {
	if o == nil || o.handle == nil {
		return
	}
	C.audiocpp_options_free(o.handle)
	o.handle = nil
}

// cOptions returns the underlying handle, or NULL for a nil Options.
func (o *Options) cOptions() *C.audiocpp_options {
	if o == nil {
		return nil
	}
	return o.handle
}
