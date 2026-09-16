// benchattr is a throwaway benchmarking tool comparing speakerattr's
// default (hybrid-thinking) prompting against the NoThink variant on real
// chapters from an existing library.duckdb copy. Not wired into cmd/server
// or any build; delete this directory when done.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/rhino1998/lectable/backend/internal/llmworker"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
)

type chapter struct {
	id, title string
	idx       int
}

func main() {
	dbPath := flag.String("db", "", "path to a read-only copy of library.duckdb")
	bookID := flag.String("book", "", "book id")
	numChapters := flag.Int("chapters", 3, "number of chapters from the start")
	startIdx := flag.Int("start-idx", 0, "chapter idx to start from")
	modelPath := flag.String("model", "", "path to gguf model")
	gpuLayers := flag.Int("gpu-layers", -1, "NGPULayers")
	nctx := flag.Uint("nctx", 24576, "context size")
	label := flag.String("label", "run", "label for this run's output")
	noThink := flag.Bool("nothink", false, "enable NoThink")
	describeOnly := flag.Bool("describe-only", false, "run speakerattr's experimental isolated DescribeChapter pass instead of AttributeChapter - see internal/speakerattr/describe_experiment.go")
	scareQuoteOnly := flag.Bool("scarequote-only", false, "run speakerattr's ScareQuoteChapter pass instead of AttributeChapter")
	flag.Parse()

	db, err := sql.Open("duckdb", *dbPath+"?access_mode=READ_ONLY")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var bookTitle string
	if err := db.QueryRow(`SELECT title FROM books WHERE id = ?`, *bookID).Scan(&bookTitle); err != nil {
		log.Fatalf("book lookup: %v", err)
	}

	rows, err := db.Query(`SELECT id, title, idx FROM chapters WHERE book_id = ? AND idx >= ? ORDER BY idx LIMIT ?`, *bookID, *startIdx, *numChapters)
	if err != nil {
		log.Fatal(err)
	}
	var chapters []chapter
	for rows.Next() {
		var c chapter
		if err := rows.Scan(&c.id, &c.title, &c.idx); err != nil {
			log.Fatal(err)
		}
		chapters = append(chapters, c)
	}
	rows.Close()

	// benchattr talks to a llmworker.Worker directly, in-process - no
	// ttsworker HTTP round trip to set up for a one-shot benchmarking tool
	// (llmworker.Worker satisfies speakerattr's llmBackend interface
	// directly, the same as *ttsworker.Manager does for cmd/server - see
	// speakerattr.llmBackend's own doc comment).
	llm := llmworker.New(llmworker.Config{
		ModelPath:     *modelPath,
		NGPULayers:    int32(*gpuLayers),
		NCtx:          uint32(*nctx),
		SystemPrompts: speakerattr.SystemPrompts(),
	})
	defer llm.Close()

	client := speakerattr.NewClient(speakerattr.Config{NoThink: *noThink}, llm)
	defer client.Close()

	fmt.Printf("=== %s (book=%q, nothink=%v) ===\n", *label, bookTitle, *noThink)

	var known []string
	var totalElapsed time.Duration
	for _, ch := range chapters {
		prows, err := db.Query(`SELECT idx, content, inline, is_quote FROM paragraphs WHERE chapter_id = ? ORDER BY idx`, ch.id)
		if err != nil {
			log.Fatal(err)
		}
		var paras []speakerattr.ParagraphInput
		for prows.Next() {
			var p speakerattr.ParagraphInput
			if err := prows.Scan(&p.Idx, &p.Text, &p.Inline, &p.IsQuote); err != nil {
				log.Fatal(err)
			}
			paras = append(paras, p)
		}
		prows.Close()

		start := time.Now()
		var (
			remaining   []speakerattr.ParagraphInput
			attrErr     error
			speakers    map[int]string
			describes   map[int][]string
			scareQuotes map[int]bool
		)
		switch {
		case *describeOnly:
			describes, remaining, attrErr = client.DescribeChapter(context.Background(), bookTitle, ch.title, known, paras, nil)
		case *scareQuoteOnly:
			scareQuotes, remaining, attrErr = client.ScareQuoteChapter(context.Background(), bookTitle, ch.title, paras, nil)
		default:
			speakers, _, remaining, attrErr = client.AttributeChapter(context.Background(), bookTitle, ch.title, known, nil, nil, paras, nil)
		}
		elapsed := time.Since(start)
		totalElapsed += elapsed

		fmt.Printf("\n--- chapter %d %q: %d paragraphs, %v (remaining=%d) ---\n", ch.idx, ch.title, len(paras), elapsed, len(remaining))
		if attrErr != nil {
			fmt.Printf("ERROR: %v\n", attrErr)
		}
		for _, p := range paras {
			text := p.Text
			if len(text) > 90 {
				text = text[:90] + "..."
			}
			text = strings.ReplaceAll(text, "\n", " ")
			if *describeOnly {
				names := describes[p.Idx]
				describesStr := ""
				if len(names) > 0 {
					describesStr = " [describes: " + strings.Join(names, ", ") + "]"
				}
				fmt.Printf("[%3d]%s | %s\n", p.Idx, describesStr, text)
				for _, name := range names {
					known = addKnownLocal(known, name)
				}
				continue
			}
			if *scareQuoteOnly {
				flag := ""
				if scareQuotes[p.Idx] {
					flag = " [SCARE QUOTE]"
				}
				fmt.Printf("[%3d]%s | %s\n", p.Idx, flag, text)
				continue
			}
			speaker := speakers[p.Idx]
			fmt.Printf("[%3d] %-20s | %s\n", p.Idx, speaker, text)
			known = addKnownLocal(known, speaker)
		}
	}
	fmt.Printf("\n=== %s TOTAL attribution time: %v ===\n", *label, totalElapsed)
}

func addKnownLocal(known []string, name string) []string {
	if name == "" || name == "Narrator" || name == "Unknown" {
		return known
	}
	for _, k := range known {
		if k == name {
			return known
		}
	}
	return append(known, name)
}
