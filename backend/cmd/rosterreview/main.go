// Command rosterreview runs the roster review pass (httpapi.ReviewRoster)
// over a book's roster offline and prints what it would propose - for
// trying a change to its rules or prompt without touching the live library.
// It writes proposals to the library it is given, so give it a copy:
//
//	sqlite3 data/library.sqlite ".backup /tmp/library-copy.sqlite"
//	go run ./cmd/rosterreview -db /tmp/library-copy.sqlite -book <bookID> [-llm http://127.0.0.1:8091] [-v]
//
// Without -llm no model is called: each pair of names that would be read
// about is ruled out as unclear, and -v prints the prompt it would have
// been asked with. -llm is a running ttsworker, whose model it shares with
// whatever the job queue is doing.
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
	"strings"

	"github.com/rhino1998/lectable/backend/internal/httpapi"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// llm answers through a ttsworker's /llm/generate, or - with no url - with
// an empty verdict; it prints each exchange when verbose.
type llm struct {
	url     string
	verbose bool
}

func (l llm) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	out := `{"evidence": "", "verdict": "unclear"}`
	if l.url != "" {
		body, err := json.Marshal(ttsproto.LLMGenerateRequest{SystemPrompt: systemPrompt, UserPrompt: userPrompt, Temp: temp, MaxTokens: maxTokens})
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.url, "/")+"/llm/generate", bytes.NewReader(body))
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
			return "", fmt.Errorf("llm/generate: %s: %s", resp.Status, raw)
		}
		var reply ttsproto.LLMGenerateResponse
		if err := json.Unmarshal(raw, &reply); err != nil {
			return "", err
		}
		out = reply.Text
	}
	if l.verbose {
		fmt.Printf("---- prompt ----\n%s\n---- reply ----\n%s\n\n", userPrompt, out)
	}
	return out, nil
}

func main() {
	dbPath := flag.String("db", "", "a COPY of library.sqlite (proposals are written to it)")
	bookID := flag.String("book", "", "id of a book whose roster to review")
	llmURL := flag.String("llm", "", "a running ttsworker to ask (e.g. http://127.0.0.1:8091); none: no model is called")
	verbose := flag.Bool("v", false, "print each prompt and reply")
	flag.Parse()
	if *dbPath == "" || *bookID == "" {
		flag.Usage()
		log.Fatal("-db and -book are required")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	book, err := st.GetBook(*bookID)
	if err != nil || book == nil {
		log.Fatalf("book %s: not found (%v)", *bookID, err)
	}
	srv := &httpapi.Server{Store: st, Speaker: speakerattr.NewClient(speakerattr.Config{}, llm{url: *llmURL, verbose: *verbose})}
	if err := srv.ReviewRoster(context.Background(), book); err != nil {
		log.Fatal(err)
	}
	proposals, err := st.ListRosterProposals(store.SeriesScope(book))
	if err != nil {
		log.Fatal(err)
	}
	counts := map[string]int{}
	for _, p := range proposals {
		counts[string(p.Status)+" "+p.Kind+" ("+string(p.Source)+")"]++
		line := fmt.Sprintf("%-9s %-12s %-5s %q", p.Status, p.Kind, p.Source, p.Subject)
		if p.Target != "" {
			line += fmt.Sprintf(" -> %q", p.Target)
		}
		if p.Detail != "" {
			line += fmt.Sprintf(" [%s]", p.Detail)
		}
		fmt.Printf("%s: %s\n", line, p.Reason)
		for _, e := range p.Evidence {
			fmt.Printf("    “%s”\n", e)
		}
	}
	fmt.Println()
	for k, n := range counts {
		fmt.Printf("%4d %s\n", n, k)
	}
}
