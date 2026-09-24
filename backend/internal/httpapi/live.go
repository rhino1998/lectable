package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/rhino1998/lectable/backend/internal/live"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// DepJobs is the live invalidation name for jobs.Manager's in-memory queue
// state (the jobs snapshot itself, plus chapterDetailDTO.Generating and
// bookSummaryDTO.Preprocessing, both read straight off it) - cmd/server
// forwards jobs.Manager.SubscribeChanges to it.
const DepJobs = "jobs"

// DBDep is the live invalidation name for writes to one store table - see
// store.Store.OnChange, which cmd/server forwards through this.
func DBDep(table string) string {
	if table == store.AllTables {
		return live.AnyDep
	}
	return "db:" + table
}

func dbDeps(tables ...string) []string {
	out := make([]string, len(tables))
	for i, t := range tables {
		out[i] = DBDep(t)
	}
	return out
}

// narrationDeps covers everything a paragraph's effective voice (and so its
// audio status/URL, and every ready-count/progress figure derived from
// those) can depend on - see internal/narration.Resolver.
var narrationDeps = dbDeps("books", "chapters", "paragraphs", "paragraph_audio", "characters", "character_voices", "voice_presets", "default_voice")

func withDeps(base []string, extra ...string) []string {
	return append(append([]string(nil), base...), extra...)
}

// registerLiveTopics exposes each GET endpoint the frontend used to poll as
// a live topic (GET /api/events - see package live) built by the exact same
// build* function, so a snapshot and the REST response can never drift.
// Topic names and params:
//
//	books                                 GET /api/books
//	book           {bookId}               GET /api/books/{id}
//	chapter        {bookId, chapterIdx}   GET /api/books/{id}/chapters/{idx}
//	chapterMusic   {bookId, chapterIdx}   GET /api/books/{id}/chapters/{idx}/music
//	voice          {bookId}               GET /api/books/{id}/voice
//	speakers       {bookId}               GET /api/books/{id}/speakers
//	characterAppearances  {bookId, characterId}
//	characterDescriptions {bookId, characterId}
//	bookmarks      {bookId}               GET /api/books/{id}/bookmarks
//	jobs                                  GET /api/jobs
//	voicePresets                          GET /api/voices/presets
//	customVoicePresets                    GET /api/voices/custom-presets
//	defaultVoice                          GET /api/voices/default
//
// MinInterval is set on the topics whose build scans a whole book (or the
// whole library), so steady background generation - a write every couple
// of seconds per paragraph - can't keep the single DuckDB connection busy
// rebuilding them (see internal/store's own doc comment on why one slow
// query stalls everything).
func (s *Server) registerLiveTopics(h *live.Hub) {
	// Only books/book show the saved reading position (see
	// store.PositionTable).
	h.Register("books", noParams("books", withDeps(narrationDeps, DBDep(store.PositionTable), DepJobs), time.Second, s.buildBooks))
	h.Register("book", bookTopic("book", withDeps(narrationDeps, DBDep(store.PositionTable), DBDep("music_regions"), DepJobs), 500*time.Millisecond, s.buildBook))
	h.Register("chapter", chapterTopic("chapter", withDeps(narrationDeps, DBDep("images"), DBDep("breaks"), DBDep("sfx"), DepJobs), 0, s.buildChapter))
	h.Register("chapterMusic", chapterTopic("chapterMusic", withDeps(dbDeps("books", "chapters", "music_regions"), DepJobs), 0, s.buildChapterMusic))
	h.Register("voice", bookTopic("voice", dbDeps("books"), 0, s.buildVoice))
	// DepJobs too: an emotion variant finishing rendering (a
	// KindVoiceProvision task) changes a row's Emotions status without any
	// DB write of its own.
	h.Register("speakers", bookTopic("speakers", withDeps(narrationDeps, DepJobs), 2*time.Second, s.buildSpeakers))
	h.Register("characterAppearances", characterTopic("characterAppearances", narrationDeps, 2*time.Second, s.buildCharacterAppearances))
	h.Register("characterDescriptions", characterTopic("characterDescriptions", narrationDeps, 2*time.Second, s.buildCharacterDescriptions))
	h.Register("bookmarks", bookTopic("bookmarks", dbDeps("bookmarks", "books", "chapters", "paragraphs"), 0, s.buildBookmarks))
	h.Register("jobs", noParams("jobs", withDeps(dbDeps("books", "chapters"), DepJobs), 250*time.Millisecond, func() (any, error) {
		return s.buildJobsSnapshot(), nil
	}))
	// Built-in presets are compiled in, so nothing ever invalidates this -
	// it's a topic anyway so the frontend has one way to read everything.
	h.Register("voicePresets", noParams("voicePresets", nil, 0, s.buildVoicePresets))
	h.Register("customVoicePresets", noParams("customVoicePresets", dbDeps("voice_presets", "character_voices", "characters", "books"), 0, s.buildCustomVoicePresets))
	h.Register("defaultVoice", noParams("defaultVoice", dbDeps("default_voice"), 0, s.buildDefaultVoice))
}

