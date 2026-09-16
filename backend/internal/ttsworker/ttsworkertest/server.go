// Package ttsworkertest is a fake ttsworker HTTP server for tests: it
// speaks the same wire format as cmd/ttsworker (internal/ttsproto) over a
// real loopback httptest.Server, but every response is synthesized
// in-process - no audio.cpp, no GPU, no model weights. It exists so
// internal/jobs, internal/voicerefs, and internal/httpapi can be tested
// against a real *ttsworker.Manager (exercising the actual HTTP client
// code, not a hand-rolled substitute) without needing a real ttsworker
// process, which needs libaudiocpp.so and real (slow, GPU-bound)
// inference - exactly what the "no real GPU/heavy compute" testing
// constraint rules out.
//
// This needs no changes to internal/ttsworker itself: Manager's exported
// Generate/Design/Align/Health methods only ever need a baseURL and
// http.Client (see ttsworker.New) - Start (which spawns a real ttsworker
// subprocess) is never called here, so New's own Config{Port: ...} is
// enough to point a fully-functional Manager at this fake server instead
// of a real one.
package ttsworkertest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// sampleRate/channels match nothing in particular about real audio.cpp
// output - just a fixed, cheap-to-synthesize format every response uses,
// since nothing in the backend (WSOLA re-speed, wav.Duration) cares what
// rate/channel count a clip was recorded at, only that its own header
// says so correctly.
const (
	sampleRate = 16000
	channels   = 1
	// secondsPerChar is how long the fake Generate/Design responses claim
	// their audio is, given the input text/refText's length - proportional
	// rather than fixed so tests asserting on relative durations (e.g. "the
	// split text produced two roughly-equal-length halves") still see a
	// meaningful signal, without ever actually taking that long to run.
	secondsPerChar = 0.01
	minSeconds     = 0.05
)

// Server is a fake ttsworker, and a log of every call it received - tests
// can inspect Generate/Design/Align calls after exercising code that talks
// to Manager(), instead of only observing side effects (files written,
// store rows changed) further downstream.
type Server struct {
	*httptest.Server

	mu             sync.Mutex
	generateCalls  []ttsproto.GenerateRequest
	designCalls    []ttsproto.DesignRequest
	alignCalls     []ttsproto.AlignRequest
	llmGenCalls    []ttsproto.LLMGenerateRequest
	generateCount  int
	designCount    int
	unloadTTSCount int
	unloadLLMCount int

	// OnGenerate/OnDesign/OnAlign/OnLLMGenerate let a test override the
	// default synthetic behavior (e.g. to simulate a failure, or return a
	// specific reply) - nil means "use the default". Set directly on the
	// Server value before the code under test calls it; not safe to
	// mutate concurrently with an in-flight request.
	OnGenerate    func(ttsproto.GenerateRequest) ([]byte, error)
	OnDesign      func(ttsproto.DesignRequest) ([]byte, error)
	OnAlign       func(ttsproto.AlignRequest) ([]ttsproto.Word, error)
	OnLLMGenerate func(ttsproto.LLMGenerateRequest) (string, error)
}

// New starts a fake ttsworker and registers its shutdown with t.Cleanup.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /generate", s.handleGenerate)
	mux.HandleFunc("POST /design", s.handleDesign)
	mux.HandleFunc("POST /align", s.handleAlign)
	mux.HandleFunc("POST /llm/generate", s.handleLLMGenerate)
	mux.HandleFunc("POST /unload", s.handleUnloadTTS)
	mux.HandleFunc("POST /llm/unload", s.handleUnloadLLM)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Server.Close)
	return s
}

// Manager returns a *ttsworker.Manager wired to talk to this fake server -
// safe to pass anywhere production code expects a real one (jobs.NewManager,
// httpapi.Server.TTS, voicerefs.EnsureFile/Regenerate, ...).
func (s *Server) Manager() *ttsworker.Manager {
	u, err := url.Parse(s.Server.URL)
	if err != nil {
		panic(err) // httptest.Server's own URL is always valid
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		panic(err) // httptest.Server always listens on 127.0.0.1:<numeric port>
	}
	return ttsworker.New(ttsworker.Config{Port: port})
}

