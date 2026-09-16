package audiocpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -laudiocpp
#include <stdlib.h>
#include <audiocpp.h>
*/
import "C"

// StreamInputKind mirrors audiocpp_stream_input_kind.
type StreamInputKind int

const (
	StreamInputNone        StreamInputKind = 0
	StreamInputAudioChunks StreamInputKind = 1
)

// StreamOutputKind mirrors audiocpp_stream_output_kind.
type StreamOutputKind int

const (
	StreamOutputFinalResult StreamOutputKind = 0
	StreamOutputPullEvents  StreamOutputKind = 1
)

// StreamPolicy describes how a streaming session wants audio pushed to it.
type StreamPolicy struct {
	InputKind    StreamInputKind
	OutputKind   StreamOutputKind
	ChunkSamples int64
	ChunkSeconds float64
}

// Session wraps audiocpp_session (-> engine::runtime::IVoiceTaskSession).
type Session struct {
	handle *C.audiocpp_session
	model  *Model // keeps the model (and its registry) referenced
}

// Close releases the underlying audiocpp_session. Safe to call more than
// once, and on a nil *Session.
func (s *Session) Close() {
	if s == nil || s.handle == nil {
		return
	}
	C.audiocpp_session_free(s.handle)
	s.handle = nil
}

// Family returns the session's model family id.
func (s *Session) Family() string {
	return C.GoString(C.audiocpp_session_family(s.handle))
}

// Prepare forces allocation ahead of Run, using request as a representative
// of what will follow. Optional: Run always prepares itself if this isn't
// called first. A session prepared with one request must not Run another.
func (s *Session) Prepare(request *Request) error {
	return check(C.audiocpp_session_prepare(s.handle, request.handle), "audiocpp_session_prepare")
}

// Run executes request on this (offline) session and returns its result.
func (s *Session) Run(request *Request) (*Result, error) {
	var handle *C.audiocpp_result
	if err := check(
		C.audiocpp_session_run(s.handle, request.handle, &handle),
		"audiocpp_session_run",
	); err != nil {
		return nil, err
	}
	return &Result{handle: handle, owned: true}, nil
}

// StreamPolicy returns the chunk size/shape this (streaming) session wants.
func (s *Session) StreamPolicy() (StreamPolicy, error) {
	var inputKind C.audiocpp_stream_input_kind
	var outputKind C.audiocpp_stream_output_kind
	var chunkSamples C.int64_t
	var chunkSeconds C.double
	if err := check(
		C.audiocpp_stream_policy(s.handle, &inputKind, &outputKind, &chunkSamples, &chunkSeconds),
		"audiocpp_stream_policy",
	); err != nil {
		return StreamPolicy{}, err
	}
	return StreamPolicy{
		InputKind:    StreamInputKind(inputKind),
		OutputKind:   StreamOutputKind(outputKind),
		ChunkSamples: int64(chunkSamples),
		ChunkSeconds: float64(chunkSeconds),
	}, nil
}

// StreamStart begins (or resets) a stream in flight. request may be nil.
func (s *Session) StreamStart(request *Request) error {
	var reqHandle *C.audiocpp_request
	if request != nil {
		reqHandle = request.handle
	}
	return check(C.audiocpp_stream_start(s.handle, reqHandle), "audiocpp_stream_start")
}

// StreamPush feeds one chunk of audio and returns the event it produced, if
// any (nil, nil when the family queued nothing synchronously -- check
// StreamNextEvent).
func (s *Session) StreamPush(samples []float32, sampleRate, channels int, startSample int64) (*StreamEvent, error) {
	frames := framesFor(len(samples), channels)
	var eventHandle *C.audiocpp_event
	if err := check(
		C.audiocpp_stream_push(
			s.handle, cFloatPtr(samples), C.size_t(frames), C.int(sampleRate), C.int(channels), C.int64_t(startSample), &eventHandle,
		),
		"audiocpp_stream_push",
	); err != nil {
		return nil, err
	}
	if eventHandle == nil {
		return nil, nil
	}
	return &StreamEvent{handle: eventHandle}, nil
}

// StreamNextEvent drains one event the family queued on its own. Returns
// (nil, nil) when the queue is empty.
func (s *Session) StreamNextEvent() (*StreamEvent, error) {
	var eventHandle *C.audiocpp_event
	if err := check(
		C.audiocpp_stream_next_event(s.handle, &eventHandle),
		"audiocpp_stream_next_event",
	); err != nil {
		return nil, err
	}
	if eventHandle == nil {
		return nil, nil
	}
	return &StreamEvent{handle: eventHandle}, nil
}

// StreamFinish ends the stream and returns its final result.
func (s *Session) StreamFinish() (*Result, error) {
	var handle *C.audiocpp_result
	if err := check(
		C.audiocpp_stream_finish(s.handle, &handle),
		"audiocpp_stream_finish",
	); err != nil {
		return nil, err
	}
	return &Result{handle: handle, owned: true}, nil
}

// StreamReset discards any stream in flight without producing a result.
func (s *Session) StreamReset() error {
	return check(C.audiocpp_stream_reset(s.handle), "audiocpp_stream_reset")
}
