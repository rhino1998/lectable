package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// testLLMRequest is POST /api/llm/test's body - a raw system+user prompt
// pair, no speakerattr-specific framing.
type testLLMRequest struct {
	SystemPrompt string  `json:"systemPrompt,omitempty"`
	UserPrompt   string  `json:"userPrompt"`
	Temp         float32 `json:"temp,omitempty"`
	MaxTokens    int     `json:"maxTokens,omitempty"`
}

// handleTestLLM is a standalone raw-prompt test surface for this app's
// own embedded speaker-attribution GGUF model (internal/llmworker,
// proxied through ttsworker.Manager.LLMGenerate) - stateless, not tied to
// any book/chapter, for exercising a system+user prompt pair directly
// (e.g. iterating on a speakerattr-style prompt) without running a real
// attribution/characterization/direction-tagging pass.
//
// Dispatched through jobs.Manager.RunLLMPreview (KindLLMPreview, poolLLM)
// rather than calling s.TTS.LLMGenerate directly - same "handled as a
// pooled job" reasoning handleGenerateSFX's own doc comment gives:
// llmworker already serves concurrent calls natively via multi-sequence
// batching (llamacpp.Scheduler - see backend/CLAUDE.md's "ttsworker /
// audioworker / llmworker" section), so this doesn't buy GPU-residency
// mutual exclusion the way poolSFX does, but it does get this call
// TierUrgent priority over queued background attribution/
// characterization/direction-tagging work and visibility on the Jobs
// dashboard, same as every other test surface in this package.
func (s *Server) handleTestLLM(w http.ResponseWriter, r *http.Request) {
	var req testLLMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	userPrompt := strings.TrimSpace(req.UserPrompt)
	if userPrompt == "" {
		writeError(w, http.StatusBadRequest, "userPrompt is required")
		return
	}
	if s.TTS == nil {
		writeError(w, http.StatusServiceUnavailable, "tts worker not configured")
		return
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}

	text, err := s.Jobs.RunLLMPreview(r.Context(), userPrompt, func(ctx context.Context) (string, error) {
		return s.TTS.LLMGenerate(ctx, req.SystemPrompt, userPrompt, req.Temp, maxTokens)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}
