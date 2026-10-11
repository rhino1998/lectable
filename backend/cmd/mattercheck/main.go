// Command mattercheck runs the matter pass (speakerattr.Client.FindMatter)
// over the books of a library and prints which chapters it would mark as
// front or back matter - for trying a change to its prompt or guard
// against real books without marking anything. It only reads, but opening
// a library applies pending migrations, so give it a copy:
//
//	sqlite3 data/library.sqlite ".backup /tmp/library-copy.sqlite"
//	go run ./cmd/mattercheck -db /tmp/library-copy.sqlite [-book <bookID>] [-url http://localhost:8080] [-v]
//
// One line per chapter, tab-separated: book id, chapter index, "matter" or
// "story", the chapter's length, its title. Books from websites are left
// out, as the pass leaves them out.
//
// The model is the running backend's, through POST /api/llm/test: each
// call is a pooled job there, taking its turn with narration, never a call
// to ttsworker behind the scheduler's back.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// llm answers through a backend's POST /api/llm/test.
type llm struct {
	url     string
	verbose bool
}

func (l llm) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
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
	if l.verbose {
		fmt.Fprintf(os.Stderr, "---- prompt ----\n%s\n---- reply ----\n%s\n\n", userPrompt, reply.Text)
	}
	return reply.Text, nil
}

func main() {
	dbPath := flag.String("db", "", "a COPY of library.sqlite")
	bookID := flag.String("book", "", "only this book")
	url := flag.String("url", "http://localhost:8080", "the running backend whose model to use")
	workers := flag.Int("j", 4, "books asked about at once")
	verbose := flag.Bool("v", false, "print each prompt and reply to stderr")
	flag.Parse()
	if *dbPath == "" {
		log.Fatal("-db is required (a copy of the library, never the live one)")
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	client := speakerattr.NewClient(speakerattr.ConfigFromEnv(), llm{url: *url, verbose: *verbose})

	ids := []string{*bookID}
	if *bookID == "" {
		// Every book the pass itself would take: none from a website, and
		// in a fresh copy of a library, none yet judged.
		if ids, err = s.BooksNeedingMatterPass(); err != nil {
			log.Fatal(err)
		}
	}

	var mu sync.Mutex // one book's lines together
	var wg sync.WaitGroup
	work := make(chan string)
	for range *workers {
		wg.Go(func() {
			for id := range work {
				chapters, err := s.MatterSections(id)
				if err != nil {
					log.Fatal(err)
				}
				sections := make([]speakerattr.MatterSection, len(chapters))
				for i, c := range chapters {
					sections[i] = speakerattr.MatterSection{Title: c.Title, Chars: c.Chars, Opening: speakerattr.MatterOpening(c.Title, c.Opening)}
				}
				matter, err := client.FindMatter(context.Background(), sections)
				if err != nil {
					log.Printf("book %s: %v", id, err)
					continue
				}
				mu.Lock()
				for i, c := range chapters {
					mark := "story"
					if matter[i] {
						mark = "matter"
					}
					fmt.Printf("%s\t%d\t%s\t%d\t%s\n", id, c.Idx, mark, c.Chars, c.Title)
				}
				mu.Unlock()
			}
		})
	}
	for _, id := range ids {
		work <- id
	}
	close(work)
	wg.Wait()
}
