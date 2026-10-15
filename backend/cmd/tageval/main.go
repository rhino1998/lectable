// Command tageval scores the deterministic speech-tag rules
// (internal/speakerattr: tagFor's named tags and BeatTag's action beats)
// against readers' speaker corrections. It reads library.sqlite read-only:
// for each chapter of the given books in the given range, a dialogue
// line's machine answer is its recorded edit's original (else its current
// speaker) and its truth is its current speaker. Each rule is reported by
// how often it fires, how often it agrees with the truth, and how many
// machine errors it would fix and correct answers it would break.
//
//	go run ./cmd/tageval -db data/library.sqlite -books <id>:<lo>-<hi>,... [-series Underverse]
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"github.com/rhino1998/lectable/backend/internal/namekey"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
)

type span struct {
	book   string
	lo, hi int
}

type score struct{ fires, agree, same, fixes, breaks int }

// samePerson is a rough test for two roster entries of one person ("Reece"
// and "Warden Vatreece" aside): a shared name word of 3+ letters, so
// "Velika" / "Lady Velika Vekuavak" count as the same answer.
func samePerson(a, b string) bool {
	words := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, w := range strings.FieldsFunc(namekey.Fold(s), func(r rune) bool { return r == ' ' || r == '-' || r == ',' }) {
			w = strings.TrimSuffix(w, "'s")
			if len(w) >= 3 && !map[string]bool{"the": true, "lord": true, "lady": true, "sir": true, "captain": true, "general": true}[w] {
				m[w] = true
			}
		}
		return m
	}
	wb := words(b)
	for w := range words(a) {
		if wb[w] {
			return true
		}
	}
	return false
}

func (s *score) add(answer, truth, machine string) {
	s.fires++
	ok := namekey.Equal(answer, truth)
	if ok {
		s.agree++
	} else if samePerson(answer, truth) {
		s.same++
		return
	}
	switch {
	case ok && !namekey.Equal(machine, truth) && !samePerson(machine, truth):
		s.fixes++
	case !ok && (namekey.Equal(machine, truth) || samePerson(machine, truth)):
		s.breaks++
	}
}

