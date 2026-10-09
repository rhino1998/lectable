package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeBackend answers the few routes the tests call, counting requests.
func fakeBackend(t *testing.T) (*server, *int) {
	t.Helper()
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/books", func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`[{"id":"aaa","title":"The Long Road","chapterCount":2},{"id":"bbb","title":"The Long Night","chapterCount":1}]`))
	})
	mux.HandleFunc("GET /api/books/{id}/chapters/{idx}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"title":"One","paragraphs":[
			{"idx":0,"text":"Rain fell.","audioStatus":"ready","words":[1,2,3]},
			{"idx":1,"text":"“Go,”","isQuote":true,"speaker":"Ana","emotion":"cold","audioStatus":"ready"},
			{"idx":2,"text":"she said.","inline":true,"audioStatus":"pending"},
			{"idx":3,"text":"“Why?”","isQuote":true,"audioStatus":"pending"}]}`))
	})
	mux.HandleFunc("PUT /api/books/{id}/title", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["title"] == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"title is blank","code":"blank"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &server{baseURL: ts.URL, outDir: t.TempDir()}, &requests
}

func callTool(t *testing.T, s *server, name, argsJSON string) (string, bool) {
	t.Helper()
	tl := findTool(name)
	if tl == nil {
		t.Fatalf("no tool %q", name)
	}
	res := s.call(tl, json.RawMessage(argsJSON))
	return res.Content[0].Text, res.IsError
}

func TestProtocol(t *testing.T) {
	s, _ := fakeBackend(t)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"routes","arguments":{"query":"GET /api/instance"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`,
	}, "\n")
	var out bytes.Buffer
	if err := serve(s, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d replies, want 4 (a notification gets none):\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"protocolVersion":"2024-11-05"`) {
		t.Errorf("initialize didn't agree to the client's version: %s", lines[0])
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Result.Tools) != len(tools) {
		t.Errorf("tools/list has %d tools, want %d", len(list.Result.Tools), len(tools))
	}
	for _, tl := range list.Result.Tools {
		if tl.InputSchema["type"] != "object" {
			t.Errorf("%s: input schema isn't an object", tl.Name)
		}
	}
	if !strings.Contains(lines[2], `"id":"x"`) || !strings.Contains(lines[2], "GET /api/instance") {
		t.Errorf("tools/call reply: %s", lines[2])
	}
	if !strings.Contains(lines[3], `"code":-32601`) {
		t.Errorf("unknown method reply: %s", lines[3])
	}
}

func TestResolveBook(t *testing.T) {
	s, requests := fakeBackend(t)
	if b, err := s.resolveBook("road"); err != nil || b.ID != "aaa" {
		t.Fatalf("road -> %v, %v", b.ID, err)
	}
	if b, err := s.resolveBook("bbb"); err != nil || b.Title != "The Long Night" {
		t.Fatalf("bbb -> %v, %v", b.Title, err)
	}
	if *requests != 1 {
		t.Errorf("listed the library %d times for two lookups, want 1", *requests)
	}
	_, err := s.resolveBook("long")
	if err == nil || !strings.Contains(err.Error(), "aaa") || !strings.Contains(err.Error(), "bbb") {
		t.Errorf("an ambiguous title should list its candidates, got %v", err)
	}
	if _, err := s.resolveBook("zzz"); err == nil {
		t.Error("no error for a title nothing matches")
	}
}

func TestRead(t *testing.T) {
	s, _ := fakeBackend(t)
	out, isErr := callTool(t, s, "read", `{"book":"road","chapter":0}`)
	if isErr {
		t.Fatal(out)
	}
	for _, want := range []string{"0 Rain fell.", "1 [Ana, cold] “Go,”", "2+ she said.", "3 [?] “Why?”", "2 pending, 2 ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	out, _ = callTool(t, s, "read", `{"book":"road","chapter":0,"from":1,"count":1}`)
	if strings.Contains(out, "Rain fell") || !strings.Contains(out, "continue with from=2") {
		t.Errorf("from/count:\n%s", out)
	}
	if out, isErr := callTool(t, s, "read", `{"book":"road","chaptr":0}`); !isErr {
		t.Errorf("a misspelled argument was accepted: %s", out)
	}
}

func TestAPICall(t *testing.T) {
	s, _ := fakeBackend(t)
	if out, isErr := callTool(t, s, "api_call", `{"method":"PUT","path":"/api/books/aaa/title","body":{"title":"New"}}`); isErr || !strings.HasPrefix(out, "204") {
		t.Errorf("rename: %s", out)
	}
	out, isErr := callTool(t, s, "api_call", `{"method":"PUT","path":"/api/books/aaa/title","body":{"title":""}}`)
	if !isErr || !strings.Contains(out, "title is blank (code blank)") {
		t.Errorf("a 400 should carry the backend's message and code: %s", out)
	}
	out, isErr = callTool(t, s, "api_call", `{"method":"POST","path":"/api/books/aaa/title"}`)
	if !isErr || !strings.Contains(out, "takes PUT") {
		t.Errorf("wrong method: %s", out)
	}
	if out, isErr := callTool(t, s, "api_get", `{"path":"/api/books/aaa/titel"}`); !isErr || !strings.Contains(out, "no route") {
		t.Errorf("unknown path: %s", out)
	}
	if out, isErr := callTool(t, s, "api_get", `{"path":"/api/books/aaa/title","body":{}}`); !isErr {
		t.Errorf("api_get took a body: %s", out)
	}
	out, isErr = callTool(t, s, "api_get", `{"path":"/api/books/aaa/chapters/0","select":"paragraphs.*.idx"}`)
	if isErr || strings.Join(strings.Fields(out), "") != "200OK[0,1,2,3]" {
		t.Errorf("select: %s", out)
	}
}

func TestFindRoutePrefersLiteral(t *testing.T) {
	r, _ := findRoute("POST", "/api/books/aaa/chapters/insert")
	if r == nil || r.Path != "/api/books/{id}/chapters/insert" {
		t.Errorf("got %+v", r)
	}
	if r, methods := findRoute("PATCH", "/api/books/aaa"); r != nil || len(methods) == 0 {
		t.Errorf("PATCH /api/books/aaa: route %v, other methods %v", r, methods)
	}
}

func TestRenderJSONShortens(t *testing.T) {
	var items []any
	for range 500 {
		items = append(items, map[string]any{"text": strings.Repeat("word ", 400)})
	}
	out := renderJSON(map[string]any{"items": items, "total": 500}, 3000)
	if len(out) > 3300 {
		t.Errorf("%d chars, want about 3000", len(out))
	}
	for _, want := range []string{`"total": 500`, "more (of 500)", "shortened to fit"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if _, err := selectPath(map[string]any{"a": []any{1.0}}, "a.b"); err == nil {
		t.Error("a field name stepping into an array should fail")
	}
}

func TestAround(t *testing.T) {
	text := strings.Repeat("é ", 300) + "needle" + strings.Repeat(" ü", 300)
	out := around(text, "NEEDLE", 80)
	if !strings.Contains(out, "needle") || strings.ContainsRune(out, '�') || len(out) > 100 {
		t.Errorf("got %q", out)
	}
}
