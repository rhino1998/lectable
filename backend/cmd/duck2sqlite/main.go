// duck2sqlite copies a DuckDB library (the format this app stored its
// database in before SQLite) into a new SQLite one:
//
//	duck2sqlite -from data/library.duckdb -to data/library.sqlite
//
// Run it with the server stopped (DuckDB locks its file). The source is
// opened read-only and never changed; -to must not exist yet. Every table
// is copied except sync_tree_nodes, a derived cache the server rebuilds.
// Afterwards each table's row count, and the paragraphs' total text
// length, are compared between the two.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// skipTables are derived caches, cheaper rebuilt than copied.
var skipTables = []string{"sync_tree_nodes"}

func main() {
	from := flag.String("from", "", "DuckDB library to read")
	to := flag.String("to", "", "SQLite library to create")
	flag.Parse()
	if *from == "" || *to == "" {
		flag.Usage()
		os.Exit(2)
	}
	if _, err := os.Stat(*to); err == nil {
		log.Fatalf("%s already exists", *to)
	}
	if err := run(*from, *to); err != nil {
		for _, f := range []string{*to, *to + "-wal", *to + "-shm"} {
			os.Remove(f)
		}
		log.Fatal(err)
	}
}

func run(from, to string) error {
	src, err := sql.Open("duckdb", from+"?access_mode=READ_ONLY")
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := store.Open(to)
	if err != nil {
		return err
	}
	defer dst.Close()

	tables := dst.Tables()
	names := make([]string, 0, len(tables))
	for t := range tables {
		if !slices.Contains(skipTables, t) {
			names = append(names, t)
		}
	}
	sort.Strings(names)

	for _, table := range names {
		start := time.Now()
		srcCols, err := sourceColumns(src, table)
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		if len(srcCols) == 0 {
			log.Printf("%-22s not in the source, left empty", table)
			continue
		}
		var cols, exprs []string
		for _, c := range tables[table] {
			typ, ok := srcCols[c]
			if !ok {
				log.Printf("%s.%s: not in the source, takes its default", table, c)
				continue
			}
			cols = append(cols, c)
			if typ == "JSON" {
				// The driver decodes a JSON column; keep its stored text.
				exprs = append(exprs, `CAST("`+c+`" AS VARCHAR)`)
			} else {
				exprs = append(exprs, `"`+c+`"`)
			}
		}
		for c := range srcCols {
			if !slices.Contains(cols, c) {
				log.Printf("%s.%s: dropped (not in the SQLite schema)", table, c)
			}
		}
		rows, err := readAll(src, `SELECT `+strings.Join(exprs, ", ")+` FROM `+table, len(cols))
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		if err := dst.LoadTable(table, cols, rows); err != nil {
			return err
		}
		log.Printf("%-22s %8d rows  %s", table, len(rows), time.Since(start).Round(time.Millisecond))
	}

	filled, err := dst.BackfillAudioChapters()
	if err != nil {
		return err
	}
	var audioRows int64
	if err := src.QueryRow(`SELECT count(*) FROM paragraph_audio`).Scan(&audioRows); err != nil {
		return err
	}
	log.Printf("paragraph_audio: chapter_id/chars filled on %d rows; %d rows have no paragraph and were copied as they are", filled, audioRows-filled)
	if err := dst.Analyze(); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return verify(src, to, names)
}

func sourceColumns(src *sql.DB, table string) (map[string]string, error) {
	rows, err := src.Query(`SELECT column_name, data_type FROM information_schema.columns WHERE table_name = ?`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		out[name] = typ
	}
	return out, rows.Err()
}

func readAll(db *sql.DB, query string, n int) ([][]any, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals := make([]any, n)
		ptrs := make([]any, n)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

func verify(src *sql.DB, to string, tables []string) error {
	dst, err := sql.Open("sqlite3", "file:"+to+"?mode=ro")
	if err != nil {
		return err
	}
	defer dst.Close()
	checks := map[string]string{}
	for _, t := range tables {
		checks[t+" rows"] = `SELECT count(*) FROM ` + t
	}
	checks["paragraphs text length"] = `SELECT coalesce(sum(length(content)), 0) FROM paragraphs`
	checks["word timings length"] = `SELECT coalesce(sum(length(word_timings)), 0) FROM paragraph_audio`
	bad := 0
	for name, q := range checks {
		var a, b int64
		if err := src.QueryRow(q).Scan(&a); err != nil {
			if strings.HasSuffix(name, " rows") {
				continue // table absent from the source
			}
			return fmt.Errorf("verify %s (source): %w", name, err)
		}
		if err := dst.QueryRow(q).Scan(&b); err != nil {
			return fmt.Errorf("verify %s: %w", name, err)
		}
		if a != b {
			log.Printf("MISMATCH %s: source %d, copy %d", name, a, b)
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d checks failed", bad)
	}
	log.Printf("verified %d checks: row counts and text lengths match", len(checks))
	return nil
}
