// Package store persists the library (books, chapters, paragraphs, and
// per-paragraph, per-voice TTS audio status) in DuckDB.
//
// Referential integrity (deleting a book's chapters/paragraphs) is handled
// explicitly in application code rather than via FK ON DELETE CASCADE, to
// stay on the simplest, most portable subset of DuckDB's SQL support.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	_ "github.com/duckdb/duckdb-go/v2"
)

type DuckStore struct {
	db *notifyDB
}

func Open(path string) (*DuckStore, error) {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, err
	}
	// DuckDB is single-writer; one connection keeps that explicit and avoids
	// any cross-connection lock contention. This app has no need for write
	// concurrency.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateCharactersPronouns(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateLexiconPrefix(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateMusicRegionsLighting(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`INSERT INTO default_voice (id) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM default_voice)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed default_voice: %w", err)
	}
	return &DuckStore{db: &notifyDB{DB: db}}, nil
}

func (s *DuckStore) Close() error { return s.db.Close() }

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
