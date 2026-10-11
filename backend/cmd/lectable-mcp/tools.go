package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type server struct {
	baseURL string
	dbPath  string
	logFile string
	outDir  string // where a route's file response is saved

	// The library as last listed, for resolving a "book" argument: the
	// list costs the backend seconds on a busy library.
	bookCache   []bookSummary
	bookCacheAt time.Time
}

// bookCacheTTL is how long resolveBook trusts the last listing. A
// reference it can't find there is looked up afresh.
const bookCacheTTL = 2 * time.Minute

type tool struct {
	Name        string
	Description string
	Schema      map[string]any
	ReadOnly    bool
	run         func(s *server, raw json.RawMessage) (string, error)
}

// defaultMaxChars bounds a tool's answer unless the call says otherwise.
const defaultMaxChars = 16000

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func schema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

const bookArg = "A book's id, or any part of its title that matches only one book"

var (
	selectProp   = prop("string", `Dot path into the JSON answer, to return only that part: a field name, an array index, or * for every element - "chapters.*.title", "queued.0"`)
	maxCharsProp = prop("integer", fmt.Sprintf("Longest answer to return (default %d); long arrays and strings are shortened to fit", defaultMaxChars))
	queryProp    = map[string]any{"type": "object", "description": `Query parameters, e.g. {"scope": "rest"}`, "additionalProperties": true}
)

var tools = []tool{
	{
		Name:        "books",
		Description: "List the library: each book's id, title, author, series, chapter count, how much is narrated and read, and whether it is preprocessing or came from a website. Start here to get a book id.",
		Schema:      schema(nil, map[string]any{"query": prop("string", "Only books whose title, author or series contains this")}),
		ReadOnly:    true,
		run:         runBooks,
	},
	{
		Name:        "book",
		Description: "One book's chapter list: each chapter's index, title, paragraph count, how many have audio, and which LLM passes (attribution, summary, ambience, ...) have run on it.",
		Schema: schema([]string{"book"}, map[string]any{
			"book":  prop("string", bookArg),
			"from":  prop("integer", "First chapter index to list (default 0)"),
			"count": prop("integer", "How many chapters to list (default 150)"),
		}),
		ReadOnly: true,
		run:      runBook,
	},
	{
		Name:        "read",
		Description: `Read a chapter's text, one paragraph per line as "<idx> [speaker, emotion] text" (no brackets for narration, [?] for unattributed dialogue; "+" after the index marks a segment of the same printed paragraph as the line before). Use this, not api_get, to read the book.`,
		Schema: schema([]string{"book", "chapter"}, map[string]any{
			"book":      prop("string", bookArg),
			"chapter":   prop("integer", "Chapter index, from 0"),
			"from":      prop("integer", "First paragraph index to show (default 0)"),
			"count":     prop("integer", "How many paragraphs to show (default: as many as fit)"),
			"max_chars": prop("integer", fmt.Sprintf("Longest answer to return (default %d); the answer says where to continue from", defaultMaxChars)),
		}),
		ReadOnly: true,
		run:      runRead,
	},
	{
		Name:        "search",
		Description: "Find where a phrase occurs in a book's text (a substring match, case-insensitive, up to 200 hits): chapter and paragraph index plus the text around it. With meaning, finds the paragraphs closest to the query in meaning instead, best first, for a book that has been embedded. Follow up with read for context.",
		Schema: schema([]string{"book", "query"}, map[string]any{
			"book":    prop("string", bookArg),
			"query":   prop("string", "The phrase to look for, matched as one substring"),
			"limit":   prop("integer", "How many hits to show (default 40)"),
			"meaning": prop("boolean", "Search by meaning rather than for the exact phrase: describe what you are looking for"),
		}),
		ReadOnly: true,
		run:      runSearch,
	},
	{
		Name:        "speakers",
		Description: "A book's character roster (shared by its series), most lines first: character id, name, line count, aliases, pronouns, role/invalid flags and assigned voice.",
		Schema: schema([]string{"book"}, map[string]any{
			"book":      prop("string", bookArg),
			"query":     prop("string", "Only characters whose name or an alias contains this"),
			"min_lines": prop("integer", "Leave out characters with fewer lines than this"),
			"limit":     prop("integer", "How many to list (default 80)"),
			"summaries": prop("boolean", "Also show each character's voice characterization"),
		}),
		ReadOnly: true,
		run:      runSpeakers,
	},
	{
		Name:        "jobs",
		Description: "The job queue at a glance: paused or not, what is running, how much is queued by kind and by book, and the estimated work left. GET /api/jobs itself lists every queued task - use this instead.",
		Schema: schema(nil, map[string]any{
			"book": prop("string", bookArg+" - show only its tasks, and list its next queued ones"),
		}),
		ReadOnly: true,
		run:      runJobs,
	},
	{
		Name:        "routes",
		Description: "Search the backend's HTTP routes (generated from the handlers, so always current): method, path, query params, the JSON body's shape, response type, error codes and the handler's doc. Few matches are shown in full, many as a list. No query lists every route.",
		Schema: schema(nil, map[string]any{
			"query":  prop("string", `Words that must all appear in the method, path, handler name or doc - "roster-review", "PUT speaker", "import url"`),
			"detail": prop("boolean", "Show every match in full however many there are"),
		}),
		ReadOnly: true,
		run:      runRoutes,
	},
	{
		Name:        "api_get",
		Description: "GET any backend route and return its JSON, shortened to fit (use select to pick a part). A file response (audio, cover, epub) is saved to a temp file and its path returned. Changes nothing. Find routes with the routes tool; prefer books/book/read/search/speakers/jobs where they cover it.",
		Schema: schema([]string{"path"}, map[string]any{
			"path":      prop("string", `The route's path with its parameters filled in - "/api/books/<id>/voice" (also "/metrics")`),
			"query":     queryProp,
			"select":    selectProp,
			"grep":      prop("string", "For a plain-text answer (/metrics): keep only lines matching this regular expression"),
			"max_chars": maxCharsProp,
		}),
		ReadOnly: true,
		run:      runAPIGet,
	},
	{
		Name:        "api_call",
		Description: "Call a backend route that changes something (POST, PUT, DELETE): queue a pass, fix a speaker, file a roster proposal, pause the queue. Check the body's shape with the routes tool first. Some calls can't be undone (merging or deleting a character, resetting a pass, deleting audio or a book) - confirm those with the user before calling.",
		Schema: schema([]string{"method", "path"}, map[string]any{
			"method":    map[string]any{"type": "string", "enum": []string{"POST", "PUT", "DELETE", "PATCH"}},
			"path":      prop("string", `The route's path with its parameters filled in - "/api/books/<id>/chapters/3/summarize"`),
			"query":     queryProp,
			"body":      map[string]any{"description": "The JSON body, as an object (not a string)"},
			"files":     map[string]any{"type": "object", "description": `For an upload route: multipart file field -> local file path, e.g. {"file": "/tmp/book.epub"}`, "additionalProperties": map[string]any{"type": "string"}},
			"select":    selectProp,
			"max_chars": maxCharsProp,
		}),
		run: runAPICall,
	},
	{
		Name:        "sql",
		Description: "Run one read-only SQL statement against the library database (SQLite) - for what no route answers: counts, joins, text across books. With no query, lists the tables and their columns. The connection cannot write. paragraphs.content is the text, paragraphs.speaker the attributed name; a roster's scope is 'series:<name>' or 'book:<id>'.",
		Schema: schema(nil, map[string]any{
			"query":    prop("string", "A SELECT (or WITH/PRAGMA/EXPLAIN) statement; omit to see the schema"),
			"max_rows": prop("integer", "Rows to show (default 50)"),
			"max_cell": prop("integer", "Longest value to show in one cell, in characters (default 200)"),
		}),
		ReadOnly: true,
		run:      runSQL,
	},
	{
		Name:        "logs",
		Description: "The end of the backend's log (server and ttsworker both write to it), optionally only lines matching a regular expression - errors, a job key, \"timing\", \"live: slow build\".",
		Schema: schema(nil, map[string]any{
			"grep":     prop("string", `Regular expression a line must match (RE2; "(?i)" for case-insensitive)`),
			"lines":    prop("integer", "How many lines to show, from the end (default 60)"),
			"previous": prop("boolean", "Read the previous run's log instead (the one before the last restart)"),
		}),
		ReadOnly: true,
		run:      runLogs,
	},
}

