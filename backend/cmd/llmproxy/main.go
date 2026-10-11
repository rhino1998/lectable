// Command llmproxy serves the OpenAI chat-completions wire format in front
// of a running lectable backend's own LLM, so tools that only speak that
// format (DuckDB's ai/flock extensions, anything with an "OpenAI-compatible
// base URL" setting) can use the model ttsworker already has loaded instead
// of a second llama-server holding its own copy in VRAM.
//
// It talks to cmd/server's POST /api/llm/test, never to ttsworker itself:
// that route runs each call as a pooled job (jobs.Manager.RunLLMPreview),
// which is what takes turns with the audio models for the GPU. A call sent
// straight to the worker would load the LLM behind the scheduler's back.
// Those jobs are TierUrgent, so a bulk query through here goes ahead of
// queued background passes for as long as it runs.
//
// Usage:
//
//	go run ./cmd/llmproxy                    # 127.0.0.1:8092 -> http://localhost:8080
//	go run ./cmd/llmproxy -addr 127.0.0.1:9000
//
// Then, in DuckDB:
//
//	SET duckdb_ai_provider = 'llamacpp';
//	SET duckdb_ai_base_url = 'http://127.0.0.1:8092/v1';
//
// Configuration: LECTABLE_URL (http://localhost:8080).
//
// What it doesn't do: embeddings (the worker loads a chat model and
// llamacpp-go extracts none - POST /v1/embeddings answers 501), token
// streaming (a stream:true call gets the whole reply as one chunk), and
// response_format/tools (ignored: the reply is whatever the model wrote).
// The worker takes one system and one user turn, so a longer conversation
// is flattened into the user turn as a transcript.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// modelID is the one model GET /v1/models lists. Whatever a request names
// is accepted and ignored: the backend has a single LLM.
const modelID = "lectable"

// defaultMaxTokens caps a reply whose request names no limit - the
// backend's own default for /api/llm/test.
const defaultMaxTokens = 2048

func main() {
	addr := flag.String("addr", "127.0.0.1:8092", "address to listen on")
	flag.Parse()

	p := &proxy{
		baseURL: strings.TrimRight(envOr("LECTABLE_URL", "http://localhost:8080"), "/"),
		// No timeout: a call waits its turn in the backend's job queue,
		// and for the model to load.
		client: &http.Client{},
	}
	log.Printf("llmproxy listening on %s -> %s", *addr, p.baseURL)
	if err := http.ListenAndServe(*addr, p.routes()); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type proxy struct {
	baseURL string
	client  *http.Client
}

func (p *proxy) routes() http.Handler {
	mux := http.NewServeMux()
	// Clients disagree on whether a base URL ends in /v1, so both work.
	for _, prefix := range []string{"", "/v1"} {
		mux.HandleFunc("GET "+prefix+"/models", handleModels)
		mux.HandleFunc("POST "+prefix+"/chat/completions", p.handleChat)
		mux.HandleFunc("POST "+prefix+"/embeddings", handleEmbeddings)
	}
	return mux
}

// chatRequest is the part of an OpenAI chat-completions body this proxy
// reads.
type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Temperature         float32       `json:"temperature"`
	MaxTokens           int           `json:"max_tokens"`
	MaxCompletionTokens int           `json:"max_completion_tokens"`
	Stream              bool          `json:"stream"`
}

type chatMessage struct {
	Role    string      `json:"role"`
	Content chatContent `json:"content"`
}

// chatContent is a message's text, sent either as a plain string or as a
// list of typed parts, of which only the text ones are kept.
type chatContent string

func (c *chatContent) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = chatContent(s)
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		if string(b) == "null" {
			return nil
		}
		return errors.New("content must be a string or a list of parts")
	}
	var texts []string
	for _, part := range parts {
		if part.Type == "text" || part.Type == "" {
			texts = append(texts, part.Text)
		}
	}
	*c = chatContent(strings.Join(texts, "\n"))
	return nil
}

// prompts turns a conversation into the one system and one user turn the
// worker takes. System (and developer) messages are joined into the system
// prompt. A single user message is the user prompt as written; anything
// longer becomes a transcript, each turn under its speaker.
func prompts(messages []chatMessage) (system, user string) {
	var systems []string
	var turns []chatMessage
	for _, m := range messages {
		if strings.TrimSpace(string(m.Content)) == "" {
			continue
		}
		switch m.Role {
		case "system", "developer":
			systems = append(systems, string(m.Content))
		default:
			turns = append(turns, m)
		}
	}
	system = strings.Join(systems, "\n\n")
	if len(turns) == 1 {
		return system, string(turns[0].Content)
	}
	var b strings.Builder
	for i, m := range turns {
		if i > 0 {
			b.WriteString("\n\n")
		}
		speaker := "User"
		if m.Role == "assistant" {
			speaker = "Assistant"
		}
		b.WriteString(speaker + ": " + string(m.Content))
	}
	return system, b.String()
}

func (p *proxy) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	system, user := prompts(req.Messages)
	if user == "" {
		writeError(w, http.StatusBadRequest, "messages must include a user message")
		return
	}
	maxTokens := req.MaxCompletionTokens
	if maxTokens <= 0 {
		maxTokens = req.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	text, err := p.generate(r.Context(), system, user, req.Temperature, maxTokens)
	if err != nil {
		log.Printf("llmproxy: %v", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": modelID,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]string{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": modelID,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		// The backend doesn't report token counts.
		"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
}

// generate runs one completion through the backend's POST /api/llm/test.
func (p *proxy) generate(ctx context.Context, system, user string, temp float32, maxTokens int) (string, error) {
	body, err := json.Marshal(map[string]any{
		"systemPrompt": system,
		"userPrompt":   user,
		"temp":         temp,
		"maxTokens":    maxTokens,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/llm/test", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("backend unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading backend reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("backend answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decoding backend reply: %w", err)
	}
	return out.Text, nil
}

func handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []map[string]string{{"id": modelID, "object": "model", "owned_by": "lectable"}},
	})
}

func handleEmbeddings(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, "no embedding model: the lectable worker loads a chat model only")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers in OpenAI's error shape, which is what its clients
// print.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": "lectable_error"},
	})
}
