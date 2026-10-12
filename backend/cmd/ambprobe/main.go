// Command ambprobe measures a cheap "does the bed change?" verdict against
// the ambience pass's own describe calls: it scores sample chapters'
// music as the pass does (speakerattr.Client.ScoreMusic), and for every
// describe call that had a previous region asks the same user prompt for
// a one-word verdict instead - same, shift or new. Nothing is stored; the
// library is opened read-only.
//
//	go run ./cmd/ambprobe -db data/library.sqlite -n 24 -out /tmp/ambprobe.json
//
// The model is the running backend's, through POST /api/llm/test, like
// cmd/mattercheck.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

const verdictSystemPrompt = `You are the sound designer for an audiobook. Behind the narrator's voice plays a quiet background bed: an instrumental music underscore and, when the scene has a clear physical setting, an ambient soundscape of that place. You are given the bed that is playing now (the previous region) and the full text of the next stretch of the chapter. Decide whether the bed has to change for this stretch.

Answer with exactly one word:
- same: the scene carries on - the same people, the same place and moment, and the mood is close enough that the same music and ambience still fit.
- shift: still the same scene and place, but the mood has clearly moved (calm to tense, banter to grief), so the music should evolve while the ambience stays.
- new: a different scene - the story jumps in time or to another place, switches to other characters, something shocking erupts out of calm - or the characters have left the place the ambience describes.

Reply with only that one word, nothing else.`

// call is one describe call the pass made, and the verdict asked of the
// same prompt.
type call struct {
	Book     string  `json:"book"`
	Chapter  int     `json:"chapter"`
	User     string  `json:"user"`
	Describe string  `json:"describe"`
	Seconds  float64 `json:"seconds"`
	Verdict  string  `json:"verdict,omitempty"`
	VSeconds float64 `json:"verdictSeconds,omitempty"`
}

// llm answers through a backend's POST /api/llm/test, keeping what it was
// asked.
type llm struct {
	url   string
	mu    sync.Mutex
	calls []call
}

func (l *llm) ask(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	body, err := json.Marshal(map[string]any{"systemPrompt": systemPrompt, "userPrompt": userPrompt, "temp": temp, "maxTokens": maxTokens})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.url+"/api/llm/test", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm/test: %s: %s", resp.Status, raw)
	}
	var reply struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", err
	}
	return reply.Text, nil
}

func (l *llm) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	start := time.Now()
	text, err := l.ask(ctx, systemPrompt, userPrompt, temp, maxTokens)
	if err == nil {
		l.mu.Lock()
		l.calls = append(l.calls, call{User: userPrompt, Describe: text, Seconds: time.Since(start).Seconds()})
		l.mu.Unlock()
	}
	return text, err
}

var noVerdict bool

// region is one scored region, as the lighting sub-pass would group it.
type region struct {
	Book       string `json:"book"`
	Chapter    int    `json:"chapter"`
	StartIdx   int    `json:"startIdx"`
	Mood       string `json:"mood"`
	Prompt     string `json:"prompt"`
	Ambience   string `json:"ambience"`
	Transition string `json:"transition"`
	// Lit is whether the region has enough text for a lighting call.
	Lit bool `json:"lit"`
}

type chapter struct {
	id, bookID, bookTitle, title string
	idx                          int
}

