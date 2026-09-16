package llmworker

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

func writeError(w http.ResponseWriter, status int, err error) {
	log.Printf("llmworker: request failed: %v", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ttsproto.ErrorResponse{Detail: err.Error()})
}

// HandleGenerate serves POST /llm/generate.
func (w *Worker) HandleGenerate(rw http.ResponseWriter, r *http.Request) {
	var req ttsproto.LLMGenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	text, err := w.LLMGenerate(r.Context(), req.SystemPrompt, req.UserPrompt, req.Temp, req.MaxTokens)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(ttsproto.LLMGenerateResponse{Text: text})
}

// HandleUnload serves POST /llm/unload: frees the loaded model right now
// (see Worker.Unload's own doc comment) rather than waiting for Config.
// IdleUnloadAfter - called by cmd/server (via internal/ttsworker.Manager.
// UnloadLLM) on a genuine pool-family switch away from LLM work, or when
// the reader pauses the whole job queue.
func (w *Worker) HandleUnload(rw http.ResponseWriter, r *http.Request) {
	w.Unload()
	rw.WriteHeader(http.StatusNoContent)
}
