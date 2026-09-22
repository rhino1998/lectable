package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/live"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsworker/ttsworkertest"
)

// newLiveTestServer is newTestServer plus a running live.Hub wired to the
// store's change hook, the same way cmd/server wires it.
func newLiveTestServer(t *testing.T) (*store.Store, *httptest.Server) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "library.duckdb"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	fake := ttsworkertest.New(t)
	dataDir := t.TempDir()
	tts := fake.Manager()
	mgr := jobs.NewManager(s, tts, dataDir, nil)

	hub := live.New(live.Config{Debounce: 10 * time.Millisecond})
	s.OnChange(func(tables []string) {
		for _, tb := range tables {
			hub.Invalidate(DBDep(tb))
		}
	})

	srv := &Server{
		Store:     s,
		TTS:       tts,
		Jobs:      mgr,
		DataDir:   dataDir,
		Narration: narration.NewResolver(s),
		Live:      hub,
	}
	ts := httptest.NewServer(NewRouter(srv))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	mgr.Start(ctx)
	go hub.Run(ctx)
	return s, ts
}

type liveMsg struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Data   json.RawMessage `json:"data"`
	Ops    json.RawMessage `json:"ops"`
	Status int             `json:"status"`
	Error  string          `json:"error"`
}

// liveConn reads messages on a background goroutine - a websocket Read
// whose context expires closes the connection, so "expect nothing within
// 300ms" can't be a timed Read.
type liveConn struct {
	conn *websocket.Conn
	msgs chan liveMsg
}

func dialLive(t *testing.T, ts *httptest.Server) *liveConn {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/events", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetReadLimit(1 << 20)
	t.Cleanup(func() { conn.CloseNow() })
	lc := &liveConn{conn: conn, msgs: make(chan liveMsg, 16)}
	go func() {
		defer close(lc.msgs)
		for {
			var m liveMsg
			if err := wsjson.Read(context.Background(), conn, &m); err != nil {
				return
			}
			lc.msgs <- m
		}
	}()
	return lc
}

func readLive(t *testing.T, lc *liveConn) liveMsg {
	t.Helper()
	select {
	case m, ok := <-lc.msgs:
		if !ok {
			t.Fatal("connection closed")
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a message")
	}
	return liveMsg{}
}

func expectNoLive(t *testing.T, lc *liveConn) {
	t.Helper()
	select {
	case m := <-lc.msgs:
		t.Fatalf("expected no message, got %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestLiveSubscribeSnapshotThenPatch(t *testing.T) {
	s, ts := newLiveTestServer(t)
	bookID := createTestBook(t, s, "", 0, "first", "second")
	conn := dialLive(t, ts)

	send := func(v any) {
		if err := wsjson.Write(t.Context(), conn.conn, v); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	send(map[string]any{"type": "subscribe", "id": "bm", "topic": "bookmarks", "params": map[string]any{"bookId": bookID}})
	m := readLive(t, conn)
	if m.Type != "snapshot" || m.ID != "bm" || string(m.Data) != "[]" {
		t.Fatalf("bookmarks snapshot: %+v (data %s)", m, m.Data)
	}

	send(map[string]any{"type": "subscribe", "id": "ch", "topic": "chapter", "params": map[string]any{"bookId": bookID, "chapterIdx": 0}})
	m = readLive(t, conn)
	if m.Type != "snapshot" || m.ID != "ch" {
		t.Fatalf("chapter snapshot: %+v", m)
	}
	var chapter chapterDetailDTO
	if err := json.Unmarshal(m.Data, &chapter); err != nil || len(chapter.Paragraphs) != 2 {
		t.Fatalf("chapter snapshot data: %v %s", err, m.Data)
	}

	send(map[string]any{"type": "subscribe", "id": "missing", "topic": "book", "params": map[string]any{"bookId": "nope"}})
	m = readLive(t, conn)
	if m.Type != "error" || m.ID != "missing" || m.Status != http.StatusNotFound {
		t.Fatalf("expected 404 error, got %+v", m)
	}

	send(map[string]any{"type": "subscribe", "id": "bad", "topic": "chapter", "params": map[string]any{"bookId": bookID}})
	m = readLive(t, conn)
	if m.Type != "error" || m.ID != "bad" || m.Status != http.StatusBadRequest {
		t.Fatalf("expected 400 error, got %+v", m)
	}

	// A write through the REST API reaches the bookmarks subscriber as a
	// patch, and only that subscriber - the chapter topic doesn't depend on
	// the bookmarks table.
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/bookmarks", createBookmarkRequest{ChapterIdx: 0, ParagraphIdx: 1, Note: "n"}, nil)
	resp.Body.Close()
	m = readLive(t, conn)
	if m.Type != "patch" || m.ID != "bm" || !strings.Contains(string(m.Ops), `"note":"n"`) {
		t.Fatalf("bookmarks patch: %+v (ops %s)", m, m.Ops)
	}
	expectNoLive(t, conn)

	// A write straight to the store (the background-job path, no HTTP
	// request involved) patches just the changed paragraph field.
	ch, err := s.GetChapterByIdx(bookID, 0)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := s.GetParagraphIDByIdx(ch.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphSFXPrompt(pid, "thunder"); err != nil {
		t.Fatal(err)
	}
	m = readLive(t, conn)
	if m.Type != "patch" || m.ID != "ch" {
		t.Fatalf("chapter patch: %+v", m)
	}
	var ops []struct {
		Op    string `json:"op"`
		Path  []any  `json:"path"`
		Value any    `json:"value"`
	}
	if err := json.Unmarshal(m.Ops, &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Op != "set" || ops[0].Value != "thunder" ||
		len(ops[0].Path) != 3 || ops[0].Path[0] != "paragraphs" || ops[0].Path[1] != float64(1) || ops[0].Path[2] != "sfxPrompt" {
		t.Fatalf("unexpected chapter ops: %s", m.Ops)
	}

	// Unsubscribed topics go quiet.
	send(map[string]any{"type": "unsubscribe", "id": "ch"})
	time.Sleep(50 * time.Millisecond)
	if err := s.SetParagraphSFXPrompt(pid, "rain"); err != nil {
		t.Fatal(err)
	}
	expectNoLive(t, conn)
}
