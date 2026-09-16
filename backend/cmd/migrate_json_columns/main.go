// Command migrate_json_columns is a one-off migration for pre-existing
// deployments, covering two schema changes at once - see migrate_tts_tags'
// own doc comment for why a running deployment needs a tool like this
// instead of this app's normal "wipe and start fresh" schema-change
// convention:
//
//  1. chapters.state (a TEXT enum: '', 'attributed', 'tagged') becomes
//     chapters.passes (a native JSON object - see store.Passes), one
//     independent boolean field per pipeline pass instead of one flat,
//     forward-only state. 'attributed' backfills to
//     {"attribution":true,"description":true}; 'tagged' backfills to
//     {"attribution":true,"description":true,"direction":true} - every
//     book in practice ran attribution before tagging, and the one case
//     that wouldn't (a chapter manually tagged first, never attributed) is
//     rare enough that this slightly-optimistic backfill beats leaving
//     'tagged' permanently unrepresented in the new shape.
//  2. paragraphs.describes_characters/tts_tags/pronunciation move from
//     TEXT (holding JSON text, decoded/encoded by hand in Go) to DuckDB's
//     own native JSON column type - same content, just a type DuckDB
//     itself now understands as JSON rather than an opaque string.
//
// Both conversions could be done in place (`ALTER TABLE ... ALTER COLUMN
// ... TYPE JSON` - confirmed empirically to work, including on a TEXT
// column already holding valid JSON text). This tool builds a fresh
// table with the new column shape and copies every row across via
// INSERT ... SELECT instead, then drops the old table and renames the new
// one into its place - so a failure partway through never leaves the
// live table itself in a half-migrated state; the original is only ever
// dropped once the new one has verifiably received every row.
//
// Safe to run more than once (checks each table's own current column
// shape first; an already-migrated table is simply skipped) and safe to
// run with only one of the two migrations still pending (each is
// independent).
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"

	_ "github.com/duckdb/duckdb-go/v2"
)

