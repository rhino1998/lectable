// sqdecide is a throwaway benchmark: the scare-quote pass with each verdict
// generated (speakerattr.Client.ScareQuoteChapter, what the app runs)
// against the same pass with each verdict read off the model's logits
// (ScareQuoteDecide), over the chapters attreval/sq_gold.json covers plus
// the eval's control chapters. Runs the model in-process, so the live
// queue must be paused and drained first. Writes one attreval-shaped run
// file per mode (attreval.py sq-summary <label>-<mode> scores it), each
// line also carrying the decide pass's probability.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rhino1998/lectable/backend/internal/llmworker"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
)

var mtpDraft int

type target struct {
	name, bookID string
	chapter      int
}

type line struct {
	ParagraphIdx int     `json:"paragraphIdx"`
	Text         string  `json:"text"`
	Current      bool    `json:"current"`
	Proposed     bool    `json:"proposed"`
	Candidate    bool    `json:"candidate"`
	NotSpoken    float64 `json:"notSpoken,omitempty"`
	Mass         float64 `json:"mass,omitempty"`
}

type chapterRun struct {
	Book    string  `json:"book"`
	Chapter int     `json:"chapter"`
	Error   *string `json:"error"`
	Lines   []line  `json:"lines"`
}

type loaded struct {
	target
	bookTitle, title string
	paras            []speakerattr.ParagraphInput
	current          map[int]bool
}

