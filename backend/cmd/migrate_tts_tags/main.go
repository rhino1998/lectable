// Command migrate_tts_tags is a one-off migration for pre-existing
// deployments: adds the paragraphs.tts_tags column (see store.go's own
// CREATE TABLE comment and internal/speakerattr's DirectChapter) to an
// already-on-disk library.duckdb that predates it, since this app's normal
// "no migrations, just edit CREATE TABLE IF NOT EXISTS" convention
// (backend/CLAUDE.md's "Layout" section) only reaches a table that doesn't
// exist yet - CREATE TABLE IF NOT EXISTS is a no-op against a table that
// already exists, even if its own column list has since grown.
//
// DuckDB's own ALTER TABLE ADD COLUMN refuses a column-level constraint
// outright ("Adding columns with constraints not yet supported" - the same
// limitation backend/CLAUDE.md already documents for a NOT NULL column),
// so this adds the column WITHOUT the NOT NULL the fresh-install schema
// declares - DEFAULT '{}' alone still backfills every existing row
// immediately (verified: an ALTER TABLE ... DEFAULT retroactively sets the
// existing rows' value, not just future ones) and every future INSERT INTO
// paragraphs already omits tts_tags from its own column list (see
// store.go's insertParagraph-equivalent), so it always gets the DEFAULT
// applied by the insert itself regardless of the column's own nullability -
// there is never an actual NULL in this column in practice, even though
// the migrated column is technically nullable while a fresh install's
// isn't. Safe to run more than once (checks for the column first).
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

	has, err := hasColumn(db, "paragraphs", "tts_tags")
	if err != nil {
		log.Fatalf("check existing schema: %v", err)
	}
	if has {
		fmt.Println("paragraphs.tts_tags already exists - nothing to do")
		return
	}

	if _, err := db.Exec(`ALTER TABLE paragraphs ADD COLUMN tts_tags TEXT DEFAULT '{}'`); err != nil {
		log.Fatalf("add tts_tags column: %v", err)
	}
	fmt.Println("added paragraphs.tts_tags")

	// Best-effort verification only, past this point - never log.Fatal/
	// os.Exit directly once the ALTER above has actually run, since that
	// would skip the deferred db.Close() below and leave the ALTER's own
	// WAL entry un-checkpointed - see migrate_chapter_directed's own doc
	// comment for the real crash this caused there (DuckDB's WAL replay on
	// the *next* open hit an internal engine assertion failure trying to
	// replay an uncheckpointed ALTER). A verification failure here is
	// merely informational - the schema change itself already succeeded.
	var total, backfilled int
	if err := db.QueryRow(`SELECT count(*) FROM paragraphs`).Scan(&total); err != nil {
		log.Printf("verify (non-fatal): count paragraphs: %v", err)
		return
	}
	if err := db.QueryRow(`SELECT count(*) FROM paragraphs WHERE tts_tags = '{}'`).Scan(&backfilled); err != nil {
		log.Printf("verify (non-fatal): count backfilled paragraphs: %v", err)
		return
	}
	fmt.Printf("%d/%d existing paragraphs backfilled to '{}'\n", backfilled, total)
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