func main() {
	dbPath := flag.String("db", "./data/library.duckdb", "path to library.duckdb")
	flag.Parse()

	db, err := sql.Open("duckdb", *dbPath)
	if err != nil {
		log.Fatalf("open %s: %v", *dbPath, err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatalf("ping %s: %v (is the backend still running against this file?)", *dbPath, err)
	}

	if err := migrateChapters(db); err != nil {
		log.Fatalf("migrate chapters.passes: %v", err)
	}
	if err := migrateParagraphs(db); err != nil {
		log.Fatalf("migrate paragraphs' JSON columns: %v", err)
	}
}

// columnType reports table.column's own data_type (e.g. "JSON", "VARCHAR"),
// and whether the column exists at all.
func columnType(db *sql.DB, table, column string) (string, bool, error) {
	var typ string
	err := db.QueryRow(
		`SELECT data_type FROM information_schema.columns WHERE table_name = ? AND column_name = ?`,
		table, column,
	).Scan(&typ)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return typ, true, nil
}

func migrateChapters(db *sql.DB) error {
	typ, hasPasses, err := columnType(db, "chapters", "passes")
	if err != nil {
		return fmt.Errorf("check chapters.passes: %w", err)
	}
	if hasPasses && typ == "JSON" {
		fmt.Println("chapters.passes already JSON - nothing to do")
		return nil
	}
	_, hasState, err := columnType(db, "chapters", "state")
	if err != nil {
		return fmt.Errorf("check chapters.state: %w", err)
	}
	if !hasState {
		return fmt.Errorf("chapters has neither a JSON passes column nor a state column to migrate from - unexpected schema, not migrating")
	}

	if _, err := db.Exec(`
		CREATE TABLE chapters_new (
			id TEXT PRIMARY KEY,
			book_id TEXT NOT NULL,
			idx INTEGER NOT NULL,
			title TEXT NOT NULL,
			passes JSON NOT NULL DEFAULT '{}',
			UNIQUE(book_id, idx)
		)`); err != nil {
		return fmt.Errorf("create chapters_new: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO chapters_new (id, book_id, idx, title, passes)
		SELECT id, book_id, idx, title,
			CASE state
				WHEN 'attributed' THEN '{"attribution": true, "description": true}'
				WHEN 'tagged' THEN '{"attribution": true, "description": true, "direction": true}'
				ELSE '{}'
			END
		FROM chapters`); err != nil {
		return fmt.Errorf("copy chapters -> chapters_new: %w", err)
	}

	var total, none, attributed, tagged, newTotal int
	if err := db.QueryRow(`SELECT count(*) FROM chapters`).Scan(&total); err != nil {
		return fmt.Errorf("count chapters: %w", err)
	}
	db.QueryRow(`SELECT count(*) FROM chapters WHERE state = ''`).Scan(&none)
	db.QueryRow(`SELECT count(*) FROM chapters WHERE state = 'attributed'`).Scan(&attributed)
	db.QueryRow(`SELECT count(*) FROM chapters WHERE state = 'tagged'`).Scan(&tagged)
	if err := db.QueryRow(`SELECT count(*) FROM chapters_new`).Scan(&newTotal); err != nil {
		return fmt.Errorf("verify chapters_new row count: %w", err)
	}
	if newTotal != total {
		return fmt.Errorf("chapters_new has %d row(s), chapters has %d - refusing to swap (chapters_new left in place for inspection)", newTotal, total)
	}
	fmt.Printf("copied %d chapter(s) into chapters_new (%d none, %d attributed, %d tagged)\n", total, none, attributed, tagged)

	// Past this point, never log.Fatal/os.Exit directly (see
	// migrate_chapter_directed's own doc comment for the real WAL-replay
	// crash an early exit after a committed write caused there) - each
	// statement below is its own committed statement.
	if _, err := db.Exec(`DROP TABLE chapters`); err != nil {
		return fmt.Errorf("drop old chapters (chapters_new is intact and this is safe to retry): %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE chapters_new RENAME TO chapters`); err != nil {
		log.Printf("rename chapters_new -> chapters failed (non-fatal - old chapters is already dropped; re-run this tool, or rename chapters_new manually): %v", err)
		return nil
	}
	fmt.Println("chapters.passes migration complete")
	return nil
}

func migrateParagraphs(db *sql.DB) error {
	typ, has, err := columnType(db, "paragraphs", "tts_tags")
	if err != nil {
		return fmt.Errorf("check paragraphs.tts_tags: %w", err)
	}
	if has && typ == "JSON" {
		fmt.Println("paragraphs' JSON columns already migrated - nothing to do")
		return nil
	}

	if _, err := db.Exec(`
		CREATE TABLE paragraphs_new (
			id TEXT PRIMARY KEY,
			chapter_id TEXT NOT NULL,
			idx INTEGER NOT NULL,
			position INTEGER NOT NULL DEFAULT 0,
			content TEXT NOT NULL,
			speaker TEXT NOT NULL DEFAULT '',
			inline BOOLEAN NOT NULL DEFAULT false,
			is_quote BOOLEAN NOT NULL DEFAULT false,
			describes_characters JSON NOT NULL DEFAULT '[]',
			tts_tags JSON NOT NULL DEFAULT '{}',
			pronunciation JSON NOT NULL DEFAULT '[]',
			UNIQUE(chapter_id, idx)
		)`); err != nil {
		return fmt.Errorf("create paragraphs_new: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO paragraphs_new
			(id, chapter_id, idx, position, content, speaker, inline, is_quote,
			 describes_characters, tts_tags, pronunciation)
		SELECT id, chapter_id, idx, position, content, speaker, inline, is_quote,
			describes_characters, tts_tags, pronunciation
		FROM paragraphs`); err != nil {
		return fmt.Errorf("copy paragraphs -> paragraphs_new: %w", err)
	}

	var total, newTotal int
	if err := db.QueryRow(`SELECT count(*) FROM paragraphs`).Scan(&total); err != nil {
		return fmt.Errorf("count paragraphs: %w", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM paragraphs_new`).Scan(&newTotal); err != nil {
		return fmt.Errorf("verify paragraphs_new row count: %w", err)
	}
	if newTotal != total {
		return fmt.Errorf("paragraphs_new has %d row(s), paragraphs has %d - refusing to swap (paragraphs_new left in place for inspection)", newTotal, total)
	}
	fmt.Printf("copied %d paragraph(s) into paragraphs_new\n", total)

	if _, err := db.Exec(`DROP TABLE paragraphs`); err != nil {
		return fmt.Errorf("drop old paragraphs (paragraphs_new is intact and this is safe to retry): %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE paragraphs_new RENAME TO paragraphs`); err != nil {
		log.Printf("rename paragraphs_new -> paragraphs failed (non-fatal - old paragraphs is already dropped; re-run this tool, or rename paragraphs_new manually): %v", err)
		return nil
	}
	fmt.Println("paragraphs' JSON columns migration complete")
	return nil
}
