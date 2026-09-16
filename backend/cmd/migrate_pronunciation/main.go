// Command migrate_pronunciation is a one-off migration for pre-existing
// deployments: adds the paragraphs.pronunciation column (see store.go's
// own CREATE TABLE comment and internal/speakerattr's ResolvePronunciation)
// to an already-on-disk library.duckdb that predates it - see
// migrate_tts_tags' own doc comment for why a running deployment needs
// this instead of this app's normal "wipe and start fresh" schema-change
// convention.
//
// Like migrate_tts_tags/migrate_chapter_directed, this can't add the
// column exactly as the fresh-install schema declares it: DuckDB's ALTER
// TABLE ADD COLUMN refuses any column-level constraint ("Adding columns
// with constraints not yet supported"), and the fresh-install declaration
// is `NOT NULL DEFAULT '[]'`. DEFAULT alone (no NOT NULL) still backfills
// every existing row to '[]' immediately, and every future INSERT INTO
// paragraphs already omits pronunciation from its own column list, so it
// always gets the DEFAULT applied by the insert itself regardless of the
// column's own nullability - there is never an actual NULL in this column
// in practice. Safe to run more than once (checks for the column first).
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"

	_ "github.com/duckdb/duckdb-go/v2"
)

func main() {
	path := flag.String("db", "./data/library.duckdb", "path to library.duckdb")
	flag.Parse()

	db, err := sql.Open("duckdb", *path)
	if err != nil {
		log.Fatalf("open %s: %v", *path, err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("ping %s: %v (is the backend still running against this file?)", *path, err)
	}

	has, err := hasColumn(db, "paragraphs", "pronunciation")
	if err != nil {
		log.Fatalf("check existing schema: %v", err)
	}
	if has {
		fmt.Println("paragraphs.pronunciation already exists - nothing to do")
		return
	}

	if _, err := db.Exec(`ALTER TABLE paragraphs ADD COLUMN pronunciation TEXT DEFAULT '[]'`); err != nil {
		log.Fatalf("add pronunciation column: %v", err)
	}
	fmt.Println("added paragraphs.pronunciation")

	// Best-effort verification only, past this point - once the ALTER
	// above has actually run, this must never call log.Fatal/os.Exit
	// directly (a real, previously-hit failure mode documented in
	// migrate_chapter_directed's own doc comment: an early exit skips the
	// deferred db.Close() below, leaving the ALTER's own WAL entry
	// un-checkpointed, and DuckDB's WAL replay on the *next* open hits an
	// internal engine assertion failure trying to replay it). A
	// verification failure here is merely informational - the schema
	// change itself already succeeded and committed with the ALTER above -
	// so it's logged and this simply returns, letting defer db.Close() run
	// normally either way.
	var total, backfilled int
	if err := db.QueryRow(`SELECT count(*) FROM paragraphs`).Scan(&total); err != nil {
		log.Printf("verify (non-fatal): count paragraphs: %v", err)
		return
	}
	if err := db.QueryRow(`SELECT count(*) FROM paragraphs WHERE pronunciation = '[]'`).Scan(&backfilled); err != nil {
		log.Printf("verify (non-fatal): count backfilled paragraphs: %v", err)
		return
	}
	fmt.Printf("%d/%d existing paragraphs backfilled to '[]'\n", backfilled, total)
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`SELECT column_name FROM information_schema.columns WHERE table_name = ?`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
