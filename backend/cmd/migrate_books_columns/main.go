// Command migrate_books_columns is a one-off migration for pre-existing
// deployments: adds books.speech_direction and books.estimate_sec_per_char
// (see store.go's own CREATE TABLE comments on both) to an already-on-disk
// library.duckdb that predates them - see migrate_tts_tags' own doc
// comment for why a running deployment needs this instead of this app's
// normal "wipe and start fresh" schema-change convention.
//
// Both are plain scalar defaults (no text to escape, unlike ref_line's own
// in-app migrateCharactersRefLine): speech_direction defaults false (a
// book generates immediately rather than waiting on direction-tagging,
// unless a reader explicitly opts in - see store.Book.SpeechDirection's
// own doc comment), estimate_sec_per_char defaults 0 (uncalibrated, until
// jobs.Manager.EnqueueLengthEstimate's own PocketTTS sample sets it).
//
// Safe to run more than once (checks for each column first; running it
// again on an already-migrated database just skips whichever column(s)
// already exist).
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

	if err := addColumnIfMissing(db, "books", "speech_direction", "BOOLEAN DEFAULT false"); err != nil {
		log.Fatalf("migrate books.speech_direction: %v", err)
	}
	if err := addColumnIfMissing(db, "books", "estimate_sec_per_char", "DOUBLE DEFAULT 0"); err != nil {
		log.Fatalf("migrate books.estimate_sec_per_char: %v", err)
	}

	var total int
	if err := db.QueryRow(`SELECT count(*) FROM books`).Scan(&total); err != nil {
		log.Printf("verify (non-fatal): count books: %v", err)
		return
	}
	fmt.Printf("%d book(s) now have speech_direction/estimate_sec_per_char\n", total)
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

func addColumnIfMissing(db *sql.DB, table, column, columnDef string) error {
	has, err := hasColumn(db, table, column)
	if err != nil {
		return fmt.Errorf("check existing schema: %w", err)
	}
	if has {
		fmt.Printf("%s.%s already exists - nothing to do\n", table, column)
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, columnDef)); err != nil {
		return err
	}
	fmt.Printf("added %s.%s\n", table, column)
	return nil
}
