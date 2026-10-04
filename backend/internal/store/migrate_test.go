package store

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// schemaVersion is the newest migration db has applied.
func schemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version_id), 0) FROM ` + migrationsTable + ` WHERE is_applied`).Scan(&v); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	return v
}

func migrationCount(t *testing.T) int {
	t.Helper()
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}

func TestOpenAppliesEveryMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, want := schemaVersion(t, s.db.DB), migrationCount(t); got != want {
		t.Errorf("schema version = %d, want %d", got, want)
	}
	if _, ok := s.columns[migrationsTable]; ok {
		t.Errorf("%s is listed among the library's own tables", migrationsTable)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening an up-to-date library applies nothing more.
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if got, want := schemaVersion(t, s.db.DB), migrationCount(t); got != want {
		t.Errorf("schema version after reopen = %d, want %d", got, want)
	}
}

// A library created before migrations has the first migration's tables
// and no version table: Open must adopt it without touching its rows.
func TestOpenAdoptsLibraryFromBeforeMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite")
	initial, err := fs.ReadFile(migrationFiles, "migrations/00001_initial_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, ddl, ok := strings.Cut(string(initial), "-- +goose StatementBegin")
	if !ok {
		t.Fatal("initial migration has no StatementBegin")
	}
	ddl, _, _ = strings.Cut(ddl, "-- +goose StatementEnd")

	old, err := sql.Open(driverName, dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(ddl); err != nil {
		t.Fatalf("create pre-migration library: %v", err)
	}
	if _, err := old.Exec(`INSERT INTO bookmarks (id, paragraph_id, note, created_at) VALUES ('m1', 'p1', 'Kept', 1)`); err != nil {
		t.Fatalf("insert bookmark: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if got, want := schemaVersion(t, s.db.DB), migrationCount(t); got != want {
		t.Errorf("schema version = %d, want %d", got, want)
	}
	var note string
	if err := s.db.QueryRow(`SELECT note FROM bookmarks WHERE id = 'm1'`).Scan(&note); err != nil || note != "Kept" {
		t.Errorf("bookmark after adoption = %q, %v; want \"Kept\"", note, err)
	}
}