func main() {
	dbPath := flag.String("db", "", "library.sqlite (opened read-only)")
	modelPath := flag.String("model", speakerattr.ModelPathFromEnv(), "gguf model")
	goldPath := flag.String("gold", "attreval/sq_gold.json", "reviewed candidates")
	outDir := flag.String("out", "data/attreval-runs", "where run files go")
	label := flag.String("label", "decide", "run label")
	modes := flag.String("modes", "decide,independent,gen", "comma-separated: gen, decide, independent")
	arise := flag.String("arise", "b225960dc6812f3a8bfc534cb64e94b1", "Arise: Alpha's book id")
	controls := flag.String("controls", "spire=b33edda121f423b4c1e755bf87df093a:40,44,48,52,56,60,80,100;jake=645aa73d56486fe4e4732c9d0180e7c0:10,40,70", "name=bookID:chapters;...")
	concurrent := flag.Int("concurrent", 4, "LLM slots")
	flag.IntVar(&mtpDraft, "mtp", 3, "MTP draft length (0 = off)")
	emotionChapters := flag.String("emotion", "", "run the emotion probe over these chapters (name=bookID:chapters;...) instead; modes are then gen, gen-dense, decide")
	pronInputs := flag.String("pron", "", "run the pronunciation probe over this chapters file ([{key, paras: [{idx, text, inline, isQuote}]}]) instead; modes are then gen, decide, independent, avg, each optionally suffixed -all")
	flag.Parse()

	if *emotionChapters != "" {
		runEmotions(*dbPath, *emotionChapters, *modelPath, *outDir, *label, strings.Split(*modes, ","), *concurrent)
		return
	}
	if *pronInputs != "" {
		runPronunciation(*pronInputs, *modelPath, *outDir, *label, strings.Split(*modes, ","), *concurrent)
		return
	}

	db, err := sql.Open("sqlite3", "file:"+*dbPath+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var gold map[string]bool
	raw, err := os.ReadFile(*goldPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := json.Unmarshal(raw, &gold); err != nil {
		log.Fatal(err)
	}
	seen := map[int]bool{}
	var targets []target
	for k := range gold {
		ch, _ := strconv.Atoi(strings.Split(k, "/")[1])
		if !seen[ch] {
			seen[ch] = true
			targets = append(targets, target{"arise", *arise, ch})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].chapter < targets[j].chapter })
	for _, c := range strings.Split(*controls, ";") {
		name, rest, _ := strings.Cut(c, "=")
		id, chs, _ := strings.Cut(rest, ":")
		for _, s := range strings.Split(chs, ",") {
			ch, _ := strconv.Atoi(s)
			targets = append(targets, target{name, id, ch})
		}
	}

	var chapters []loaded
	for _, t := range targets {
		l := loaded{target: t, current: map[int]bool{}}
		var chapterID string
		if err := db.QueryRow(`SELECT c.id, c.title, b.title FROM chapters c JOIN books b ON b.id = c.book_id WHERE c.book_id = ? AND c.idx = ?`, t.bookID, t.chapter).Scan(&chapterID, &l.title, &l.bookTitle); err != nil {
			log.Fatalf("%s ch %d: %v", t.name, t.chapter, err)
		}
		rows, err := db.Query(`SELECT idx, content, inline, is_quote, scare_quote FROM paragraphs WHERE chapter_id = ? ORDER BY idx`, chapterID)
		if err != nil {
			log.Fatal(err)
		}
		for rows.Next() {
			var p speakerattr.ParagraphInput
			var sq bool
			if err := rows.Scan(&p.Idx, &p.Text, &p.Inline, &p.IsQuote, &sq); err != nil {
				log.Fatal(err)
			}
			l.paras = append(l.paras, p)
			l.current[p.Idx] = sq
		}
		rows.Close()
		chapters = append(chapters, l)
	}

	llm := llmworker.New(llmworker.Config{
		ModelPath:     *modelPath,
		NGPULayers:    -1,
		NCtx:          uint32(speakerattr.SlotNCtx()),
		SystemPrompts: speakerattr.SystemPrompts(),
		MaxConcurrent: *concurrent,
		PrimeAsState:  true,
		NUBatch:       2048,
		MTPDraft:      mtpDraft,
	})
	defer llm.Close()
	client := speakerattr.NewClient(speakerattr.Config{}, llm)
	defer client.Close()

	// Load and prime outside any mode's clock.
	if _, err := llm.LLMGenerate(context.Background(), "You are a test.", "Say ok.", 0, 4); err != nil {
		log.Fatalf("load: %v", err)
	}

	for _, mode := range strings.Split(*modes, ",") {
		before, beforeRounds := llm.Totals(), llm.RoundStats()
		start := time.Now()
		runs := make([]chapterRun, len(chapters))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 2) // attreval.py's two chapters at a time
		for i, ch := range chapters {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				runs[i] = runChapter(client, mode, ch)
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)
		t, r := llm.Totals(), llm.RoundStats()
		fmt.Printf("=== %s: %v wall; %d calls; prompt %d tok (+%d reused); %d generated tok; rounds %d prefill (%v) + %d decode (%v)\n",
			mode, elapsed.Round(time.Millisecond), t.Calls-before.Calls, t.PromptTokens-before.PromptTokens, t.ReusedTokens-before.ReusedTokens, t.GenTokens-before.GenTokens,
			r.PrefillRounds-beforeRounds.PrefillRounds, (r.PrefillTime - beforeRounds.PrefillTime).Round(time.Millisecond),
			r.DecodeRounds-beforeRounds.DecodeRounds, (r.DecodeTime - beforeRounds.DecodeTime).Round(time.Millisecond))
		out, _ := json.MarshalIndent(runs, "", " ")
		path := filepath.Join(*outDir, "sq-"+*label+"-"+mode+".json")
		if err := os.WriteFile(path, out, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", path)
	}
}

func runChapter(client *speakerattr.Client, mode string, ch loaded) chapterRun {
	run := chapterRun{Book: ch.name, Chapter: ch.chapter, Lines: []line{}}
	ctx := context.Background()
	candidate := speakerattr.ScareQuoteCandidates(ch.paras)
	flagged := map[int]bool{}
	var verdicts map[int]speakerattr.ScareQuoteVerdict
	var err error
	switch mode {
	case "gen":
		flagged, _, err = client.ScareQuoteChapter(ctx, ch.bookTitle, ch.title, ch.paras, nil)
	case "decide", "independent":
		verdicts, err = client.ScareQuoteDecide(ctx, ch.bookTitle, ch.title, ch.paras, mode == "independent")
		for idx, v := range verdicts {
			flagged[idx] = v.NotSpoken > 0.5
		}
	default:
		log.Fatalf("unknown mode %q", mode)
	}
	if err != nil {
		msg := err.Error()
		run.Error = &msg
	}
	for _, p := range ch.paras {
		if !p.IsQuote || !candidate[p.Idx] && !ch.current[p.Idx] && !flagged[p.Idx] {
			continue
		}
		v := verdicts[p.Idx]
		run.Lines = append(run.Lines, line{ParagraphIdx: p.Idx, Text: p.Text, Current: ch.current[p.Idx], Proposed: flagged[p.Idx], Candidate: candidate[p.Idx], NotSpoken: v.NotSpoken, Mass: v.Mass})
	}
	return run
}

type pronChapter struct {
	Key   string `json:"key"`
	Paras []struct {
		Idx     int    `json:"idx"`
		Text    string `json:"text"`
		Inline  bool   `json:"inline"`
		IsQuote bool   `json:"isQuote"`
	} `json:"paras"`
}

type pronResult struct {
	Key string `json:"key"`
	speakerattr.PronunciationProbe
}

func runPronunciation(inputs, modelPath, outDir, label string, modes []string, concurrent int) {
	raw, err := os.ReadFile(inputs)
	if err != nil {
		log.Fatal(err)
	}
	var chapters []pronChapter
	if err := json.Unmarshal(raw, &chapters); err != nil {
		log.Fatal(err)
	}
	llm := llmworker.New(llmworker.Config{
		ModelPath:     modelPath,
		NGPULayers:    -1,
		NCtx:          uint32(speakerattr.SlotNCtx()),
		SystemPrompts: speakerattr.SystemPrompts(),
		MaxConcurrent: concurrent,
		PrimeAsState:  true,
		NUBatch:       2048,
		MTPDraft:      mtpDraft,
	})
	defer llm.Close()
	client := speakerattr.NewClient(speakerattr.Config{}, llm)
	defer client.Close()
	if _, err := llm.LLMGenerate(context.Background(), "You are a test.", "Say ok.", 0, 4); err != nil {
		log.Fatalf("load: %v", err)
	}

	for _, mode := range modes {
		probeMode, all := strings.CutSuffix(mode, "-all")
		before, beforeRounds := llm.Totals(), llm.RoundStats()
		start := time.Now()
		results := make([][]pronResult, len(chapters))
		var wg sync.WaitGroup
		sem := make(chan struct{}, concurrent)
		for i, ch := range chapters {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				paras := make([]speakerattr.ParagraphInput, len(ch.Paras))
				for k, p := range ch.Paras {
					paras[k] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote}
				}
				probes, err := client.ProbePronunciation(context.Background(), paras, probeMode, all)
				if err != nil {
					log.Printf("%s: %v", ch.Key, err)
				}
				for _, pr := range probes {
					results[i] = append(results[i], pronResult{ch.Key, pr})
				}
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)
		var flat []pronResult
		for _, r := range results {
			flat = append(flat, r...)
		}
		t, r := llm.Totals(), llm.RoundStats()
		fmt.Printf("=== %s: %d terms; %v wall; %d calls; prompt %d tok (+%d reused); %d generated tok; rounds %d prefill (%v) + %d decode (%v)\n",
			mode, len(flat), elapsed.Round(time.Millisecond), t.Calls-before.Calls, t.PromptTokens-before.PromptTokens, t.ReusedTokens-before.ReusedTokens, t.GenTokens-before.GenTokens,
			r.PrefillRounds-beforeRounds.PrefillRounds, (r.PrefillTime - beforeRounds.PrefillTime).Round(time.Millisecond),
			r.DecodeRounds-beforeRounds.DecodeRounds, (r.DecodeTime - beforeRounds.DecodeTime).Round(time.Millisecond))
		out, _ := json.MarshalIndent(flat, "", " ")
		path := filepath.Join(outDir, "pron-"+label+"-"+mode+".json")
		if err := os.WriteFile(path, out, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", path)
	}
}

type emotionLine struct {
	ParagraphIdx int    `json:"paragraphIdx"`
	Text         string `json:"text"`
	Current      string `json:"current"`
	speakerattr.EmotionVerdict
}

type emotionRun struct {
	Book    string        `json:"book"`
	Chapter int           `json:"chapter"`
	Error   string        `json:"error,omitempty"`
	Lines   []emotionLine `json:"lines"`
}

func runEmotions(dbPath, spec, modelPath, outDir, label string, modes []string, concurrent int) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	type chapter struct {
		target
		bookTitle, title string
		paras            []speakerattr.ParagraphInput
		current          map[int]string
	}
	var chapters []chapter
	for _, c := range strings.Split(spec, ";") {
		name, rest, _ := strings.Cut(c, "=")
		id, chs, _ := strings.Cut(rest, ":")
		for _, s := range strings.Split(chs, ",") {
			idx, _ := strconv.Atoi(s)
			ch := chapter{target: target{name, id, idx}, current: map[int]string{}}
			var chapterID string
			if err := db.QueryRow(`SELECT c.id, c.title, b.title FROM chapters c JOIN books b ON b.id = c.book_id WHERE c.book_id = ? AND c.idx = ?`, id, idx).Scan(&chapterID, &ch.title, &ch.bookTitle); err != nil {
				log.Fatalf("%s ch %d: %v", name, idx, err)
			}
			// As the app's pass sees a chapter: a flagged scare quote isn't dialogue.
			rows, err := db.Query(`SELECT idx, content, inline, is_quote AND NOT scare_quote, emotion FROM paragraphs WHERE chapter_id = ? ORDER BY idx`, chapterID)
			if err != nil {
				log.Fatal(err)
			}
			for rows.Next() {
				var p speakerattr.ParagraphInput
				var emotion string
				if err := rows.Scan(&p.Idx, &p.Text, &p.Inline, &p.IsQuote, &emotion); err != nil {
					log.Fatal(err)
				}
				ch.paras = append(ch.paras, p)
				ch.current[p.Idx] = emotion
			}
			rows.Close()
			chapters = append(chapters, ch)
		}
	}

	llm := llmworker.New(llmworker.Config{
		ModelPath:     modelPath,
		NGPULayers:    -1,
		NCtx:          uint32(speakerattr.SlotNCtx()),
		SystemPrompts: speakerattr.SystemPrompts(),
		MaxConcurrent: concurrent,
		PrimeAsState:  true,
		NUBatch:       2048,
		MTPDraft:      mtpDraft,
	})
	defer llm.Close()
	client := speakerattr.NewClient(speakerattr.Config{}, llm)
	defer client.Close()
	if _, err := llm.LLMGenerate(context.Background(), "You are a test.", "Say ok.", 0, 4); err != nil {
		log.Fatalf("load: %v", err)
	}

	for _, mode := range modes {
		before, beforeRounds := llm.Totals(), llm.RoundStats()
		start := time.Now()
		runs := make([]emotionRun, len(chapters))
		var wg sync.WaitGroup
		sem := make(chan struct{}, concurrent)
		for i, ch := range chapters {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				run := emotionRun{Book: ch.name, Chapter: ch.chapter, Lines: []emotionLine{}}
				verdicts, err := client.EmotionProbe(context.Background(), ch.bookTitle, ch.title, ch.paras, mode)
				if err != nil {
					run.Error = err.Error()
				}
				for _, p := range ch.paras {
					if v, ok := verdicts[p.Idx]; ok {
						run.Lines = append(run.Lines, emotionLine{p.Idx, p.Text, ch.current[p.Idx], v})
					}
				}
				runs[i] = run
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)
		t, r := llm.Totals(), llm.RoundStats()
		fmt.Printf("=== %s: %v wall; %d calls; prompt %d tok (+%d reused); %d generated tok; rounds %d prefill (%v) + %d decode (%v)\n",
			mode, elapsed.Round(time.Millisecond), t.Calls-before.Calls, t.PromptTokens-before.PromptTokens, t.ReusedTokens-before.ReusedTokens, t.GenTokens-before.GenTokens,
			r.PrefillRounds-beforeRounds.PrefillRounds, (r.PrefillTime - beforeRounds.PrefillTime).Round(time.Millisecond),
			r.DecodeRounds-beforeRounds.DecodeRounds, (r.DecodeTime - beforeRounds.DecodeTime).Round(time.Millisecond))
		out, _ := json.MarshalIndent(runs, "", " ")
		path := filepath.Join(outDir, "emo-"+label+"-"+mode+".json")
		if err := os.WriteFile(path, out, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", path)
	}
}
