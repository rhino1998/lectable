// Command migrate_chapter_state is a one-off migration for pre-existing
// deployments: adds the chapters.state column (see store.go's own CREATE
// TABLE comment and store.ChapterState) to an already-on-disk
// library.duckdb that predates it, replacing the old chapters.attributed/
// chapters.directed columns - see migrate_tts_tags' own doc comment for
// why a running deployment needs this instead of this app's normal "wipe
// and start fresh" schema-change convention.
//
// Unlike a plain "add a column with DEFAULT ”" migration
// (migrate_tts_tags/migrate_chapter_directed's own shape - DuckDB's ALTER
// TABLE ADD COLUMN refuses any column-level constraint, so this also
// can't declare NOT NULL directly), this one also backfills real progress
// from the columns it's replacing: a chapter already attributed=true
// becomes state='attributed', and a chapter with any directed entry
// becomes state='tagged' (applied after the attributed backfill, so a
// chapter that's both lands on the further-along 'tagged' rather than
// being overwritten back down - see store.ChapterState's own doc comment
// for why cardinality(directed) > 0, not a specific model key, is enough:
// every entry ever written was true in practice, per SetChapterDirected's
// only real call site). Without this backfill every chapter would start
// over at ChapterStateNone and pipeline-skip logic (httpapi.runBookPipeline)
// would redo attribution/tagging for a whole library's worth of
// already-finished chapters.
//
// Finally, best-effort attempts to drop the two old columns outright,
// now that nothing reads them - logged but non-fatal if this DuckDB
// version doesn't support DROP COLUMN, in which case they're simply left
// behind as harmless, unused legacy columns.
//
// Safe to run more than once (checks for the state column first; running
// it again on an already-migrated database is a no-op).
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

	has, err := hasColumn(db, "chapters", "state")
	if err != nil {
		log.Fatalf("check existing schema: %v", err)
	}
	if has {
		fmt.Println("chapters.state already exists - nothing to do")
		return
	}

	if _, err := db.Exec(`ALTER TABLE chapters ADD COLUMN state TEXT DEFAULT ''`); err != nil {
		log.Fatalf("add state column: %v", err)
	}
	fmt.Println("added chapters.state")

	// Past this point, never log.Fatal/os.Exit directly (see
	// migrate_chapter_directed's own doc comment for the real WAL-replay
	// crash an early exit after a committed write caused there) - each
	// step below is its own committed statement, so a failure partway
	// through just means whatever ran already succeeded and is safe to
	// leave as-is; log and continue rather than abort.
	if res, err := db.Exec(`UPDATE chapters SET state = 'attributed' WHERE attributed = true`); err != nil {
		log.Printf("backfill attributed -> state: %v", err)
	} else {
		n, _ := res.RowsAffected()
		fmt.Printf("backfilled %d chapter(s) to state='attributed'\n", n)
	}
	if res, err := db.Exec(`UPDATE chapters SET state = 'tagged' WHERE cardinality(directed) > 0`); err != nil {
		log.Printf("backfill directed -> state: %v", err)
	} else {
		n, _ := res.RowsAffected()
		fmt.Printf("backfilled %d chapter(s) to state='tagged'\n", n)
	}

	if _, err := db.Exec(`ALTER TABLE chapters DROP COLUMN attributed`); err != nil {
		log.Printf("drop chapters.attributed (non-fatal, safe to leave in place): %v", err)
	} else {
		fmt.Println("dropped chapters.attributed")
	}
	if _, err := db.Exec(`ALTER TABLE chapters DROP COLUMN directed`); err != nil {
		log.Printf("drop chapters.directed (non-fatal, safe to leave in place): %v", err)
	} else {
		fmt.Println("dropped chapters.directed")
	}

	var total, none, attributed, tagged int
	if err := db.QueryRow(`SELECT count(*) FROM chapters`).Scan(&total); err != nil {
		log.Printf("verify (non-fatal): count chapters: %v", err)
		return
	}
	if err := db.QueryRow(`SELECT count(*) FROM chapters WHERE state = ''`).Scan(&none); err != nil {
		log.Printf("verify (non-fatal): count state='': %v", err)
		return
	}
	if err := db.QueryRow(`SELECT count(*) FROM chapters WHERE state = 'attributed'`).Scan(&attributed); err != nil {
		log.Printf("verify (non-fatal): count state='attributed': %v", err)
		return
	}
	if err := db.QueryRow(`SELECT count(*) FROM chapters WHERE state = 'tagged'`).Scan(&tagged); err != nil {
		log.Printf("verify (non-fatal): count state='tagged': %v", err)
		return
	}
	fmt.Printf("%d total chapters: %d none, %d attributed, %d tagged\n", total, none, attributed, tagged)
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
