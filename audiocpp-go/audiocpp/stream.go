package audiocpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -laudiocpp
#include <stdlib.h>
#include <audiocpp.h>
*/
import "C"

// VoiceActivityKind mirrors audiocpp_voice_activity_kind.
type VoiceActivityKind int

const (
	VoiceActivitySpeechStart   VoiceActivityKind = 0
	VoiceActivitySpeechEnd     VoiceActivityKind = 1
	VoiceActivitySpeechSegment VoiceActivityKind = 2
)

// VoiceActivity is one entry from StreamEvent.VoiceActivity.
type VoiceActivity struct {
	Kind        VoiceActivityKind
	Sample      int64
	Probability float32
}

// StreamEvent wraps audiocpp_event, one pull-based update from a streaming
// session (audiocpp_stream_push / audiocpp_stream_next_event).
type StreamEvent struct {
	handle *C.audiocpp_event
}

// Close releases the underlying audiocpp_event. Safe to call more than once,
// and on a nil *StreamEvent.
func (e *StreamEvent) Close() {
	if e == nil || e.handle == nil {
		return
	}
	C.audiocpp_event_free(e.handle)
	e.handle = nil
}

// IsFinal reports whether this event carries the stream's final result.
func (e *StreamEvent) IsFinal() bool {
	return C.audiocpp_event_is_final(e.handle) != 0
}

// Result returns a borrowed view onto this event's data, read through the
// same accessors as a Session.Run result. Valid only until the event is
// closed; its own Close is a no-op that leaves the event's memory alone.
func (e *StreamEvent) Result() *Result {
	return &Result{handle: C.audiocpp_event_as_result(e.handle), owned: false}
}

// VoiceActivity returns every voice-activity marker this event carries.
func (e *StreamEvent) VoiceActivity() ([]VoiceActivity, error) {
	n := int(C.audiocpp_event_voice_activity_count(e.handle))
	out := make([]VoiceActivity, 0, n)
	var kind C.audiocpp_voice_activity_kind
	var sample C.int64_t
	var probability C.float
	for i := 0; i < n; i++ {
		if err := check(
			C.audiocpp_event_voice_activity(e.handle, C.size_t(i), &kind, &sample, &probability),
			"audiocpp_event_voice_activity",
		); err != nil {
			return nil, err
		}
		out = append(out, VoiceActivity{
			Kind:        VoiceActivityKind(kind),
			Sample:      int64(sample),
			Probability: float32(probability),
		})
	}
	return out, nil
}