// handleEvents is GET /api/events - the live-state WebSocket (package
// live's protocol, topics per registerLiveTopics).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.Live.ServeWS(w, r, wsAcceptOptions(s.AllowOrigin))
}

func noParams(kind string, deps []string, minInterval time.Duration, build func() (any, error)) live.Resolver {
	return func(json.RawMessage) (*live.Topic, error) {
		return &live.Topic{Key: kind, Deps: deps, MinInterval: minInterval, Build: build}, nil
	}
}

type topicParams struct {
	BookID      string `json:"bookId"`
	ChapterIdx  *int   `json:"chapterIdx"`
	CharacterID string `json:"characterId"`
}

func parseTopicParams(raw json.RawMessage) (topicParams, error) {
	var p topicParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("invalid params: %w", err)
		}
	}
	if p.BookID == "" {
		return p, fmt.Errorf("bookId is required")
	}
	return p, nil
}

func bookTopic(kind string, deps []string, minInterval time.Duration, build func(bookID string) (any, error)) live.Resolver {
	return func(raw json.RawMessage) (*live.Topic, error) {
		p, err := parseTopicParams(raw)
		if err != nil {
			return nil, err
		}
		return &live.Topic{
			Key: kind + ":" + p.BookID, Deps: deps, MinInterval: minInterval,
			Build: func() (any, error) { return build(p.BookID) },
		}, nil
	}
}

func chapterTopic(kind string, deps []string, minInterval time.Duration, build func(bookID string, idx int) (any, error)) live.Resolver {
	return func(raw json.RawMessage) (*live.Topic, error) {
		p, err := parseTopicParams(raw)
		if err != nil {
			return nil, err
		}
		if p.ChapterIdx == nil || *p.ChapterIdx < 0 {
			return nil, fmt.Errorf("chapterIdx is required")
		}
		idx := *p.ChapterIdx
		return &live.Topic{
			Key: kind + ":" + p.BookID + ":" + strconv.Itoa(idx), Deps: deps, MinInterval: minInterval,
			Build: func() (any, error) { return build(p.BookID, idx) },
		}, nil
	}
}

func characterTopic(kind string, deps []string, minInterval time.Duration, build func(bookID, characterID string) (any, error)) live.Resolver {
	return func(raw json.RawMessage) (*live.Topic, error) {
		p, err := parseTopicParams(raw)
		if err != nil {
			return nil, err
		}
		if p.CharacterID == "" {
			return nil, fmt.Errorf("characterId is required")
		}
		return &live.Topic{
			Key: kind + ":" + p.BookID + ":" + p.CharacterID, Deps: deps, MinInterval: minInterval,
			Build: func() (any, error) { return build(p.BookID, p.CharacterID) },
		}, nil
	}
}

// wsAcceptOptions mirrors withCORS's origin policy for WebSocket upgrades:
// restrict to AllowOrigin's host when one is configured, or skip the check
// entirely when it's unset - consistent with withCORS itself not setting
// any CORS headers (and so not restricting anything) in that case. This
// app has no auth and is meant for single-user local/LAN use, so the
// exposure from a permissive origin check is the same as the REST API's.
func wsAcceptOptions(allowOrigin string) *websocket.AcceptOptions {
	if allowOrigin == "" {
		return &websocket.AcceptOptions{InsecureSkipVerify: true}
	}
	if u, err := url.Parse(allowOrigin); err == nil && u.Host != "" {
		return &websocket.AcceptOptions{OriginPatterns: []string{u.Host}}
	}
	return &websocket.AcceptOptions{InsecureSkipVerify: true}
}
