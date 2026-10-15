package llamacpp

import (
	"errors"
	"fmt"
)

// ErrContextFull is a Scheduler request's error when its own sequence has
// no room for it: the prompt alone doesn't fit, or the reply reached the
// end of the sequence before MaxTokens or an end-of-generation token.
var ErrContextFull = errors.New("llamacpp: context full")

// Error is returned by calls that fail without libllama giving back a
// dedicated status code -- Detail carries the most recent WARN/ERROR line
// libllama logged for that failure, if any (see log.go); it is often empty
// for failures libllama doesn't log about, such as a too-small output
// buffer.
type Error struct {
	Call   string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("llamacpp: %s failed", e.Call)
	}
	return fmt.Sprintf("llamacpp: %s failed: %s", e.Call, e.Detail)
}

func newError(call string) *Error {
	return &Error{Call: call, Detail: popLastLogLine()}
}
