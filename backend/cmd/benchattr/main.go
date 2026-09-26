// benchattr is a throwaway benchmarking tool comparing speakerattr's
// default (hybrid-thinking) prompting against the NoThink variant on real
// chapters from an existing library.duckdb copy. Not wired into cmd/server
// or any build; delete this directory when done.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/rhino1998/lectable/backend/internal/llmworker"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/llamacpp-go/llamacpp"
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
	useRoster := flag.Bool("roster", false, "seed attribution with the book's real character roster (valid names, aliases, most frequent speakers) instead of starting empty")
	score := flag.Bool("score", false, "score each chapter's dialogue speakers against the ones stored in the library (e.g. hand-corrected chapters)")
	quiet := flag.Bool("quiet", false, "don't print every paragraph")
	maxConcurrent := flag.Int("concurrent", 2, "llmworker MaxConcurrent (generation slots)")
	ubatch := flag.Uint("ubatch", 0, "physical batch size (0 = llama.cpp default, 512)")
	flash := flag.String("flash", "auto", "flash attention: auto, on, off")
	prime := flag.Int("prime", -1, "prime only the first N of speakerattr.SystemPrompts (attribution's is first); -1 = all, like production")
	primeState := flag.Bool("prime-state", false, "llmworker PrimeAsState: primed prompts as restored KV snapshots in a non-unified cache")
	kvType := flag.String("kv", "", "KV cache type: f16, q8_0 (empty = llama.cpp default)")
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
		SystemPrompts: primeList(*prime),
		MaxConcurrent: *maxConcurrent,
		PrimeAsState:  *primeState,
		NUBatch:       uint32(*ubatch),
		FlashAttn:     map[string]llamacpp.FlashAttnMode{"auto": llamacpp.FlashAttnAuto, "on": llamacpp.FlashAttnOn, "off": llamacpp.FlashAttnOff}[*flash],
		KVType:        map[string]llamacpp.KVType{"": llamacpp.KVTypeDefault, "f16": llamacpp.KVTypeF16, "q8_0": llamacpp.KVTypeQ8_0}[*kvType],
	})
	defer llm.Close()

	client := speakerattr.NewClient(speakerattr.Config{NoThink: *noThink}, llm)
	defer client.Close()

	fmt.Printf("=== %s (book=%q, nothink=%v) ===\n", *label, bookTitle, *noThink)

	var known []string
	roster := speakerattr.Roster{}
	if *useRoster {
		roster = loadRoster(db, *bookID)
		known = append(known, roster.Names...)
	}
	var totalElapsed time.Duration
	var agree, total int
	for _, ch := range chapters {
		prows, err := db.Query(`SELECT idx, content, inline, is_quote AND NOT scare_quote, speaker FROM paragraphs WHERE chapter_id = ? ORDER BY idx`, ch.id)
		if err != nil {
			log.Fatal(err)
		}
		var paras []speakerattr.ParagraphInput
		stored := map[int]string{}
		for prows.Next() {
			var p speakerattr.ParagraphInput
			var sp string
			if err := prows.Scan(&p.Idx, &p.Text, &p.Inline, &p.IsQuote, &sp); err != nil {
				log.Fatal(err)
			}
			paras = append(paras, p)
			stored[p.Idx] = sp
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
			r := roster
			r.Names = known
			if *useRoster {
				r.Prominent = append(append([]string(nil), roster.Prominent...), recentSpeakers(db, *bookID, ch.idx)...)
			}
			speakers, _, remaining, attrErr = client.AttributeChapter(context.Background(), bookTitle, ch.title, r, paras, nil)
		}
		elapsed := time.Since(start)
		totalElapsed += elapsed

		fmt.Printf("\n--- chapter %d %q: %d paragraphs, %v (remaining=%d) ---\n", ch.idx, ch.title, len(paras), elapsed, len(remaining))
		if attrErr != nil {
			fmt.Printf("ERROR: %v\n", attrErr)
		}
		if *score && !*describeOnly && !*scareQuoteOnly {
			chAgree, chTotal := 0, 0
			for _, p := range paras {
				if !p.IsQuote || stored[p.Idx] == "" {
					continue
				}
				chTotal++
				if speakers[p.Idx] == stored[p.Idx] {
					chAgree++
				}
			}
			agree += chAgree
			total += chTotal
			fmt.Printf("SCORE chapter %d: %d/%d dialogue lines match the stored speaker\n", ch.idx, chAgree, chTotal)
		}
		for _, p := range paras {
			if *quiet {
				break
			}
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
	t := llm.Totals()
	perTok := func(d time.Duration, n int) float64 {
		if d <= 0 {
			return 0
		}
		return float64(n) / d.Seconds()
	}
	fmt.Printf("=== %s LLM: %d calls; prompt %d tok (+%d reused) in %v summed prefill (%.0f tok/s per call); gen %d tok in %v summed decode (%.1f tok/s per call); queued %v ===\n",
		*label, t.Calls, t.PromptTokens, t.ReusedTokens, t.Prefill.Round(time.Millisecond), perTok(t.Prefill, t.PromptTokens),
		t.GenTokens, t.Decode.Round(time.Millisecond), perTok(t.Decode, t.GenTokens), t.Queued.Round(time.Millisecond))
	r := llm.RoundStats()
	fmt.Printf("=== %s ROUNDS: prefill %d rounds, %d tok in %v (%.0f tok/s); decode-only %d rounds, %d tok in %v (%.1f ms/round, %.0f tok/s) ===\n",
		*label, r.PrefillRounds, r.PrefillTokens, r.PrefillTime.Round(time.Millisecond), perTok(r.PrefillTime, r.PrefillTokens),
		r.DecodeRounds, r.DecodeTokens, r.DecodeTime.Round(time.Millisecond), float64(r.DecodeTime.Milliseconds())/float64(max(r.DecodeRounds, 1)), perTok(r.DecodeTime, r.DecodeTokens))
	if r.PrefixReused+r.PrefixRestored > 0 {
		fmt.Printf("=== %s PREFIX: reused in slot %d, restored %d in %v ===\n", *label, r.PrefixReused, r.PrefixRestored, r.RestoreTime.Round(time.Millisecond))
	}
	if *score && total > 0 {
		fmt.Printf("=== %s SCORE: %d/%d (%.1f%%) dialogue lines match the stored speaker ===\n", *label, agree, total, 100*float64(agree)/float64(total))
	}
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

// loadRoster reads the book's series roster the way httpapi does for a
// real attribution run: valid characters, their aliases, and the most
// frequent speakers as Prominent.
func loadRoster(db *sql.DB, bookID string) speakerattr.Roster {
	r := speakerattr.Roster{Aliases: map[string][]string{}, Roles: map[string]bool{}}
	rows, err := db.Query(`SELECT c.name, c.is_role, CAST(c.aliases AS VARCHAR) FROM characters c JOIN books b ON c.scope = CASE WHEN b.series_name = '' THEN 'book:' || b.id ELSE 'series:' || b.series_name END WHERE b.id = ? AND NOT c.invalid ORDER BY c.created_at`, bookID)
	if err != nil {
		log.Fatalf("roster: %v", err)
	}
	for rows.Next() {
		var name, aliases string
		var role bool
		if err := rows.Scan(&name, &role, &aliases); err != nil {
			log.Fatal(err)
		}
		r.Names = append(r.Names, name)
		if role {
			r.Roles[name] = true
		}
		var list []string
		if json.Unmarshal([]byte(aliases), &list) == nil && len(list) > 0 {
			r.Aliases[name] = list
		}
	}
	rows.Close()
	crows, err := db.Query(`SELECT p.speaker FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id WHERE c.book_id = ? AND p.is_quote AND p.speaker NOT IN ('', 'Narrator', 'Unknown') GROUP BY p.speaker ORDER BY count(*) DESC LIMIT 12`, bookID)
	if err != nil {
		log.Fatalf("prominent: %v", err)
	}
	for crows.Next() {
		var n string
		if err := crows.Scan(&n); err != nil {
			log.Fatal(err)
		}
		r.Prominent = append(r.Prominent, n)
	}
	crows.Close()
	return r
}

// recentSpeakers mirrors httpapi.prominentSpeakers' "still on stage"
// half: everyone who spoke in the two chapters before chapterIdx.
func recentSpeakers(db *sql.DB, bookID string, chapterIdx int) []string {
	rows, err := db.Query(`SELECT DISTINCT p.speaker FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id WHERE c.book_id = ? AND c.idx >= ? AND c.idx < ? AND p.is_quote AND p.speaker NOT IN ('', 'Narrator', 'Unknown')`, bookID, chapterIdx-2, chapterIdx)
	if err != nil {
		log.Fatalf("recent speakers: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			log.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func primeList(n int) []string {
	all := speakerattr.SystemPrompts()
	if n < 0 || n > len(all) {
		return all
	}
	return all[:n]
}
