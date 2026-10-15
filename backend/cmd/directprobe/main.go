// directprobe is a throwaway tuning harness for the direction pass's
// second stage (speakerattr.Client.DirectLines, free-form speech direction
// for Fish Drama 3): it picks lines the way httpapi.deliverChapter does
// (a stored emotion label, or a delivery verb beside the line) from chosen
// chapters of a library, runs the stage under each prompt variant, and
// writes every reply plus a summary of what came back - how many lines got
// direction, how long and how varied the styles are, how often a label
// was echoed, which tags were used, how often the words were changed.
// Runs the model in-process, so the live queue must be paused and drained
// first (POST /api/jobs/pause?drain=1), and resumed after.
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
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rhino1998/lectable/backend/internal/direction"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/llmworker"
	"github.com/rhino1998/lectable/backend/internal/namekey"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

type chapter struct {
	name    string
	dc      speakerattr.DirectionContext
	lines   []speakerattr.DirectionLine
	labels  map[int]string
	speaker map[int]string
}

// variant is one prompt setup under test.
type variant struct {
	Name string
	Opts speakerattr.DirectionOptions
}

type lineResult struct {
	Book    string   `json:"book"`
	Idx     int      `json:"idx"`
	Speaker string   `json:"speaker"`
	Label   string   `json:"label"`
	Text    string   `json:"text"`
	Before  []string `json:"before"`
	After   []string `json:"after"`
	speakerattr.DirectionProbeResult
	Annotated string `json:"annotated"`
}

