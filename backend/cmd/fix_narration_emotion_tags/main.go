// Command fix_narration_emotion_tags is a one-off data-cleanup tool: it
// strips any <|emotion:*|> delivery-tag insertion from narration
// paragraphs (is_quote = false) that were tagged before
// speakerattr.direction.go's stripEmotionFromNonQuotes existed (or before
// a chapter was re-tagged since it landed) - emotion tags are meant to be
// dialogue-only (see backend/CLAUDE.md's "Speech-direction tagging"
// section), but that enforcement only ever applies going forward, to a
// *fresh* DirectChapter run. Nothing retroactively cleans up a tag
// already sitting in paragraphs.tts_tags from before the fix existed -
// and even re-running direction-tagging on an already-tagged chapter
// used to leave a stale tag in place, since a paragraph absent from a
// fresh run's own output only meant "untouched", not "explicitly cleared"
// (see httpapi.clearStaleDirectionTags, added alongside this tool to fix
// that going forward - this tool exists because that fix alone doesn't
// touch data already on disk, and re-tagging an entire book through the
// LLM queue to pick up the fix is far slower than a direct data pass).
//
// Safe to run more than once - a paragraph with no emotion tag on its
// narration (whether it never had one, or a previous run of this same
// tool already cleared it) is simply left untouched.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/rhino1998/lectable/backend/internal/deliverytags"
)

// paragraphDirection mirrors store.ParagraphDirection's own JSON shape -
// duplicated here (rather than importing internal/store, which would
// pull in this whole module's DuckDB schema-management/business-logic
// surface for two struct fields) since this tool only ever needs these
// two fields and their exact json tags.
type paragraphDirection struct {
	SentenceText string `json:"sentenceText,omitempty"`
	InlineText   string `json:"inlineText,omitempty"`
}

func main() {
	dbPath := flag.String("db", "./data/library.duckdb", "path to library.duckdb")
	dataDir := flag.String("data-dir", "./data", "backend DATA_DIR (for invalidating affected chapters' cached audio)")
	dryRun := flag.Bool("dry-run", false, "report what would change without writing anything")
	flag.Parse()

	db, err := sql.Open("duckdb", *dbPath)
	if err != nil {
		log.Fatalf("open %s: %v", *dbPath, err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatalf("ping %s: %v (is the backend still running against this file?)", *dbPath, err)
	}

	rows, err := db.Query(`
		SELECT p.id, p.chapter_id, c.book_id, p.content, p.tts_tags
		FROM paragraphs p
		JOIN chapters c ON c.id = p.chapter_id
		WHERE p.is_quote = false AND p.tts_tags != '{}'`)
	if err != nil {
		log.Fatalf("query paragraphs: %v", err)
	}

	type update struct {
		id      string
		encoded string
	}
	var updates []update
	// chapterID -> bookID, for the ones this run actually changes - used
	// both to clear paragraph_audio rows and to remove the matching
	// on-disk audio/<bookID>/<chapterID>/ directory, the same DB-delete/
	// disk-delete pairing httpapi.directChapter's own invalidation uses.
	touchedChapters := map[string]string{}
	scanned := 0
	for rows.Next() {
		var id, chapterID, bookID, content, rawTags string
		if err := rows.Scan(&id, &chapterID, &bookID, &content, &rawTags); err != nil {
			log.Fatalf("scan paragraph: %v", err)
		}
		scanned++
		tags := map[string]paragraphDirection{}
		if err := json.Unmarshal([]byte(rawTags), &tags); err != nil {
			log.Printf("skip paragraph %s: decode tts_tags: %v", id, err)
			continue
		}
		changed := false
		for cloneModel, d := range tags {
			if d.SentenceText == "" {
				continue
			}
			insertions, err := deliverytags.ExtractInsertions(content, d.SentenceText)
			if err != nil {
				log.Printf("skip paragraph %s (%s): re-extract sentenceText: %v", id, cloneModel, err)
				continue
			}
			var kept []deliverytags.Insertion
			strippedAny := false
			for _, ins := range insertions {
				if strings.HasPrefix(ins.Tag, "<|emotion:") {
					strippedAny = true
					continue
				}
				kept = append(kept, ins)
			}
			if !strippedAny {
				continue
			}
			changed = true
			if len(kept) == 0 {
				d.SentenceText = ""
			} else {
				d.SentenceText = deliverytags.Merge(content, kept)
			}
			if d.SentenceText == "" && d.InlineText == "" {
				delete(tags, cloneModel)
			} else {
				tags[cloneModel] = d
			}
		}
		if !changed {
			continue
		}
		encoded, err := json.Marshal(tags)
		if err != nil {
			log.Printf("skip paragraph %s: encode tts_tags: %v", id, err)
			continue
		}
		updates = append(updates, update{id: id, encoded: string(encoded)})
		touchedChapters[chapterID] = bookID
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("iterate paragraphs: %v", err)
	}
	rows.Close()

	fmt.Printf("scanned %d narration paragraphs with tags, %d need emotion tags stripped, across %d chapters\n",
		scanned, len(updates), len(touchedChapters))
	if *dryRun {
		fmt.Println("dry run - no changes written")
		return
	}
	if len(updates) == 0 {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Fatalf("begin: %v", err)
	}
	for _, u := range updates {
		if _, err := tx.Exec(`UPDATE paragraphs SET tts_tags = ? WHERE id = ?`, u.encoded, u.id); err != nil {
			tx.Rollback()
			log.Fatalf("update paragraph %s: %v", u.id, err)
		}
	}
	// Clear each touched chapter's cached audio status too, so its
	// paragraphs regenerate with the corrected (non-emotional) narration
	// text next time they're read/generated - the DB half of the same
	// invalidation httpapi.directChapter does for a fresh tagging run
	// that actually changes something.
	for chapterID := range touchedChapters {
		if _, err := tx.Exec(`
			DELETE FROM paragraph_audio WHERE paragraph_id IN (
				SELECT id FROM paragraphs WHERE chapter_id = ?
			)`, chapterID); err != nil {
			tx.Rollback()
			log.Fatalf("clear audio status for chapter %s: %v", chapterID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("commit: %v", err)
	}
	fmt.Printf("updated %d paragraphs, cleared cached audio status for %d chapters\n", len(updates), len(touchedChapters))

	// The on-disk half of invalidation - past this point, never call
	// log.Fatal (see migrate_chapter_directed's own doc comment for the
	// real WAL-replay crash an early exit after a committed write caused
	// there); a failure removing a stale .wav is merely informational,
	// not something worth aborting over once the DB side already
	// committed.
	removed := 0
	for chapterID, bookID := range touchedChapters {
		dir := fmt.Sprintf("%s/audio/%s/%s", *dataDir, bookID, chapterID)
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("remove stale audio dir %s: %v", dir, err)
			continue
		}
		removed++
	}
	fmt.Printf("removed on-disk audio for %d/%d touched chapters\n", removed, len(touchedChapters))
}