func main() {
	dbPath := flag.String("db", "data/library.sqlite", "library database")
	books := flag.String("books", "", "comma-separated <bookID>:<lo>-<hi> chapter ranges")
	series := flag.String("series", "", "also every book of this series, over its edited chapter range")
	examples := flag.Int("examples", 6, "examples to print per rule of a break")
	undoFrom := flag.String("undo-from", "", "a backup taken before a retag run: list the lines it changed that the current rules wouldn't, to -undo-out")
	undoSkip := flag.String("undo-skip", "", "comma-separated book ids -undo-from leaves alone")
	undoOut := flag.String("undo-out", "undo.json", "where -undo-from writes {bookID: [{chapterIdx, paragraphIdx, from, to}]}")
	judged := flag.String("judged", "", "comma-separated verdict files (an audit of changes: {book? title, chapter, paragraph, old, new, verdict}) - report which the rules would still make")
	flag.Parse()
	db, err := sql.Open("sqlite3", "file:"+*dbPath+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	if *judged != "" {
		replayJudged(db, strings.Split(*judged, ","))
		return
	}
	if *undoFrom != "" {
		writeUndo(db, *undoFrom, *undoOut, strings.Split(*undoSkip, ","))
		return
	}
	var spans []span
	for _, f := range strings.Split(*books, ",") {
		if f == "" {
			continue
		}
		id, r, _ := strings.Cut(f, ":")
		lo, hi, _ := strings.Cut(r, "-")
		l, _ := strconv.Atoi(lo)
		h, _ := strconv.Atoi(hi)
		spans = append(spans, span{id, l, h})
	}
	if *series != "" {
		rows, err := db.Query(`SELECT c.book_id, MIN(c.idx), MAX(c.idx) FROM paragraph_edits e JOIN paragraphs p ON p.id = e.paragraph_id
			JOIN chapters c ON c.id = p.chapter_id JOIN books b ON b.id = c.book_id
			WHERE e.field = 'speaker' AND b.series_name = ? GROUP BY c.book_id`, *series)
		if err != nil {
			log.Fatal(err)
		}
		for rows.Next() {
			var s span
			if err := rows.Scan(&s.book, &s.lo, &s.hi); err != nil {
				log.Fatal(err)
			}
			spans = append(spans, s)
		}
		rows.Close()
	}

	scores := map[string]*score{}
	breaks := map[string][]string{}
	rule := func(name, answer, truth, machine, text string) {
		if answer == "" {
			return
		}
		s := scores[name]
		if s == nil {
			s = &score{}
			scores[name] = s
		}
		before := s.breaks
		s.add(answer, truth, machine)
		if s.breaks > before && len(breaks[name]) < *examples {
			breaks[name] = append(breaks[name], fmt.Sprintf("%q -> %q (truth %q): %s", machine, answer, truth, text))
		}
	}
	lines, wrong := 0, 0
	for _, sp := range spans {
		var scope, seriesName string
		var sn sql.NullString
		if err := db.QueryRow(`SELECT series_name FROM books WHERE id = ?`, sp.book).Scan(&sn); err != nil {
			log.Fatal(err)
		}
		seriesName = sn.String
		scope = "book:" + sp.book
		if seriesName != "" {
			scope = "series:" + seriesName
		}
		resolver := rosterResolver(db, scope)
		crows, err := db.Query(`SELECT id FROM chapters WHERE book_id = ? AND idx BETWEEN ? AND ? ORDER BY idx`, sp.book, sp.lo, sp.hi)
		if err != nil {
			log.Fatal(err)
		}
		var chapters []string
		for crows.Next() {
			var id string
			if err := crows.Scan(&id); err != nil {
				log.Fatal(err)
			}
			chapters = append(chapters, id)
		}
		crows.Close()
		for _, ch := range chapters {
			paras, machine, truth := loadChapter(db, ch)
			// The whole rule set as attribution runs it, over the machine's answers.
			refined, changes := speakerattr.RefineWithSpeechTags(paras, machine, resolver)
			for _, c := range changes {
				rule("pipeline: "+c.Reason, refined[c.Idx], truth[c.Idx], machine[c.Idx], c.From+" -> "+c.To)
			}
			for i := range paras {
				p := &paras[i]
				if !p.IsQuote {
					continue
				}
				lines++
				if !namekey.Equal(machine[p.Idx], truth[p.Idx]) {
					wrong++
				}
				// The neighbouring narration segments of the same printed paragraph.
				var prev, next *speakerattr.ParagraphInput
				if p.Inline && i > 0 && !paras[i-1].IsQuote {
					prev = &paras[i-1]
				}
				if i+1 < len(paras) && paras[i+1].Inline && !paras[i+1].IsQuote {
					next = &paras[i+1]
				}
				m, t := machine[p.Idx], truth[p.Idx]
				short := p.Text
				if len(short) > 60 {
					short = short[:60]
				}
				ctx := short
				if next != nil {
					ctx += " | " + trunc(next.Text, 70)
				} else if prev != nil {
					ctx = trunc(prev.Text, 70) + " | " + short
				}
				explicit, pronoun := speakerattr.ExplicitTag(prev, next, resolver)
				rule("named speech tag", explicit, t, m, ctx)
				if explicit != "" || pronoun {
					continue
				}
				rule("beat after the quote", speakerattr.BeatTag(nil, next, resolver), t, m, ctx)
				if speakerattr.BeatTag(nil, next, resolver) == "" {
					rule("beat before the quote", speakerattr.BeatTag(prev, nil, resolver), t, m, ctx)
				}
			}
		}
	}
	fmt.Printf("%d dialogue lines, machine wrong on %d (%.1f%%)\n\n", lines, wrong, 100*float64(wrong)/float64(lines))
	var names []string
	for n := range scores {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("%-30s %7s %9s %9s %7s %7s %7s\n", "rule", "fires", "precision", "same-pers", "fixes", "breaks", "net")
	for _, n := range names {
		s := scores[n]
		fmt.Printf("%-30s %7d %8.1f%% %9d %7d %7d %+7d\n", n, s.fires, 100*float64(s.agree+s.same)/float64(s.fires), s.same, s.fixes, s.breaks, s.fixes-s.breaks)
	}
	for _, n := range names {
		if len(breaks[n]) == 0 {
			continue
		}
		fmt.Printf("\n%s - lines it would break:\n", n)
		for _, e := range breaks[n] {
			fmt.Println("  ", e)
		}
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func rosterResolver(db *sql.DB, scope string) *speakerattr.SpeakerNameResolver {
	rows, err := db.Query(`SELECT name, aliases FROM characters WHERE scope = ? AND NOT invalid`, scope)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	var names []string
	aliases := map[string][]string{}
	for rows.Next() {
		var name string
		var raw sql.NullString
		if err := rows.Scan(&name, &raw); err != nil {
			log.Fatal(err)
		}
		names = append(names, name)
		var as []string
		if raw.Valid && raw.String != "" {
			if err := json.Unmarshal([]byte(raw.String), &as); err != nil {
				log.Printf("%s: aliases of %s: %v", scope, name, err)
			}
		}
		aliases[name] = as
	}
	return speakerattr.NewSpeakerNameResolver(names, aliases)
}

func loadChapter(db *sql.DB, chapterID string) ([]speakerattr.ParagraphInput, map[int]string, map[int]string) {
	edits := map[int]string{}
	erows, err := db.Query(`SELECT p.idx, e.original FROM paragraph_edits e JOIN paragraphs p ON p.id = e.paragraph_id WHERE p.chapter_id = ? AND e.field = 'speaker'`, chapterID)
	if err != nil {
		log.Fatal(err)
	}
	for erows.Next() {
		var idx int
		var orig string
		if err := erows.Scan(&idx, &orig); err != nil {
			log.Fatal(err)
		}
		edits[idx] = orig
	}
	erows.Close()
	rows, err := db.Query(`SELECT idx, content, inline, is_quote AND NOT scare_quote, speaker FROM paragraphs WHERE chapter_id = ? ORDER BY idx`, chapterID)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	var paras []speakerattr.ParagraphInput
	machine, truth := map[int]string{}, map[int]string{}
	label := func(s string) string {
		if s == "" {
			return "Narrator"
		}
		return s
	}
	for rows.Next() {
		var p speakerattr.ParagraphInput
		var speaker string
		if err := rows.Scan(&p.Idx, &p.Text, &p.Inline, &p.IsQuote, &speaker); err != nil {
			log.Fatal(err)
		}
		paras = append(paras, p)
		if p.IsQuote {
			truth[p.Idx] = label(speaker)
			if orig, ok := edits[p.Idx]; ok {
				machine[p.Idx] = label(orig)
			} else {
				machine[p.Idx] = label(speaker)
			}
		}
	}
	return paras, machine, truth
}

// replayJudged re-asks the current rules about audited changes: whether a
// tag or beat would still credit each line to the speaker the change gave
// it, tallied by the audit's verdict.
func replayJudged(db *sql.DB, files []string) {
	type item struct {
		Title     string `json:"title"`
		Chapter   int    `json:"chapter"`
		Paragraph int    `json:"paragraph"`
		Old, New  string
		Verdict   string `json:"verdict"`
	}
	kept := map[string][2]int{} // verdict -> {still made, total}
	resolvers := map[string]*speakerattr.SpeakerNameResolver{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			log.Fatal(err)
		}
		var items []item
		if err := json.Unmarshal(raw, &items); err != nil {
			log.Fatal(err)
		}
		for _, it := range items {
			var bookID, chID string
			var sn sql.NullString
			if err := db.QueryRow(`SELECT b.id, b.series_name, c.id FROM books b JOIN chapters c ON c.book_id = b.id WHERE b.title = ? AND c.idx = ?`, it.Title, it.Chapter).Scan(&bookID, &sn, &chID); err != nil {
				log.Printf("%s ch %d: %v", it.Title, it.Chapter, err)
				continue
			}
			r := resolvers[bookID]
			if r == nil {
				scope := "book:" + bookID
				if sn.String != "" {
					scope = "series:" + sn.String
				}
				r = rosterResolver(db, scope)
				var narrator sql.NullString
				_ = db.QueryRow(`SELECT json_extract(setting, '$.narrator') FROM book_settings WHERE book_id = ?`, bookID).Scan(&narrator)
				if narrator.String != "" && firstPersonBook(db, bookID) {
					if c, ok := r.Resolve(narrator.String); ok {
						r = r.WithFirstPerson(c)
					} else {
						r = r.WithFirstPerson(narrator.String)
					}
				}
				resolvers[bookID] = r
			}
			paras, _, _ := loadChapter(db, chID)
			answer := ""
			for i := range paras {
				if paras[i].Idx != it.Paragraph {
					continue
				}
				var prev, next *speakerattr.ParagraphInput
				if paras[i].Inline && i > 0 && !paras[i-1].IsQuote {
					prev = &paras[i-1]
				}
				if i+1 < len(paras) && paras[i+1].Inline && !paras[i+1].IsQuote {
					next = &paras[i+1]
				}
				name, pronoun := speakerattr.ExplicitTag(prev, next, r)
				if name == "" && !pronoun {
					name = speakerattr.BeatTag(prev, next, r)
				}
				answer = name
			}
			k := kept[it.Verdict]
			if answer != "" && namekey.Equal(answer, it.New) {
				k[0]++
			}
			k[1]++
			kept[it.Verdict] = k
		}
	}
	fmt.Println("verdict    still made / audited")
	for _, v := range []string{"new", "old", "neither", "same", "unclear"} {
		fmt.Printf("  %-8s %4d / %d\n", v, kept[v][0], kept[v][1])
	}
}

// firstPersonBook is httpapi.taggedInFirstPerson over the database.
func firstPersonBook(db *sql.DB, bookID string) bool {
	rows, err := db.Query(`SELECT p.idx, p.content, p.inline, p.is_quote AND NOT p.scare_quote FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id WHERE c.book_id = ? ORDER BY c.idx, p.idx`, bookID)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	var paras []speakerattr.ParagraphInput
	for rows.Next() {
		var p speakerattr.ParagraphInput
		if err := rows.Scan(&p.Idx, &p.Text, &p.Inline, &p.IsQuote); err != nil {
			log.Fatal(err)
		}
		paras = append(paras, p)
	}
	return speakerattr.TaggedInFirstPerson(speakerattr.FirstPersonTags(paras))
}

type undoLine struct {
	ChapterIdx   int    `json:"chapterIdx"`
	ParagraphIdx int    `json:"paragraphIdx"`
	From         string `json:"from"`
	To           string `json:"to"`
}

// writeUndo compares the library with a backup taken before a retag run:
// for every unedited dialogue line whose speaker changed since, it replays
// the current rules (RefineWithSpeechTags) over the backup's speakers, and
// lists the line for restoring when the rules no longer give it its
// current speaker. A line whose old speaker has since been merged away is
// left alone.
func writeUndo(db *sql.DB, backup, out string, skip []string) {
	skipBooks := map[string]bool{}
	for _, b := range skip {
		skipBooks[b] = true
	}
	if _, err := db.Exec(`ATTACH DATABASE ? AS old`, "file:"+backup+"?mode=ro"); err != nil {
		log.Fatal(err)
	}
	chars := map[string]bool{}
	crows, err := db.Query(`SELECT scope, name FROM characters WHERE NOT invalid`)
	if err != nil {
		log.Fatal(err)
	}
	for crows.Next() {
		var scope, name string
		if err := crows.Scan(&scope, &name); err != nil {
			log.Fatal(err)
		}
		chars[scope+"\x00"+name] = true
	}
	crows.Close()
	type chap struct{ book, id, scope string }
	var changed []chap
	rows, err := db.Query(`SELECT DISTINCT b.id, c.id, COALESCE(b.series_name, '') FROM paragraphs p JOIN old.paragraphs o ON o.id = p.id
		JOIN chapters c ON c.id = p.chapter_id JOIN books b ON b.id = c.book_id
		WHERE p.is_quote AND o.speaker <> p.speaker`)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var c chap
		var series string
		if err := rows.Scan(&c.book, &c.id, &series); err != nil {
			log.Fatal(err)
		}
		c.scope = "book:" + c.book
		if series != "" {
			c.scope = "series:" + series
		}
		changed = append(changed, c)
	}
	rows.Close()
	resolvers := map[string]*speakerattr.SpeakerNameResolver{}
	result := map[string][]undoLine{}
	n := 0
	for _, c := range changed {
		r := resolvers[c.book]
		if r == nil {
			r = rosterResolver(db, c.scope)
			var narrator sql.NullString
			_ = db.QueryRow(`SELECT json_extract(setting, '$.narrator') FROM book_settings WHERE book_id = ?`, c.book).Scan(&narrator)
			if narrator.String != "" && firstPersonBook(db, c.book) {
				if name, ok := r.Resolve(narrator.String); ok {
					r = r.WithFirstPerson(name)
				} else {
					r = r.WithFirstPerson(narrator.String)
				}
			}
			resolvers[c.book] = r
		}
		paras, _, current := loadChapter(db, c.id)
		before := map[int]string{}
		prow, err := db.Query(`SELECT p.idx, o.speaker FROM paragraphs p JOIN old.paragraphs o ON o.id = p.id WHERE p.chapter_id = ? AND p.is_quote`, c.id)
		if err != nil {
			log.Fatal(err)
		}
		for prow.Next() {
			var idx int
			var sp string
			if err := prow.Scan(&idx, &sp); err != nil {
				log.Fatal(err)
			}
			if sp == "" {
				sp = "Narrator"
			}
			before[idx] = sp
		}
		prow.Close()
		edited := map[int]bool{}
		erow, err := db.Query(`SELECT p.idx FROM paragraph_edits e JOIN paragraphs p ON p.id = e.paragraph_id WHERE p.chapter_id = ? AND e.field = 'speaker'`, c.id)
		if err != nil {
			log.Fatal(err)
		}
		for erow.Next() {
			var idx int
			if err := erow.Scan(&idx); err != nil {
				log.Fatal(err)
			}
			edited[idx] = true
		}
		erow.Close()
		machine := map[int]string{}
		for idx, sp := range current {
			if b, ok := before[idx]; ok {
				machine[idx] = b
			} else {
				machine[idx] = sp
			}
		}
		refined, _ := speakerattr.RefineWithSpeechTags(paras, machine, r)
		var chIdx int
		if err := db.QueryRow(`SELECT idx FROM chapters WHERE id = ?`, c.id).Scan(&chIdx); err != nil {
			log.Fatal(err)
		}
		for idx, now := range current {
			was, ok := before[idx]
			// Only a named speaker moved to another: Narrator/Unknown on
			// either side is first-time attribution or a re-attribution of
			// Unknown lines since the backup, not the retag.
			sentinel := func(s string) bool { return s == "Narrator" || s == "Unknown" }
			if !ok || was == now || edited[idx] || refined[idx] == now || sentinel(was) || sentinel(now) || skipBooks[c.book] {
				continue
			}
			if was != "Narrator" && was != "Unknown" && !chars[c.scope+"\x00"+was] {
				continue // merged away since
			}
			result[c.book] = append(result[c.book], undoLine{ChapterIdx: chIdx, ParagraphIdx: idx, From: now, To: was})
			n++
		}
	}
	raw, _ := json.MarshalIndent(result, "", " ")
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d lines to restore in %d books -> %s\n", n, len(result), out)
}
