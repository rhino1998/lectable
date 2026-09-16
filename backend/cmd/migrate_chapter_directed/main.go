// Command migrate_chapter_directed is a one-off migration for pre-existing
// deployments: adds the chapters.directed column (see store.go's own
// CREATE TABLE comment and Chapter.Directed) to an already-on-disk
// library.duckdb that predates it - see migrate_tts_tags' own doc comment
// for why a running deployment needs this instead of this app's normal
// "wipe and start fresh" schema-change convention.
//
// Like migrate_tts_tags, this can't add the column exactly as the
// fresh-install schema declares it: DuckDB's ALTER TABLE ADD COLUMN
// refuses any column-level constraint (confirmed empirically - "Adding
// columns with constraints not yet supported"), and the fresh-install
// declaration is `NOT NULL DEFAULT MAP {}`. DEFAULT alone (no NOT NULL)
// still backfills every existing row to an empty map immediately, and
// every future INSERT INTO chapters already omits `directed` from its own
// column list, so it always gets the DEFAULT applied by the insert itself
// regardless of the column's own nullability - there is never an actual
// NULL in this column in practice. Safe to run more than once (checks for
// the column first).
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

	has, err := hasColumn(db, "chapters", "directed")
	if err != nil {
		log.Fatalf("check existing schema: %v", err)
	}
	if has {
		fmt.Println("chapters.directed already exists - nothing to do")
		return
	}

	if _, err := db.Exec(`ALTER TABLE chapters ADD COLUMN directed MAP(VARCHAR, BOOLEAN) DEFAULT MAP {}`); err != nil {
		log.Fatalf("add directed column: %v", err)
	}
	fmt.Println("added chapters.directed")

	// Best-effort verification only, past this point - once the ALTER
	// above has actually run, this must never call log.Fatal/os.Exit
	// directly (a real, previously-hit failure mode: an early exit skips
	// the deferred db.Close() below, leaving the ALTER's own WAL entry
	// un-checkpointed, and DuckDB's WAL replay on the *next* open hit an
	// internal engine assertion failure trying to replay it - not a bug in
	// the migration's own SQL, but a crash-safety lesson learned from
	// hitting it directly during this migration's own first, buggy attempt
	// at this same verification query). A verification failure here is
	// merely informational - the schema change itself already succeeded
	// and committed with the ALTER above - so it's logged and this simply
	// returns, letting defer db.Close() run normally either way.
	//
	// len(directed) doesn't work here - DuckDB's len() only accepts
	// VARCHAR/BIT/list types, not MAP (confirmed empirically: "No function
	// matches the given name and argument types 'len(MAP(...))'"; the
	// number of entries in a MAP is cardinality(), not len()).
	var total, backfilled int
	if err := db.QueryRow(`SELECT count(*) FROM chapters`).Scan(&total); err != nil {
		log.Printf("verify (non-fatal): count chapters: %v", err)
		return
	}
	if err := db.QueryRow(`SELECT count(*) FROM chapters WHERE cardinality(directed) = 0`).Scan(&backfilled); err != nil {
		log.Printf("verify (non-fatal): count backfilled chapters: %v", err)
		return
	}
	fmt.Printf("%d/%d existing chapters backfilled to an empty map\n", backfilled, total)
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
