// Package store persists the library (books, chapters, paragraphs, and
// per-paragraph, per-voice TTS audio status) in SQLite.
//
// Referential integrity (deleting a book's chapters/paragraphs) is handled
// explicitly in application code rather than via FK ON DELETE CASCADE.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"runtime"
	"strings"
)

type SQLiteStore struct {
	db   *notifyDB
	path string
	// columns is every table's columns in declaration order, read once at
	// Open - what the row fingerprints (sync.go) and row export (export.go)
	// build their column lists from, so a column added to the schema is
	// covered without touching either.
	columns map[string][]tableColumn
}

// tableColumn is one column's name and declared type ("TEXT", "JSON",
// "BOOLEAN", ...), upper-cased.
type tableColumn struct {
	Name string
	Type string
}

// dsn builds a connection string for path. Every connection runs in WAL
// mode, so readers never wait on the writer (or each other) and the
// writer never waits on readers.
func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "NORMAL")
	q.Set("_busy_timeout", "10000")
	q.Set("_foreign_keys", "0")
	if readOnly {
		q.Set("_query_only", "1")
	} else {
		// Take the write lock at BEGIN rather than at the first write, so a
		// transaction that reads then writes can't fail on a stale snapshot.
		q.Set("_txlock", "immediate")
	}
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// Open opens (creating if needed) the SQLite library at path and applies
// any migrations it hasn't had (see migrate): one write
// connection, since SQLite allows one writer at a time, plus a pool of
// read-only connections that Query/QueryRow outside a transaction use.
func Open(path string) (*SQLiteStore, error) {
	write, err := sql.Open(driverName, dsn(path, false))
	if err != nil {
		return nil, err
	}
	write.SetMaxOpenConns(1)

	if err := migrate(write); err != nil {
		write.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if _, err := write.Exec(`INSERT INTO default_voice (id) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM default_voice)`); err != nil {
		write.Close()
		return nil, fmt.Errorf("seed default_voice: %w", err)
	}
	columns, err := loadColumns(write)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("read schema: %w", err)
	}

	read, err := sql.Open(driverName, dsn(path, true))
	if err != nil {
		write.Close()
		return nil, err
	}
	read.SetMaxOpenConns(max(4, runtime.NumCPU()))
	return &SQLiteStore{db: &notifyDB{DB: write, read: read}, path: path, columns: columns}, nil
}

func loadColumns(db *sql.DB) (map[string][]tableColumn, error) {
	rows, err := db.Query(`SELECT m.name, p.name, p.type FROM sqlite_master m JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND m.name NOT LIKE 'paragraphs_fts%' AND m.name <> '` + migrationsTable + `' ORDER BY m.name, p.cid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]tableColumn{}
	for rows.Next() {
		var table string
		var c tableColumn
		if err := rows.Scan(&table, &c.Name, &c.Type); err != nil {
			return nil, err
		}
		c.Type = strings.ToUpper(c.Type)
		out[table] = append(out[table], c)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Close() error {
	s.db.DB.Exec(`PRAGMA optimize`) //nolint:errcheck // best-effort statistics refresh

	rerr := s.db.read.Close()
	if err := s.db.DB.Close(); err != nil {
		return err
	}
	return rerr
}

func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RandomSeed picks a seed for a new custom voice preset - just needs to be
// some fixed positive int that anchors one realization of that preset's
// instruct-conditioned voice distribution; which one doesn't matter.
func RandomSeed() int {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return (int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])) & 0x7fffffff
}
