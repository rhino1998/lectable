package store

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"testing"
)

// For every single-column secondary index, compares per-value row counts
// from a full scan against an indexed equality lookup.
func TestCheckLiveCopyIndexes(t *testing.T) {
	path := os.Getenv("LIVE_DB_COPY")
	if path == "" {
		t.Skip("LIVE_DB_COPY unset")
	}
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.Query(`SELECT index_name, table_name, sql FROM duckdb_indexes()`)
	if err != nil {
		t.Fatal(err)
	}
	type ix struct{ name, table, sql string }
	var ixs []ix
	for rows.Next() {
		var i ix
		_ = rows.Scan(&i.name, &i.table, &i.sql)
		ixs = append(ixs, i)
	}
	rows.Close()
	colRe := regexp.MustCompile(`\("?([a-z_]+)"?\)`)
	for _, i := range ixs {
		m := colRe.FindStringSubmatch(i.sql)
		if m == nil {
			t.Logf("%s: skipped (not single-column): %s", i.name, i.sql)
			continue
		}
		col := m[1]
		vals, err := db.Query(fmt.Sprintf(`SELECT CAST(%s AS VARCHAR), count(*) FROM %s GROUP BY 1`, col, i.table))
		if err != nil {
			t.Fatal(err)
		}
		type vc struct {
			v sql.NullString
			n int
		}
		var all []vc
		for vals.Next() {
			var x vc
			_ = vals.Scan(&x.v, &x.n)
			all = append(all, x)
		}
		vals.Close()
		bad, missing := 0, 0
		for _, x := range all {
			if !x.v.Valid {
				continue
			}
			var got int
			if err := db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = ?`, i.table, col), x.v.String).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != x.n {
				bad++
				missing += x.n - got
			}
		}
		t.Logf("%s on %s(%s): %d distinct values, %d mismatched, %d rows hidden", i.name, i.table, col, len(all), bad, missing)
	}
}

// Drops and recreates every secondary index from its own stored SQL.
func TestRepairLiveIndexes(t *testing.T) {
	path := os.Getenv("REPAIR_DB")
	if path == "" {
		t.Skip("REPAIR_DB unset")
	}
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.Query(`SELECT index_name, sql FROM duckdb_indexes()`)
	if err != nil {
		t.Fatal(err)
	}
	type ix struct{ name, sql string }
	var ixs []ix
	for rows.Next() {
		var i ix
		_ = rows.Scan(&i.name, &i.sql)
		ixs = append(ixs, i)
	}
	rows.Close()
	for _, i := range ixs {
		if _, err := db.Exec(`DROP INDEX ` + i.name); err != nil {
			t.Fatalf("drop %s: %v", i.name, err)
		}
		if _, err := db.Exec(i.sql); err != nil {
			t.Fatalf("recreate %s (%s): %v", i.name, i.sql, err)
		}
		t.Logf("rebuilt %s", i.name)
	}
	if _, err := db.Exec(`CHECKPOINT`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}