func synthesize(charCount int) []byte {
	seconds := float64(charCount) * secondsPerChar
	if seconds < minSeconds {
		seconds = minSeconds
	}
	n := int(seconds * sampleRate)
	samples := make([]float32, n*channels) // silence
	data, err := wav.Encode(samples, sampleRate, channels)
	if err != nil {
		panic(err) // wav.Encode only fails on a bad channel count, which is fixed above
	}
	return data
}

func writeAudio(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "audio/wav")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(ttsproto.ErrorResponse{Detail: err.Error()})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ttsproto.HealthResponse{
		Status:            "ok",
		DefaultCloneModel: "audiocpp-higgs-4b",
		CloneModelsLoaded: []string{"audiocpp-higgs-4b"},
		LLMModelLoaded:    true,
	})
}

func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req ttsproto.GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	s.generateCalls = append(s.generateCalls, req)
	s.generateCount++
	hook := s.OnGenerate
	s.mu.Unlock()

	if hook != nil {
		data, err := hook(req)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeAudio(w, data)
		return
	}
	writeAudio(w, synthesize(len(req.Text)))
}

func (s *Server) handleDesign(w http.ResponseWriter, r *http.Request) {
	var req ttsproto.DesignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	s.designCalls = append(s.designCalls, req)
	s.designCount++
	hook := s.OnDesign
	s.mu.Unlock()

	if hook != nil {
		data, err := hook(req)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeAudio(w, data)
		return
	}
	writeAudio(w, synthesize(len(req.RefText)))
}

func (s *Server) handleAlign(w http.ResponseWriter, r *http.Request) {
	var req ttsproto.AlignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	s.alignCalls = append(s.alignCalls, req)
	hook := s.OnAlign
	s.mu.Unlock()

	if hook != nil {
		words, err := hook(req)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ttsproto.AlignResponse{Words: words})
		return
	}

	// Default: one word per whitespace-separated token, evenly spaced -
	// enough for tests asserting word count/ordering, not real timing.
	fields := strings.Fields(req.Text)
	words := make([]ttsproto.Word, len(fields))
	const perWord = 0.3
	for i, f := range fields {
		words[i] = ttsproto.Word{Text: f, Start: float64(i) * perWord, End: float64(i+1) * perWord}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ttsproto.AlignResponse{Words: words})
}

func (s *Server) handleLLMGenerate(w http.ResponseWriter, r *http.Request) {
	var req ttsproto.LLMGenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	s.llmGenCalls = append(s.llmGenCalls, req)
	hook := s.OnLLMGenerate
	s.mu.Unlock()

	if hook != nil {
		text, err := hook(req)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ttsproto.LLMGenerateResponse{Text: text})
		return
	}
	// Default: echo the user prompt back, wrapped enough to be visibly
	// distinct from it - enough for tests asserting a call happened and
	// what it was asked, without needing a real model.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ttsproto.LLMGenerateResponse{Text: "[fake reply to] " + req.UserPrompt})
}

func (s *Server) handleUnloadTTS(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.unloadTTSCount++
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUnloadLLM(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.unloadLLMCount++
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// UnloadTTSCount/UnloadLLMCount report how many times this fake server has
// received a POST /unload or POST /llm/unload call so far - for a test
// asserting that internal/jobs actually triggered a model unload (a pool-
// family switch, or Pause - see jobs.Manager.onPoolActivate/Pause).
func (s *Server) UnloadTTSCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unloadTTSCount
}

func (s *Server) UnloadLLMCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unloadLLMCount
}

// GenerateCalls/DesignCalls/AlignCalls/LLMGenerateCalls return a snapshot of every call
// received so far, in order - copies, safe to range over even while more
// calls may still be arriving concurrently.
func (s *Server) GenerateCalls() []ttsproto.GenerateRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ttsproto.GenerateRequest(nil), s.generateCalls...)
}

func (s *Server) DesignCalls() []ttsproto.DesignRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ttsproto.DesignRequest(nil), s.designCalls...)
}

func (s *Server) AlignCalls() []ttsproto.AlignRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ttsproto.AlignRequest(nil), s.alignCalls...)
}

func (s *Server) LLMGenerateCalls() []ttsproto.LLMGenerateRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ttsproto.LLMGenerateRequest(nil), s.llmGenCalls...)
}