func main() {
	dbPath := flag.String("db", "data/library.sqlite", "library.sqlite (opened read-only)")
	url := flag.String("url", "http://localhost:8080", "the running backend whose model to use")
	n := flag.Int("n", 24, "chapters to sample")
	seed := flag.Int("seed", 1, "which sample")
	workers := flag.Int("j", 4, "chapters at once")
	out := flag.String("out", "ambprobe.json", "where the calls go")
	flag.BoolVar(&noVerdict, "no-verdict", false, "skip the verdict calls")
	flag.Parse()

	db, err := sql.Open("sqlite3", "file:"+*dbPath+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Chapters with a summary (scenes are where regions start) and enough
	// text for several regions, at most two a book.
	rows, err := db.Query(`
		SELECT id, book_id, book_title, title, idx FROM (
			SELECT c.id, c.book_id, b.title AS book_title, c.title, c.idx, c.rowid AS r,
				row_number() OVER (PARTITION BY c.book_id ORDER BY (c.rowid * 7919 + ?) % 1009) AS k
			FROM chapters c JOIN books b ON b.id = c.book_id
			JOIN chapter_summaries s ON s.chapter_id = c.id
			JOIN chapter_text t ON t.chapter_id = c.id
			WHERE t.paragraphs BETWEEN 60 AND 400
		) WHERE k <= 2 ORDER BY (r * 104729 + ?) % 1013, id LIMIT ?`, *seed, *seed, *n)
	if err != nil {
		log.Fatal(err)
	}
	var chapters []chapter
	for rows.Next() {
		var c chapter
		if err := rows.Scan(&c.id, &c.bookID, &c.bookTitle, &c.title, &c.idx); err != nil {
			log.Fatal(err)
		}
		chapters = append(chapters, c)
	}
	rows.Close()

	var mu sync.Mutex
	var all []call
	var regions []region
	var wg sync.WaitGroup
	work := make(chan chapter)
	for range *workers {
		wg.Go(func() {
			for c := range work {
				calls, regs, err := probe(db, *url, c)
				if err != nil {
					log.Printf("%s ch %d: %v", c.bookTitle, c.idx, err)
				}
				mu.Lock()
				all = append(all, calls...)
				regions = append(regions, regs...)
				mu.Unlock()
				log.Printf("%s ch %d: %d describe calls", c.bookTitle, c.idx, len(calls))
			}
		})
	}
	for _, c := range chapters {
		work <- c
	}
	close(work)
	wg.Wait()

	raw, _ := json.MarshalIndent(struct {
		Calls   []call   `json:"calls"`
		Regions []region `json:"regions"`
	}{all, regions}, "", " ")
	if err := os.WriteFile(*out, raw, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", *out, len(all), "calls")
}

func probe(db *sql.DB, url string, c chapter) ([]call, []region, error) {
	var setting *speakerattr.BookSetting
	var raw string
	if err := db.QueryRow(`SELECT setting FROM book_settings WHERE book_id = ?`, c.bookID).Scan(&raw); err == nil {
		var s store.BookSetting
		if json.Unmarshal([]byte(raw), &s) == nil {
			bs := speakerattr.BookSetting(s)
			setting = &bs
		}
	}
	var notes *speakerattr.ChapterSummary
	if err := db.QueryRow(`SELECT summary FROM chapter_summaries WHERE chapter_id = ?`, c.id).Scan(&raw); err == nil {
		var n store.ChapterNotes
		if json.Unmarshal([]byte(raw), &n) == nil {
			notes = &speakerattr.ChapterSummary{Summary: n.Summary}
			for _, sc := range n.Scenes {
				notes.Scenes = append(notes.Scenes, speakerattr.Scene{StartIdx: sc.StartIdx, Location: sc.Location, Time: sc.Time, Present: sc.Present})
			}
		}
	}
	rows, err := db.Query(`SELECT idx, content, inline, is_quote FROM paragraphs WHERE chapter_id = ? ORDER BY idx`, c.id)
	if err != nil {
		return nil, nil, err
	}
	var paras []speakerattr.ParagraphInput
	for rows.Next() {
		var p speakerattr.ParagraphInput
		if err := rows.Scan(&p.Idx, &p.Text, &p.Inline, &p.IsQuote); err != nil {
			return nil, nil, err
		}
		paras = append(paras, p)
	}
	rows.Close()

	l := &llm{url: url}
	client := speakerattr.NewClient(speakerattr.ConfigFromEnv(), l)
	ctx := context.Background()
	scored, _, scoreErr := client.ScoreMusic(ctx, c.bookTitle, c.title, setting, notes, paras, nil)
	var regions []region
	for i, r := range scored {
		end := paras[len(paras)-1].Idx
		if i+1 < len(scored) {
			end = scored[i+1].StartIdx - 1
		}
		var span []speakerattr.ParagraphInput
		for _, p := range paras {
			if p.Idx >= r.StartIdx && p.Idx <= end {
				span = append(span, p)
			}
		}
		regions = append(regions, region{c.bookTitle, c.idx, r.StartIdx, r.Mood, r.Prompt, r.Ambience, r.Transition, speakerattr.NeedsLighting(span)})
	}

	for i := range l.calls {
		k := &l.calls[i]
		k.Book, k.Chapter = c.bookTitle, c.idx
		if noVerdict || !strings.Contains(k.User, "\nPrevious region:\n") {
			continue
		}
		start := time.Now()
		v, err := l.ask(ctx, verdictSystemPrompt, k.User, 0, 4)
		if err != nil {
			return l.calls, regions, err
		}
		k.Verdict, k.VSeconds = strings.TrimSpace(v), time.Since(start).Seconds()
	}
	return l.calls, regions, scoreErr
}