func main() {
	dbPath := flag.String("db", "data/library.sqlite", "library.sqlite (opened read-only)")
	modelPath := flag.String("model", speakerattr.ModelPathFromEnv(), "gguf model")
	spec := flag.String("chapters", "", "name=bookID:ch,ch;...")
	perChapter := flag.Int("lines", 30, "at most this many lines a chapter")
	variantsDir := flag.String("variants", "", "directory of <name>.txt system prompts (default prompt as 'default' always runs unless -skip-default)")
	only := flag.String("only", "", "comma-separated variant names to run (default all)")
	skipDefault := flag.Bool("skip-default", false, "don't run the default prompt")
	temps := flag.String("temps", "0", "comma-separated first-attempt temperatures")
	noLabel := flag.Bool("no-label", false, "also run each variant without the label hint")
	outDir := flag.String("out", "", "where run files go")
	label := flag.String("label", "run", "run label")
	concurrent := flag.Int("concurrent", 4, "LLM slots")
	mtp := flag.Int("mtp", 3, "MTP draft length (0 = off)")
	dryRun := flag.Bool("dry", false, "print the picked lines and one user prompt, load no model")
	flag.Parse()

	chapters := loadChapters(*dbPath, *spec, *perChapter)
	total := 0
	for _, ch := range chapters {
		total += len(ch.lines)
		fmt.Printf("%s: %d lines\n", ch.name, len(ch.lines))
	}
	fmt.Printf("%d lines\n", total)

	var variants []variant
	var tempList []float32
	for _, t := range strings.Split(*temps, ",") {
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 32)
		if err != nil {
			log.Fatalf("temp %q: %v", t, err)
		}
		tempList = append(tempList, float32(f))
	}
	prompts := map[string]string{}
	if !*skipDefault {
		prompts["default"] = ""
	}
	if *variantsDir != "" {
		files, _ := filepath.Glob(filepath.Join(*variantsDir, "*.txt"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				log.Fatal(err)
			}
			prompts[strings.TrimSuffix(filepath.Base(f), ".txt")] = strings.TrimSpace(string(b))
		}
	}
	keep := map[string]bool{}
	for _, n := range strings.Split(*only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			keep[n] = true
		}
	}
	names := make([]string, 0, len(prompts))
	for n := range prompts {
		if len(keep) == 0 || keep[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		for _, t := range tempList {
			hints := []bool{true}
			if *noLabel {
				hints = append(hints, false)
			}
			for _, hint := range hints {
				name := n
				if len(tempList) > 1 || t != 0 {
					name += fmt.Sprintf("-t%.1f", t)
				}
				if !hint {
					name += "-nolabel"
				}
				variants = append(variants, variant{name, speakerattr.DirectionOptions{SystemPrompt: prompts[n], Temp: t, LabelHint: hint}})
			}
		}
	}

	if *dryRun {
		client := speakerattr.NewClient(speakerattr.Config{}, nil)
		for _, ch := range chapters {
			if len(ch.lines) > 0 {
				fmt.Println("--- user prompt sample ---")
				fmt.Println(client.DirectionUserPrompt(ch.dc, ch.lines[0]))
				break
			}
		}
		for _, v := range variants {
			fmt.Println("variant", v.Name)
		}
		return
	}
	if *outDir == "" {
		log.Fatal("-out is required")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	llm := llmworker.New(llmworker.Config{
		ModelPath:     *modelPath,
		NGPULayers:    -1,
		NCtx:          uint32(speakerattr.SlotNCtx()),
		SystemPrompts: speakerattr.SystemPrompts(),
		MaxConcurrent: *concurrent,
		PrimeAsState:  true,
		NUBatch:       2048,
		MTPDraft:      *mtp,
	})
	defer llm.Close()
	client := speakerattr.NewClient(speakerattr.Config{}, llm)
	defer client.Close()
	if _, err := llm.LLMGenerate(context.Background(), "You are a test.", "Say ok.", 0, 4); err != nil {
		log.Fatalf("load: %v", err)
	}

	for _, v := range variants {
		start := time.Now()
		var results []lineResult
		for _, ch := range chapters {
			got := client.DirectLinesProbe(context.Background(), v.Opts, ch.dc, ch.lines)
			for _, l := range ch.lines {
				r := got[l.Idx]
				lr := lineResult{Book: ch.name, Idx: l.Idx, Speaker: l.Speaker, Label: ch.labels[l.Idx], Text: l.Text, Before: l.Before, After: l.After, DirectionProbeResult: r}
				if !r.Direction.Empty() {
					lr.Annotated = r.Direction.Annotate(l.Text)
				}
				results = append(results, lr)
			}
		}
		elapsed := time.Since(start)
		path := filepath.Join(*outDir, *label+"-"+v.Name+".jsonl")
		f, err := os.Create(path)
		if err != nil {
			log.Fatal(err)
		}
		enc := json.NewEncoder(f)
		for _, r := range results {
			if err := enc.Encode(r); err != nil {
				log.Fatal(err)
			}
		}
		f.Close()
		fmt.Printf("\n=== %s (%v, %s)\n", v.Name, elapsed.Round(time.Second), path)
		system := v.Opts.SystemPrompt
		if system == "" {
			system = speakerattr.DirectionSystemPrompt()
		}
		summarize(results, strings.ToLower(system))
	}
}

// summarize prints what a variant's replies add up to.
func summarize(results []lineResult, system string) {
	var (
		n, errs, changed, directed, styled, bare, echo, copied, retried, spanLines, markLines int
		styleWords                                                                            int
		styles                                                                                = map[string]int{}
		words                                                                                 = map[string]int{}
		marks, spans                                                                          = map[string]int{}, map[string]int{}
	)
	labelWords := map[string]bool{}
	for _, e := range emotions.All {
		labelWords[e.ID] = true
		labelWords[e.Direction] = true
		labelWords[e.Description] = true
	}
	for _, r := range results {
		n++
		if len(r.Replies) > 1 {
			retried++
		}
		switch {
		case strings.Contains(r.Err, direction.ErrTextChanged.Error()):
			changed++
		case r.Err != "":
			errs++
		}
		d := r.Direction
		if d.Empty() {
			continue
		}
		directed++
		if d.Style != "" {
			styled++
			styles[strings.ToLower(d.Style)]++
			ws := strings.FieldsFunc(strings.ToLower(d.Style), func(c rune) bool { return c == ' ' || c == ',' })
			styleWords += len(ws)
			if len(ws) < 2 {
				bare++
			}
			if labelWords[strings.ToLower(d.Style)] {
				echo++
			}
			if strings.Contains(system, "["+strings.ToLower(d.Style)+"]") {
				copied++
			}
			for _, w := range ws {
				if !stopWords[w] {
					words[w]++
				}
			}
		}
		if len(d.Marks) > 0 {
			markLines++
		}
		for _, m := range d.Marks {
			marks[m.Tag]++
		}
		if len(d.Spans) > 0 {
			spanLines++
		}
		for _, s := range d.Spans {
			spans[s.Tag]++
		}
	}
	pct := func(a, b int) string {
		if b == 0 {
			return "-"
		}
		return fmt.Sprintf("%d (%.0f%%)", a, 100*float64(a)/float64(b))
	}
	fmt.Printf("lines %d | directed %s | style %s | point tags on %s | spans on %s\n", n, pct(directed, n), pct(styled, n), pct(markLines, n), pct(spanLines, n))
	fmt.Printf("words changed %s | other errors %d | retried %s\n", pct(changed, n), errs, pct(retried, n))
	if styled > 0 {
		fmt.Printf("style: %.1f words avg | one word %s | a label's own %s | a prompt example %s | distinct %s\n",
			float64(styleWords)/float64(styled), pct(bare, styled), pct(echo, styled), pct(copied, styled), pct(len(styles), styled))
	}
	fmt.Printf("top style words: %s\n", top(words, 15))
	fmt.Printf("repeated styles: %s\n", topRepeated(styles, 8))
	fmt.Printf("point tags: %s\n", top(marks, 20))
	fmt.Printf("spans: %s\n", top(spans, 10))
}

var stopWords = map[string]bool{"a": true, "and": true, "the": true, "of": true, "with": true, "to": true, "but": true, "in": true, "her": true, "his": true, "their": true, "voice": false}

func top(m map[string]int, k int) string {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for key, v := range m {
		all = append(all, kv{key, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v || all[i].v == all[j].v && all[i].k < all[j].k })
	var parts []string
	for i, e := range all {
		if i >= k {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", e.k, e.v))
	}
	return strings.Join(parts, ", ")
}

func topRepeated(m map[string]int, k int) string {
	rep := map[string]int{}
	for key, v := range m {
		if v > 1 {
			rep[key] = v
		}
	}
	return top(rep, k)
}

// loadChapters reads spec's chapters and picks their lines as
// httpapi.deliverChapter does.
func loadChapters(dbPath, spec string, perChapter int) []chapter {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	var out []chapter
	for _, c := range strings.Split(spec, ";") {
		if strings.TrimSpace(c) == "" {
			continue
		}
		name, rest, _ := strings.Cut(c, "=")
		bookID, chs, _ := strings.Cut(rest, ":")
		var bookTitle, series string
		if err := db.QueryRow(`SELECT title, series_name FROM books WHERE id = ?`, bookID).Scan(&bookTitle, &series); err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		scope := "book:" + bookID
		if series != "" {
			scope = "series:" + series
		}
		profiles := map[string]string{}
		rows, err := db.Query(`SELECT name, summary FROM characters WHERE scope = ? AND summary <> ''`, scope)
		if err != nil {
			log.Fatal(err)
		}
		for rows.Next() {
			var n, s string
			if err := rows.Scan(&n, &s); err != nil {
				log.Fatal(err)
			}
			profiles[namekey.Fold(n)] = s
		}
		rows.Close()
		setting := ""
		var raw string
		if db.QueryRow(`SELECT setting FROM book_settings WHERE book_id = ?`, bookID).Scan(&raw) == nil {
			var st store.BookSetting
			if json.Unmarshal([]byte(raw), &st) == nil {
				setting = strings.Join(nonEmpty(st.Era, st.Place, st.Genre), "; ")
			}
		}
		for _, s := range strings.Split(chs, ",") {
			idx, _ := strconv.Atoi(s)
			out = append(out, loadChapter(db, name+":"+s, bookID, bookTitle, idx, setting, profiles, perChapter))
		}
	}
	return out
}

func loadChapter(db *sql.DB, name, bookID, bookTitle string, idx int, setting string, profiles map[string]string, perChapter int) chapter {
	var chapterID, title string
	if err := db.QueryRow(`SELECT id, title FROM chapters WHERE book_id = ? AND idx = ?`, bookID, idx).Scan(&chapterID, &title); err != nil {
		log.Fatalf("%s: %v", name, err)
	}
	ch := chapter{name: name, labels: map[int]string{}, speaker: map[int]string{},
		dc: speakerattr.DirectionContext{BookTitle: bookTitle, ChapterTitle: title, Setting: setting}}
	var scenes []store.SummaryScene
	var raw string
	if db.QueryRow(`SELECT summary FROM chapter_summaries WHERE chapter_id = ?`, chapterID).Scan(&raw) == nil {
		var notes store.ChapterNotes
		if json.Unmarshal([]byte(raw), &notes) == nil {
			scenes = notes.Scenes
		}
	}
	rows, err := db.Query(`SELECT idx, content, inline, is_quote, scare_quote, speaker, emotion, table_pos FROM paragraphs WHERE chapter_id = ? ORDER BY idx`, chapterID)
	if err != nil {
		log.Fatal(err)
	}
	var all []store.Paragraph
	for rows.Next() {
		var p store.Paragraph
		if err := rows.Scan(&p.Idx, &p.Text, &p.Inline, &p.IsQuote, &p.ScareQuote, &p.Speaker, &p.Emotion, &p.Table); err != nil {
			log.Fatal(err)
		}
		all = append(all, p)
	}
	rows.Close()
	for i, p := range all {
		if !wants(all, i) {
			continue
		}
		if len(ch.lines) >= perChapter {
			break
		}
		line := speakerattr.DirectionLine{Idx: p.Idx, Text: p.Text, Speaker: p.Speaker,
			Profile: profiles[namekey.Fold(p.Speaker)], Label: p.EffectiveEmotion(), Scene: sceneAt(scenes, p.Idx)}
		for j := max(0, i-3); j < i; j++ {
			line.Before = append(line.Before, passageLine(all[j]))
		}
		for j := i + 1; j < len(all) && j <= i+1; j++ {
			line.After = append(line.After, passageLine(all[j]))
		}
		ch.lines = append(ch.lines, line)
		ch.labels[p.Idx] = p.EffectiveEmotion()
	}
	return ch
}

// wants mirrors httpapi.wantsDirection.
func wants(all []store.Paragraph, i int) bool {
	p := all[i]
	if !p.IsQuote || p.ScareQuote || p.Unvoiced() {
		return false
	}
	if p.EffectiveEmotion() != "" {
		return true
	}
	for _, j := range []int{i - 1, i + 1} {
		if j >= 0 && j < len(all) && !all[j].IsQuote && speakerattr.DeliveryCue(all[j].Text) {
			return true
		}
	}
	return false
}

func sceneAt(scenes []store.SummaryScene, idx int) string {
	var at *store.SummaryScene
	for i := range scenes {
		if scenes[i].StartIdx <= idx {
			at = &scenes[i]
		}
	}
	if at == nil {
		return ""
	}
	return strings.Join(nonEmpty(at.Location, at.Time), ", ")
}

func passageLine(p store.Paragraph) string {
	if p.IsQuote && p.Speaker != "" && p.Speaker != "Unknown" {
		return p.Speaker + ": " + p.Text
	}
	return p.Text
}

func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
