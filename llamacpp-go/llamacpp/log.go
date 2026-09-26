package llamacpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -lllama -lggml -lggml-base -lggml-cpu
#include <stdlib.h>
#include <llama.h>

extern void goLlamacppLogCallback(int level, char *text, void *user_data);

// llama_log_set wants a plain C function pointer, not a Go one, and its
// first parameter is an enum (ABI-compatible with int but not the same C
// type) -- this trampoline is the shim that lets the exported Go function
// register as that callback.
static void llamacpp_log_trampoline(enum ggml_log_level level, const char * text, void * user_data) {
	goLlamacppLogCallback((int)level, (char *)text, user_data);
}

static void llamacpp_install_log_callback(void) {
	llama_log_set(llamacpp_log_trampoline, NULL);
}
*/
import "C"

import (
	"os"
	"strings"
	"sync"
	"unsafe"
)

var (
	logMu    sync.Mutex
	lastWarn string

	// verboseLog, set from LLAMACPP_LOG=1, also copies every libllama log
	// line (every level) to stderr - which device a model landed on,
	// buffer sizes, whether flash attention was enabled, and so on.
	verboseLog = os.Getenv("LLAMACPP_LOG") == "1"
)

// goLlamacppLogCallback receives every line libllama logs. llama.cpp's C API
// otherwise has no way to retrieve a message for a failed call (load
// failures, bad chat templates, etc. come back as NULL/false/-1 with no
// string) -- capturing WARN/ERROR lines here is what lets errors.go attach a
// human-readable reason to those.
//
//export goLlamacppLogCallback
func goLlamacppLogCallback(level C.int, text *C.char, _ unsafe.Pointer) {
	if verboseLog {
		os.Stderr.WriteString(C.GoString(text))
	}
	if level < C.GGML_LOG_LEVEL_WARN {
		return
	}
	msg := strings.TrimRight(C.GoString(text), "\n")
	if msg == "" {
		return
	}
	logMu.Lock()
	lastWarn = msg
	logMu.Unlock()
}

func init() {
	C.llamacpp_install_log_callback()
}

// popLastLogLine returns and clears the most recent WARN/ERROR line logged
// by libllama.
func popLastLogLine() string {
	logMu.Lock()
	defer logMu.Unlock()
	msg := lastWarn
	lastWarn = ""
	return msg
}