func findTool(name string) *tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func toolList() []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.Schema,
			"annotations": map[string]any{"readOnlyHint": t.ReadOnly, "destructiveHint": !t.ReadOnly, "openWorldHint": false},
		})
	}
	return out
}

// args decodes a tool's arguments, refusing a name the tool doesn't take
// so a misspelled one isn't silently ignored.
func args[T any](raw json.RawMessage) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("bad arguments: %w", err)
	}
	return v, nil
}

// ---- HTTP ----

var httpClient = &http.Client{Timeout: 10 * time.Minute}

type apiResponse struct {
	status int
	header http.Header
	body   []byte
}

func (s *server) request(method, path string, query map[string]any, body any, files map[string]string) (*apiResponse, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path must start with / (got %q)", path)
	}
	u := s.baseURL + path
	if len(query) > 0 {
		q := url.Values{}
		for k, v := range query {
			q.Set(k, fmt.Sprint(v))
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		u += sep + q.Encode()
	}
	var rd io.Reader
	contentType := ""
	switch {
	case len(files) > 0:
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for field, p := range files {
			f, err := os.Open(p)
			if err != nil {
				return nil, err
			}
			w, err := mw.CreateFormFile(field, filepath.Base(p))
			if err == nil {
				_, err = io.Copy(w, f)
			}
			f.Close()
			if err != nil {
				return nil, err
			}
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		rd, contentType = &buf, mw.FormDataContentType()
	case body != nil:
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd, contentType = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backend unreachable at %s (is it running? `make deploy-backend` from the repo root starts it): %w", s.baseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &apiResponse{resp.StatusCode, resp.Header, raw}, nil
}

// failure is a non-2xx answer as an error: the status plus the backend's
// own message and code.
func (r *apiResponse) failure(method, path string) error {
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	msg := strings.TrimSpace(string(r.body))
	if json.Unmarshal(r.body, &e) == nil && e.Error != "" {
		msg = e.Error
		if e.Code != "" {
			msg += " (code " + e.Code + ")"
		}
	}
	return fmt.Errorf("%s %s: %d %s: %s", method, path, r.status, http.StatusText(r.status), clip(msg, 2000))
}

func (s *server) getJSON(path string, query map[string]any, out any) error {
	resp, err := s.request(http.MethodGet, path, query, nil, nil)
	if err != nil {
		return err
	}
	if resp.status/100 != 2 {
		return resp.failure(http.MethodGet, path)
	}
	return json.Unmarshal(resp.body, out)
}

// ---- books ----

type bookSummary struct {
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	Author           string  `json:"author"`
	SeriesName       string  `json:"seriesName"`
	SeriesIndex      float64 `json:"seriesIndex"`
	ChapterCount     int     `json:"chapterCount"`
	PosChapterIdx    int     `json:"posChapterIdx"`
	PosParagraphIdx  int     `json:"posParagraphIdx"`
	ProgressPercent  float64 `json:"progressPercent"`
	Finished         bool    `json:"finished"`
	WordCount        int64   `json:"wordCount"`
	GeneratedPercent float64 `json:"generatedPercent"`
	Preprocessing    bool    `json:"preprocessing"`
	Preprocessed     bool    `json:"preprocessed"`
	Source           *struct {
		Kind           string `json:"kind"`
		URL            string `json:"url"`
		RemoteChapters int    `json:"remoteChapters"`
		Syncing        bool   `json:"syncing"`
	} `json:"source"`
}

func (b bookSummary) line() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s  %s", b.ID, b.Title)
	if b.Author != "" {
		sb.WriteString(" - " + b.Author)
	}
	if b.SeriesName != "" {
		fmt.Fprintf(&sb, "  [%s #%g]", b.SeriesName, b.SeriesIndex)
	}
	fmt.Fprintf(&sb, "\n    %d chapters, %dk words, narrated %.0f%%, read %.0f%%", b.ChapterCount, b.WordCount/1000, b.GeneratedPercent, b.ProgressPercent)
	if b.ProgressPercent > 0 && !b.Finished {
		fmt.Fprintf(&sb, " (at chapter %d ¶%d)", b.PosChapterIdx, b.PosParagraphIdx)
	}
	switch {
	case b.Preprocessing:
		sb.WriteString(", preprocessing now")
	case b.Preprocessed:
		sb.WriteString(", preprocessed")
	}
	if b.Source != nil {
		fmt.Fprintf(&sb, ", from %s (%s)", b.Source.Kind, b.Source.URL)
		if b.Source.Syncing {
			sb.WriteString(", syncing")
		}
	}
	return sb.String()
}

func (s *server) books() ([]bookSummary, error) {
	var books []bookSummary
	if err := s.getJSON("/api/books", nil, &books); err != nil {
		return nil, err
	}
	s.bookCache, s.bookCacheAt = books, time.Now()
	return books, nil
}

// resolveBook finds the book named by a tool's "book" argument: its id,
// its exact title, or the one book whose title contains the text.
func (s *server) resolveBook(ref string) (bookSummary, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return bookSummary{}, errors.New("book is required: an id or part of a title (the books tool lists them)")
	}
	if time.Since(s.bookCacheAt) < bookCacheTTL {
		if b, n := matchBook(s.bookCache, ref); n == 1 {
			return b[0], nil
		}
	}
	books, err := s.books()
	if err != nil {
		return bookSummary{}, err
	}
	partial, n := matchBook(books, ref)
	switch n {
	case 1:
		return partial[0], nil
	case 0:
		return bookSummary{}, fmt.Errorf("no book with id or title matching %q (the books tool lists all %d)", ref, len(books))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%q matches %d books - use an id:", ref, len(partial))
	for _, b := range partial {
		fmt.Fprintf(&sb, "\n  %s  %s", b.ID, b.Title)
	}
	return bookSummary{}, errors.New(sb.String())
}

// matchBook is the books ref names: the one with that id or exact title,
// else every one whose title contains it.
func matchBook(books []bookSummary, ref string) ([]bookSummary, int) {
	var partial []bookSummary
	for _, b := range books {
		if b.ID == ref || strings.EqualFold(b.Title, ref) {
			return []bookSummary{b}, 1
		}
		if strings.Contains(strings.ToLower(b.Title), strings.ToLower(ref)) {
			partial = append(partial, b)
		}
	}
	return partial, len(partial)
}

func runBooks(s *server, raw json.RawMessage) (string, error) {
	a, err := args[struct {
		Query string `json:"query"`
	}](raw)
	if err != nil {
		return "", err
	}
	books, err := s.books()
	if err != nil {
		return "", err
	}
	q := strings.ToLower(a.Query)
	var lines []string
	for _, b := range books {
		if q == "" || strings.Contains(strings.ToLower(b.Title+"\n"+b.Author+"\n"+b.SeriesName), q) {
			lines = append(lines, b.line())
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("no book matches %q (of %d)", a.Query, len(books)), nil
	}
	return strings.Join(lines, "\n"), nil
}

func runBook(s *server, raw json.RawMessage) (string, error) {
	a, err := args[struct {
		Book  string `json:"book"`
		From  int    `json:"from"`
		Count int    `json:"count"`
	}](raw)
	if err != nil {
		return "", err
	}
	b, err := s.resolveBook(a.Book)
	if err != nil {
		return "", err
	}
	var detail struct {
		Chapters []struct {
			Idx              int             `json:"idx"`
			Title            string          `json:"title"`
			ParagraphCount   int             `json:"paragraphCount"`
			ReadyCount       int             `json:"readyCount"`
			Passes           map[string]bool `json:"passes"`
			MusicRegionCount int             `json:"musicRegionCount"`
			MusicReadyCount  int             `json:"musicReadyCount"`
		} `json:"chapters"`
	}
	if err := s.getJSON("/api/books/"+b.ID, nil, &detail); err != nil {
		return "", err
	}
	if a.Count <= 0 {
		a.Count = 150
	}
	from := min(max(a.From, 0), len(detail.Chapters))
	to := min(from+a.Count, len(detail.Chapters))
	var sb strings.Builder
	sb.WriteString(b.line() + "\n\n")
	sb.WriteString("idx  title | paragraphs (with audio) | passes run\n")
	for _, c := range detail.Chapters[from:to] {
		var done, missing []string
		for name, ok := range c.Passes {
			if ok {
				done = append(done, name)
			} else {
				missing = append(missing, name)
			}
		}
		passes := strings.Join(sortedStrings(done), ",")
		switch {
		case len(done) == 0:
			passes = "none"
		case len(missing) == 0:
			passes = "all"
		case len(missing) <= 2:
			passes = "all but " + strings.Join(sortedStrings(missing), ",")
		}
		fmt.Fprintf(&sb, "%d  %s | %d (%d) | %s", c.Idx, oneLine(c.Title, 80), c.ParagraphCount, c.ReadyCount, passes)
		if c.MusicRegionCount > 0 {
			fmt.Fprintf(&sb, " | music %d/%d", c.MusicReadyCount, c.MusicRegionCount)
		}
		sb.WriteByte('\n')
	}
	if to < len(detail.Chapters) {
		fmt.Fprintf(&sb, "… %d more chapters - continue with from=%d", len(detail.Chapters)-to, to)
	}
	return sb.String(), nil
}

// ---- read ----

func runRead(s *server, raw json.RawMessage) (string, error) {
	a, err := args[struct {
		Book     string `json:"book"`
		Chapter  *int   `json:"chapter"`
		From     int    `json:"from"`
		Count    int    `json:"count"`
		MaxChars int    `json:"max_chars"`
	}](raw)
	if err != nil {
		return "", err
	}
	if a.Chapter == nil {
		return "", errors.New("chapter is required: a chapter index (the book tool lists them)")
	}
	b, err := s.resolveBook(a.Book)
	if err != nil {
		return "", err
	}
	var ch struct {
		Title      string `json:"title"`
		Generating bool   `json:"generating"`
		Paragraphs []struct {
			Idx          int    `json:"idx"`
			Text         string `json:"text"`
			AudioStatus  string `json:"audioStatus"`
			AudioError   string `json:"audioError"`
			Speaker      string `json:"speaker"`
			SpeakerIndex int    `json:"speakerIndex"`
			Label        string `json:"label"`
			Inline       bool   `json:"inline"`
			IsQuote      bool   `json:"isQuote"`
			ScareQuote   bool   `json:"scareQuote"`
			Emotion      string `json:"emotion"`
		} `json:"paragraphs"`
	}
	if err := s.getJSON(fmt.Sprintf("/api/books/%s/chapters/%d", b.ID, *a.Chapter), nil, &ch); err != nil {
		return "", err
	}
	if a.MaxChars <= 0 {
		a.MaxChars = defaultMaxChars
	}
	status := map[string]int{}
	for _, p := range ch.Paragraphs {
		status[p.AudioStatus]++
	}
	var counts []string
	for k, n := range status {
		counts = append(counts, fmt.Sprintf("%d %s", n, k))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s - chapter %d %q: %d paragraphs (audio: %s)", b.Title, *a.Chapter, ch.Title, len(ch.Paragraphs), strings.Join(sortedStrings(counts), ", "))
	if ch.Generating {
		sb.WriteString(", generating")
	}
	sb.WriteString("\n\n")
	shown := 0
	for _, p := range ch.Paragraphs {
		if p.Idx < a.From {
			continue
		}
		if a.Count > 0 && shown >= a.Count {
			fmt.Fprintf(&sb, "… continue with from=%d", p.Idx)
			break
		}
		var line strings.Builder
		fmt.Fprintf(&line, "%d", p.Idx)
		if p.Inline {
			line.WriteByte('+')
		}
		line.WriteByte(' ')
		switch {
		case p.IsQuote:
			who := p.Speaker
			if who == "" || who == "Unknown" {
				who = "?"
			}
			if p.SpeakerIndex > 0 {
				who += fmt.Sprintf(" %d", p.SpeakerIndex)
			}
			if p.Emotion != "" {
				who += ", " + p.Emotion
			}
			line.WriteString("[" + who + "] ")
		case p.ScareQuote:
			line.WriteString("[scare quote] ")
		}
		if p.Label != "" {
			line.WriteString("{" + p.Label + "} ")
		}
		line.WriteString(strings.Join(strings.Fields(p.Text), " "))
		if p.AudioStatus == "error" {
			line.WriteString("  !audio error: " + oneLine(p.AudioError, 200))
		}
		if shown > 0 && sb.Len()+line.Len() > a.MaxChars {
			fmt.Fprintf(&sb, "… cut to fit %d chars - continue with from=%d", a.MaxChars, p.Idx)
			break
		}
		sb.WriteString(line.String() + "\n")
		shown++
	}
	return sb.String(), nil
}

// ---- search ----

func runSearch(s *server, raw json.RawMessage) (string, error) {
	a, err := args[struct {
		Book    string `json:"book"`
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
		Meaning bool   `json:"meaning"`
	}](raw)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", errors.New("query is required")
	}
	b, err := s.resolveBook(a.Book)
	if err != nil {
		return "", err
	}
	var hits []struct {
		ChapterIdx   int    `json:"chapterIdx"`
		ChapterTitle string `json:"chapterTitle"`
		ParagraphIdx int    `json:"paragraphIdx"`
		Text         string `json:"text"`
	}
	query := map[string]any{"q": a.Query}
	if a.Meaning {
		query["mode"] = "meaning"
	}
	if err := s.getJSON("/api/books/"+b.ID+"/search", query, &hits); err != nil {
		return "", err
	}
	if len(hits) == 0 {
		if a.Meaning {
			return fmt.Sprintf("nothing found in %s: its chapters have not been embedded (a preprocessing phase)", b.Title), nil
		}
		return fmt.Sprintf("%q does not occur in %s", a.Query, b.Title), nil
	}
	if a.Limit <= 0 {
		a.Limit = 40
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d hits for %q in %s (chapter:paragraph)\n", len(hits), a.Query, b.Title)
	lastChapter := -1
	for i, h := range hits {
		if i == a.Limit {
			fmt.Fprintf(&sb, "… %d more - raise limit or make the phrase longer\n", len(hits)-i)
			break
		}
		if h.ChapterIdx != lastChapter {
			fmt.Fprintf(&sb, "-- chapter %d %q\n", h.ChapterIdx, h.ChapterTitle)
			lastChapter = h.ChapterIdx
		}
		fmt.Fprintf(&sb, "%d:%d %s\n", h.ChapterIdx, h.ParagraphIdx, around(h.Text, a.Query, 280))
	}
	return sb.String(), nil
}

// around is text cut to about n bytes centred on its first match of q.
func around(text, q string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= n {
		return text
	}
	at := strings.Index(strings.ToLower(text), strings.ToLower(q))
	start := max(at-n/2, 0)
	if at < 0 || len(strings.ToLower(text)) != len(text) {
		start = 0
	}
	end := min(start+n, len(text))
	runes := []rune(text[start:end]) // drops a rune split at either edge
	for len(runes) > 0 && runes[0] == '�' {
		runes = runes[1:]
	}
	for len(runes) > 0 && runes[len(runes)-1] == '�' {
		runes = runes[:len(runes)-1]
	}
	out := string(runes)
	if start > 0 {
		out = "…" + out
	}
	if end < len(text) {
		out += "…"
	}
	return out
}

// ---- speakers ----

func runSpeakers(s *server, raw json.RawMessage) (string, error) {
	a, err := args[struct {
		Book      string `json:"book"`
		Query     string `json:"query"`
		MinLines  int    `json:"min_lines"`
		Limit     int    `json:"limit"`
		Summaries bool   `json:"summaries"`
	}](raw)
	if err != nil {
		return "", err
	}
	b, err := s.resolveBook(a.Book)
	if err != nil {
		return "", err
	}
	var speakers []struct {
		ID             string   `json:"id"`
		Name           string   `json:"name"`
		VoicePresetID  string   `json:"voicePresetId"`
		Summary        string   `json:"summary"`
		ParagraphCount int      `json:"paragraphCount"`
		ReadyCount     int      `json:"readyCount"`
		Invalid        bool     `json:"invalid"`
		Role           bool     `json:"role"`
		Aliases        []string `json:"aliases"`
		Pronouns       string   `json:"pronouns"`
	}
	if err := s.getJSON("/api/books/"+b.ID+"/speakers", nil, &speakers); err != nil {
		return "", err
	}
	sort.SliceStable(speakers, func(i, j int) bool { return speakers[i].ParagraphCount > speakers[j].ParagraphCount })
	if a.Limit <= 0 {
		a.Limit = 80
	}
	q := strings.ToLower(a.Query)
	var sb strings.Builder
	scope := "book:" + b.ID
	if b.SeriesName != "" {
		scope = "series:" + b.SeriesName
	}
	fmt.Fprintf(&sb, "%s - %d speakers (roster scope %s; lines are this book's)\n", b.Title, len(speakers), scope)
	shown, left := 0, 0
	for _, sp := range speakers {
		if sp.ParagraphCount < a.MinLines || (q != "" && !strings.Contains(strings.ToLower(sp.Name+"\n"+strings.Join(sp.Aliases, "\n")), q)) {
			continue
		}
		if shown == a.Limit {
			left++
			continue
		}
		shown++
		fmt.Fprintf(&sb, "%s - %d lines", sp.Name, sp.ParagraphCount)
		if sp.ID != "" {
			sb.WriteString(", id " + sp.ID)
		}
		if sp.Role {
			sb.WriteString(", role")
		}
		if sp.Invalid {
			sb.WriteString(", invalid")
		}
		if sp.Pronouns != "" {
			sb.WriteString(", " + sp.Pronouns)
		}
		if len(sp.Aliases) > 0 {
			sb.WriteString(", aka " + strings.Join(sp.Aliases, " / "))
		}
		if sp.VoicePresetID != "" {
			sb.WriteString(", voice " + sp.VoicePresetID)
		}
		sb.WriteByte('\n')
		if a.Summaries && sp.Summary != "" {
			sb.WriteString("    " + oneLine(sp.Summary, 400) + "\n")
		}
	}
	if left > 0 {
		fmt.Fprintf(&sb, "… %d more with fewer lines - raise limit, or filter with query/min_lines\n", left)
	}
	return sb.String(), nil
}

// ---- jobs ----

type queueTask struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	BookID       string `json:"bookId"`
	BookTitle    string `json:"bookTitle"`
	ChapterIdx   int    `json:"chapterIdx"`
	ChapterTitle string `json:"chapterTitle"`
	ParagraphIdx int    `json:"paragraphIdx"`
	Tier         string `json:"tier"`
	Attempt      int    `json:"attempt"`
	Remaining    int    `json:"remaining"`
}

func (t queueTask) line() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s [%s] %s", t.Kind, t.Tier, oneLine(t.BookTitle, 50))
	switch {
	case t.Label != "":
		sb.WriteString(" - " + t.Label)
	case strings.HasPrefix(t.Kind, "pipeline"):
	default:
		fmt.Fprintf(&sb, " ch %d", t.ChapterIdx)
		if t.ParagraphIdx > 0 {
			fmt.Fprintf(&sb, " ¶%d", t.ParagraphIdx)
		}
	}
	if t.Remaining > 0 {
		fmt.Fprintf(&sb, " (%d left)", t.Remaining)
	}
	if t.Attempt > 0 {
		fmt.Fprintf(&sb, " (attempt %d)", t.Attempt+1)
	}
	return sb.String() + "  id " + t.ID
}

func duration(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
	return fmt.Sprintf("%.0fs", d.Seconds())
}

// topCounts renders a tally as "name n, name n", largest first.
func topCounts(counts map[string]int, limit int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	var parts []string
	for i, k := range keys {
		if i == limit {
			parts = append(parts, fmt.Sprintf("… %d more", len(keys)-i))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

func runJobs(s *server, raw json.RawMessage) (string, error) {
	a, err := args[struct {
		Book string `json:"book"`
	}](raw)
	if err != nil {
		return "", err
	}
	bookID := ""
	if a.Book != "" {
		b, err := s.resolveBook(a.Book)
		if err != nil {
			return "", err
		}
		bookID = b.ID
	}
	var snap struct {
		InFlight      []queueTask `json:"inFlight"`
		Queued        []queueTask `json:"queued"`
		QueuedTotal   int         `json:"queuedTotal"`
		Paused        bool        `json:"paused"`
		Drained       bool        `json:"drained"`
		NarrationLeft []struct {
			BookTitle string `json:"bookTitle"`
			Chars     int64  `json:"chars"`
		} `json:"narrationLeft"`
		NarrationCharsPerSecond     float64 `json:"narrationCharsPerSecond"`
		LastNarrationCharsPerSecond float64 `json:"lastNarrationCharsPerSecond"`
		WorkLeft                    []struct {
			Kind           string  `json:"kind"`
			Tasks          int     `json:"tasks"`
			TypicalSeconds float64 `json:"typicalSeconds"`
			Seconds        float64 `json:"seconds"`
		} `json:"workLeft"`
	}
	if err := s.getJSON("/api/jobs", nil, &snap); err != nil {
		return "", err
	}
	keep := func(ts []queueTask) []queueTask {
		if bookID == "" {
			return ts
		}
		var out []queueTask
		for _, t := range ts {
			if t.BookID == bookID {
				out = append(out, t)
			}
		}
		return out
	}
	inFlight, queued := keep(snap.InFlight), keep(snap.Queued)

	var sb strings.Builder
	switch {
	case snap.Paused && snap.Drained:
		sb.WriteString("queue: PAUSED and drained (models unloaded) - POST /api/jobs/resume to continue\n")
	case snap.Paused:
		sb.WriteString("queue: PAUSED, in-flight work still finishing - POST /api/jobs/resume to continue\n")
	default:
		sb.WriteString("queue: running\n")
	}
	if rate := snap.NarrationCharsPerSecond; rate > 0 {
		fmt.Fprintf(&sb, "narrating at %.0f chars/s\n", rate)
	} else if snap.LastNarrationCharsPerSecond > 0 {
		fmt.Fprintf(&sb, "not narrating now (last rate %.0f chars/s)\n", snap.LastNarrationCharsPerSecond)
	}

	const listLimit = 25
	fmt.Fprintf(&sb, "\nin flight: %d\n", len(inFlight))
	for i, t := range inFlight {
		if i == listLimit {
			fmt.Fprintf(&sb, "  … %d more\n", len(inFlight)-i)
			break
		}
		sb.WriteString("  " + t.line() + "\n")
	}

	byKind, byBook := map[string]int{}, map[string]int{}
	for _, t := range queued {
		byKind[t.Kind+"/"+t.Tier]++
		byBook[oneLine(t.BookTitle, 50)]++
	}
	total := snap.QueuedTotal
	if bookID != "" {
		total = len(queued)
	}
	fmt.Fprintf(&sb, "\nqueued: %d\n", total)
	if len(queued) < total {
		fmt.Fprintf(&sb, "  (the tallies below cover the first %d the backend lists)\n", len(queued))
	}
	if len(queued) > 0 {
		sb.WriteString("  by kind/tier: " + topCounts(byKind, 20) + "\n")
		if bookID == "" {
			sb.WriteString("  by book: " + topCounts(byBook, 12) + "\n")
		}
		next := 5
		if bookID != "" {
			next = listLimit
		}
		sb.WriteString("  next up:\n")
		for i, t := range queued {
			if i == next {
				break
			}
			sb.WriteString("    " + t.line() + "\n")
		}
	}

	if bookID == "" {
		if len(snap.WorkLeft) > 0 {
			sb.WriteString("\nwork left (whole queue):\n")
			var sum float64
			for _, w := range snap.WorkLeft {
				if w.TypicalSeconds == 0 {
					fmt.Fprintf(&sb, "  %s: %d tasks, no timing yet\n", w.Kind, w.Tasks)
					continue
				}
				fmt.Fprintf(&sb, "  %s: %d tasks, ~%s (%.0fs each)\n", w.Kind, w.Tasks, duration(w.Seconds), w.TypicalSeconds)
				sum += w.Seconds
			}
			fmt.Fprintf(&sb, "  total ~%s of the timed kinds\n", duration(sum))
		}
		if len(snap.NarrationLeft) > 0 {
			sb.WriteString("\nnarration queued, by book:\n")
			for _, n := range snap.NarrationLeft {
				fmt.Fprintf(&sb, "  %s: %dk chars\n", oneLine(n.BookTitle, 90), n.Chars/1000)
			}
		}
	}
	return sb.String(), nil
}

// ---- routes, api_get, api_call ----

func runRoutes(_ *server, raw json.RawMessage) (string, error) {
	a, err := args[struct {
		Query  string `json:"query"`
		Detail bool   `json:"detail"`
	}](raw)
	if err != nil {
		return "", err
	}
	return searchRoutes(a.Query, a.Detail), nil
}

type apiArgs struct {
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Query    map[string]any    `json:"query"`
	Body     any               `json:"body"`
	Files    map[string]string `json:"files"`
	Select   string            `json:"select"`
	Grep     string            `json:"grep"`
	MaxChars int               `json:"max_chars"`
}

func runAPIGet(s *server, raw json.RawMessage) (string, error) {
	a, err := args[apiArgs](raw)
	if err != nil {
		return "", err
	}
	if a.Method != "" || a.Body != nil || len(a.Files) > 0 {
		return "", errors.New("api_get only reads (GET, no body) - use api_call to change something")
	}
	a.Method = http.MethodGet
	return s.callAPI(a)
}

func runAPICall(s *server, raw json.RawMessage) (string, error) {
	a, err := args[apiArgs](raw)
	if err != nil {
		return "", err
	}
	a.Method = strings.ToUpper(a.Method)
	switch a.Method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
	case http.MethodGet:
		return "", errors.New("use api_get for a GET")
	default:
		return "", fmt.Errorf("method must be POST, PUT, DELETE or PATCH (got %q)", a.Method)
	}
	if a.Grep != "" {
		return "", errors.New("grep is api_get's")
	}
	return s.callAPI(a)
}

func (s *server) callAPI(a apiArgs) (string, error) {
	bare, _, _ := strings.Cut(a.Path, "?")
	// Checked against the route table first: a mistyped path would
	// otherwise come back as a bare 404 with nothing to go on.
	if strings.HasPrefix(bare, "/api/") {
		route, others := findRoute(a.Method, bare)
		switch {
		case route == nil && len(others) > 0:
			return "", fmt.Errorf("%s isn't served for %s - that path takes %s (see the routes tool)", a.Method, bare, strings.Join(others, ", "))
		case route == nil:
			return "", fmt.Errorf("no route %s %s - find the right one with the routes tool", a.Method, bare)
		case len(route.Files) > 0 && len(a.Files) == 0 && a.Body != nil:
			return "", fmt.Errorf("%s takes an upload, not a JSON body: pass files {%q: <path>}", route.key(), route.Files[0])
		case len(a.Files) > 0 && len(route.Files) == 0:
			return "", fmt.Errorf("%s takes no upload", route.key())
		}
	}
	if a.MaxChars <= 0 {
		a.MaxChars = defaultMaxChars
	}
	resp, err := s.request(a.Method, a.Path, a.Query, a.Body, a.Files)
	if err != nil {
		return "", err
	}
	if resp.status/100 != 2 {
		return "", resp.failure(a.Method, a.Path)
	}
	head := fmt.Sprintf("%d %s", resp.status, http.StatusText(resp.status))
	if len(resp.body) == 0 {
		return head, nil
	}
	mediaType, _, _ := mime.ParseMediaType(resp.header.Get("Content-Type"))
	switch {
	case mediaType == "application/json":
		var v any
		if err := json.Unmarshal(resp.body, &v); err != nil {
			return "", fmt.Errorf("%s answered JSON that doesn't parse: %w", a.Path, err)
		}
		v, err := selectPath(v, a.Select)
		if err != nil {
			return "", fmt.Errorf("select %q: %w", a.Select, err)
		}
		return head + "\n" + renderJSON(v, a.MaxChars), nil
	case strings.HasPrefix(mediaType, "text/"):
		text := string(resp.body)
		if a.Grep != "" {
			re, err := regexp.Compile(a.Grep)
			if err != nil {
				return "", fmt.Errorf("grep: %w", err)
			}
			var kept []string
			for line := range strings.SplitSeq(text, "\n") {
				if re.MatchString(line) {
					kept = append(kept, line)
				}
			}
			text = strings.Join(kept, "\n")
		}
		return head + "\n" + clip(text, a.MaxChars), nil
	}
	path, err := s.saveFile(bare, mediaType, resp.body)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s\n%s, %d bytes, saved to %s", head, mediaType, len(resp.body), path), nil
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// saveFile keeps a route's file response where the caller can read or
// play it, named after the route so the same request overwrites itself.
func (s *server) saveFile(path, mediaType string, body []byte) (string, error) {
	if err := os.MkdirAll(s.outDir, 0o755); err != nil {
		return "", err
	}
	name := strings.Trim(unsafeFileChars.ReplaceAllString(strings.TrimPrefix(path, "/api/"), "_"), "_")
	ext := ""
	switch mediaType {
	case "audio/ogg":
		ext = ".opus"
	case "audio/wav", "audio/x-wav", "audio/wave":
		ext = ".wav"
	case "application/epub+zip":
		ext = ".epub"
	case "image/jpeg":
		ext = ".jpg"
	default:
		if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
			ext = exts[0]
		}
	}
	if !strings.HasSuffix(name, ext) {
		name += ext
	}
	out := filepath.Join(s.outDir, name)
	return out, os.WriteFile(out, body, 0o644)
}

// ---- sql ----

const sqlTimeout = 60 * time.Second

func runSQL(s *server, raw json.RawMessage) (string, error) {
	a, err := args[struct {
		Query   string `json:"query"`
		MaxRows int    `json:"max_rows"`
		MaxCell int    `json:"max_cell"`
	}](raw)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(s.dbPath); err != nil {
		return "", fmt.Errorf("no library database at %s (set LECTABLE_DATA_DIR): %w", s.dbPath, err)
	}
	// mode=ro opens the file read-only and query_only refuses writes at
	// the statement level, so no statement given here can change the
	// library whatever it says.
	db, err := sql.Open("sqlite3", "file:"+s.dbPath+"?mode=ro&_query_only=1&_busy_timeout=5000")
	if err != nil {
		return "", err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sqlTimeout)
	defer cancel()
	if strings.TrimSpace(a.Query) == "" {
		return sqlSchema(ctx, db)
	}
	if a.MaxRows <= 0 {
		a.MaxRows = 50
	}
	if a.MaxCell <= 0 {
		a.MaxCell = 200
	}
	rows, err := db.QueryContext(ctx, a.Query)
	if err != nil {
		return "", fmt.Errorf("%w%s", err, sqlHint(err))
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString(strings.Join(cols, "\t") + "\n")
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		n++
		if n > a.MaxRows {
			continue // counted, not shown
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		for i, v := range vals {
			if i > 0 {
				sb.WriteByte('\t')
			}
			sb.WriteString(sqlCell(v, a.MaxCell))
		}
		sb.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("%w%s", err, sqlHint(err))
	}
	switch {
	case n == 0:
		sb.WriteString("(no rows)")
	case n > a.MaxRows:
		fmt.Fprintf(&sb, "… %d more rows (%d in all) - add a LIMIT/WHERE, or raise max_rows", n-a.MaxRows, n)
	default:
		fmt.Fprintf(&sb, "(%d rows)", n)
	}
	return sb.String(), nil
}

func sqlHint(err error) string {
	switch msg := err.Error(); {
	case strings.Contains(msg, "readonly") || strings.Contains(msg, "read-only"):
		return " - this connection is read-only; change the library through api_call"
	case strings.Contains(msg, "no such function"):
		return " - the backend's own SQL functions (fold, rowhash, bit_xor) aren't registered here"
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf(" - gave up after %s; scope it by book or chapter", sqlTimeout)
	}
	return ""
}

func sqlCell(v any, maxLen int) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return oneLine(string(t), maxLen)
	case string:
		return oneLine(t, maxLen)
	case time.Time:
		return t.Format(time.RFC3339)
	}
	return fmt.Sprint(v)
}

// sqlSchema lists every table with its columns, one line each.
func sqlSchema(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.name, group_concat(p.name || CASE WHEN p.type = '' THEN '' ELSE ' ' || lower(p.type) END, ', ')
		FROM sqlite_master m JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND m.name NOT LIKE 'paragraphs_fts_%'
		GROUP BY m.name ORDER BY m.name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var name, cols string
		if err := rows.Scan(&name, &cols); err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, "%s(%s)\n", name, cols)
	}
	return sb.String(), rows.Err()
}

