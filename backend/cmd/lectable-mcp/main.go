// Command lectable-mcp is a Model Context Protocol server (stdio) that
// gives an agent working on this repo a compact way to look at and drive a
// running backend: find a book, read a chapter with its speakers, search
// the text, see the roster and the job queue, call any API route, query
// the library read-only, and read the server log.
//
// It exists because the raw API answers for clients, not for a context
// window: GET /api/jobs lists every queued task (tens of thousands of
// lines on a busy library), a chapter carries word timings for every
// paragraph, and finding a route's body shape meant reading its handler.
// Each tool here answers with what a reader needs and says what it left
// out.
//
// Tools (tools.go): books, book, read, search, speakers, jobs - summaries
// of the matching GET routes; routes - the route catalog apigen writes
// from internal/httpapi (routes_gen.go); api_get / api_call - any route,
// read-only and mutating apart so the first can be allowed without
// asking; sql - one read-only statement against library.sqlite; logs -
// the tail of the server log.
//
// Configuration, all optional: LECTABLE_URL (http://localhost:8080),
// LECTABLE_DATA_DIR (./data, for sql), LECTABLE_LOG_FILE
// (/tmp/lectable-server.log, for logs). ../../mcp.sh builds and runs it
// with the same defaults run.sh uses, and the repo's .mcp.json registers
// that with Claude Code as "lectable".
//
// The protocol is small enough to speak directly: newline-delimited
// JSON-RPC 2.0 on stdin/stdout, with initialize, ping, tools/list and
// tools/call. Nothing but replies may be written to stdout; diagnostics go
// to stderr.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// protocolVersion is the newest MCP revision this server was written
// against; a client asking for an older one it names gets that one back.
const protocolVersion = "2025-06-18"

var knownProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const instructions = `Tools for a running lectable backend (self-hosted audiobook reader).
Start with books to get a book id; every other tool's "book" takes that id or any unambiguous part of the title.
read/search/speakers/jobs are summaries - prefer them to api_get on the same data, which returns far more than fits.
For anything else, find the route with routes, then api_get (GET) or api_call (anything that changes state).
Merging or deleting characters, resetting passes and deleting audio can't be undone: ask before calling those.`

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("lectable-mcp: ")
	s := &server{
		baseURL: strings.TrimRight(envOr("LECTABLE_URL", "http://localhost:8080"), "/"),
		dbPath:  filepath.Join(envOr("LECTABLE_DATA_DIR", "data"), "library.sqlite"),
		logFile: envOr("LECTABLE_LOG_FILE", "/tmp/lectable-server.log"),
		outDir:  filepath.Join(os.TempDir(), "lectable-mcp"),
	}
	if err := serve(s, os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// serve answers requests from in until it closes. Requests are handled
// one at a time, in order: every tool is a short read or one HTTP call.
func serve(s *server, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			if err := enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error: " + err.Error()}}); err != nil {
				return err
			}
			continue
		}
		result, rerr := s.handle(req)
		if len(req.ID) == 0 { // a notification: never answered
			continue
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *server) handle(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := protocolVersion
		for _, v := range knownProtocolVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "lectable", "version": "1"},
			"instructions":    instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolList()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params: " + err.Error()}
		}
		t := findTool(p.Name)
		if t == nil {
			return nil, &rpcError{-32602, "unknown tool: " + p.Name}
		}
		return s.call(t, p.Arguments), nil
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil, nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

// call runs one tool. A tool's failure is a result the model reads
// (isError), not a protocol error.
func (s *server) call(t *tool, raw json.RawMessage) (res callResult) {
	defer func() {
		if r := recover(); r != nil {
			res = callResult{Content: []textContent{{"text", fmt.Sprintf("%s panicked: %v", t.Name, r)}}, IsError: true}
		}
	}()
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	text, err := t.run(s, raw)
	if err != nil {
		return callResult{Content: []textContent{{"text", err.Error()}}, IsError: true}
	}
	if text == "" {
		text = "(nothing)"
	}
	return callResult{Content: []textContent{{"text", text}}}
}
