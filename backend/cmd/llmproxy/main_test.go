package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeBackend stands in for cmd/server's POST /api/llm/test, recording the
// last body it was sent.
func fakeBackend(t *testing.T, status int, reply string) (*httptest.Server, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/llm/test" {
			t.Errorf("backend got %s, want /api/llm/test", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("backend body: %v", err)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

func TestChatCompletion(t *testing.T) {
	backend, got := fakeBackend(t, http.StatusOK, `{"text":"angry"}`)
	p := &proxy{baseURL: backend.URL, client: backend.Client()}

	rec := post(t, p.routes(), "/v1/chat/completions", `{
		"model": "anything",
		"temperature": 0.2,
		"max_tokens": 16,
		"messages": [
			{"role": "system", "content": "Label the line."},
			{"role": "user", "content": [{"type": "text", "text": "Get out!"}]}
		]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Choices []struct {
			Message struct{ Role, Content string }
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "angry" || out.Choices[0].Message.Role != "assistant" {
		t.Errorf("reply = %s", rec.Body)
	}
	if (*got)["systemPrompt"] != "Label the line." || (*got)["userPrompt"] != "Get out!" || (*got)["maxTokens"] != float64(16) {
		t.Errorf("backend was sent %v", *got)
	}
}

func TestChatCompletionStream(t *testing.T) {
	backend, _ := fakeBackend(t, http.StatusOK, `{"text":"hello"}`)
	p := &proxy{baseURL: backend.URL, client: backend.Client()}

	rec := post(t, p.routes(), "/chat/completions", `{"stream": true, "messages": [{"role": "user", "content": "hi"}]}`)
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hello"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream = %q", body)
	}
}

func TestChatCompletionErrors(t *testing.T) {
	backend, _ := fakeBackend(t, http.StatusBadGateway, `{"error":"ttsworker unavailable"}`)
	p := &proxy{baseURL: backend.URL, client: backend.Client()}

	if rec := post(t, p.routes(), "/v1/chat/completions", `{"messages": [{"role": "system", "content": "x"}]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no user message: status %d", rec.Code)
	}
	rec := post(t, p.routes(), "/v1/chat/completions", `{"messages": [{"role": "user", "content": "hi"}]}`)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "ttsworker unavailable") {
		t.Errorf("backend failure: status %d: %s", rec.Code, rec.Body)
	}
	if rec := post(t, p.routes(), "/v1/embeddings", `{"input": "x"}`); rec.Code != http.StatusNotImplemented {
		t.Errorf("embeddings: status %d", rec.Code)
	}
}

func TestPrompts(t *testing.T) {
	system, user := prompts([]chatMessage{
		{Role: "system", Content: "A."},
		{Role: "developer", Content: "B."},
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: "three"},
	})
	if system != "A.\n\nB." {
		t.Errorf("system = %q", system)
	}
	if want := "User: one\n\nAssistant: two\n\nUser: three"; user != want {
		t.Errorf("user = %q, want %q", user, want)
	}
}