// ---- logs ----

// logTailBytes is how far back from the end of the log a call reads.
const logTailBytes = 16 << 20

func runLogs(s *server, raw json.RawMessage) (string, error) {
	a, err := args[struct {
		Grep     string `json:"grep"`
		Lines    int    `json:"lines"`
		Previous bool   `json:"previous"`
	}](raw)
	if err != nil {
		return "", err
	}
	path := s.logFile
	if a.Previous {
		path += ".1"
	}
	if a.Lines <= 0 {
		a.Lines = 60
	}
	var re *regexp.Regexp
	if a.Grep != "" {
		if re, err = regexp.Compile(a.Grep); err != nil {
			return "", fmt.Errorf("grep: %w", err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w (set LECTABLE_LOG_FILE if the server logs elsewhere)", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := max(st.Size()-logTailBytes, 0)
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // the first is cut mid-line
	}
	if re != nil {
		kept := lines[:0]
		for _, l := range lines {
			if re.MatchString(l) {
				kept = append(kept, l)
			}
		}
		lines = kept
	}
	matched := len(lines)
	if matched > a.Lines {
		lines = lines[matched-a.Lines:]
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%.1f MB, written %s)", path, float64(st.Size())/(1<<20), st.ModTime().Format("2006-01-02 15:04:05"))
	switch {
	case matched == 0 && re != nil:
		fmt.Fprintf(&sb, ": no line matches %q", a.Grep)
	case matched > len(lines):
		fmt.Fprintf(&sb, ": last %d of %d", len(lines), matched)
		if re != nil {
			sb.WriteString(" matching lines")
		}
	}
	sb.WriteByte('\n')
	for _, l := range lines {
		sb.WriteString(clip(l, 600) + "\n")
	}
	return sb.String(), nil
}
