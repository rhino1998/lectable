package store

import (
	"database/sql"
	"io/fs"
	"os"
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
	preMigrationLibrary(t, path)

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

// preMigrationLibrary creates a library at path the way one from before
// migrations looks - the first migration's tables, no version table - with
// one bookmark in it.
func preMigrationLibrary(t *testing.T, path string) {
	t.Helper()
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
}

func TestOpenBacksUpBeforeMigrating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "library.sqlite")
	backup := filepath.Join(dir, "backups", "before.sqlite")
	preMigrationLibrary(t, path)

	s, err := OpenWith(path, Options{BackupBeforeMigrate: backup})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// The copy is the library as it was: its rows, no migration applied.
	copied, err := sql.Open(driverName, dsn(backup, true))
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	var note string
	if err := copied.QueryRow(`SELECT note FROM bookmarks WHERE id = 'm1'`).Scan(&note); err != nil || note != "Kept" {
		t.Errorf("bookmark in the copy = %q, %v; want \"Kept\"", note, err)
	}
	if v := schemaVersion(t, copied); v != 0 {
		t.Errorf("copy is at schema version %d; it was taken after migrating", v)
	}
}

func TestOpenSkipsBackupWithNothingToMigrate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "library.sqlite")

	// A new library has nothing worth copying.
	first := filepath.Join(dir, "first.sqlite")
	s, err := OpenWith(path, Options{BackupBeforeMigrate: first})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	s.Close()
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Errorf("a new library was backed up (stat: %v)", err)
	}

	// Nor does one already at the newest version.
	second := filepath.Join(dir, "second.sqlite")
	s, err = OpenWith(path, Options{BackupBeforeMigrate: second})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s.Close()
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Errorf("an up-to-date library was backed up (stat: %v)", err)
	}
}

func TestOpenRefusesToMigrateWithoutItsBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "library.sqlite")
	preMigrationLibrary(t, path)
	// A file where the backup's directory should be.
	blocker := filepath.Join(dir, "backups")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if s, err := OpenWith(path, Options{BackupBeforeMigrate: filepath.Join(blocker, "before.sqlite")}); err == nil {
		s.Close()
		t.Fatal("OpenWith succeeded without being able to write the backup")
	}

	raw, err := sql.Open(driverName, dsn(path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if v := schemaVersion(t, raw); v != 0 {
		t.Errorf("library was migrated anyway, to schema version %d", v)
	}
}

// Migration 2 gives book_settings and chapter_summaries primary keys by
// rebuilding them: every row must survive, and of rows sharing a key (which
// the old tables allowed) the newest.
func TestPrimaryKeyMigrationKeepsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite")
	preMigrationLibrary(t, path)
	old, err := sql.Open(driverName, dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO book_settings (book_id, setting, source, updated_at) VALUES ('b1', '{"era":"old"}', 'llm', 1)`,
		`INSERT INTO book_settings (book_id, setting, source, updated_at) VALUES ('b2', '{"era":"other"}', 'llm', 1)`,
		`INSERT INTO book_settings (book_id, setting, source, updated_at) VALUES ('b1', '{"era":"new"}', 'reader', 2)`,
		`INSERT INTO chapter_summaries (chapter_id, summary, updated_at) VALUES ('c1', '{"summary":"old"}', 1)`,
		`INSERT INTO chapter_summaries (chapter_id, summary, updated_at) VALUES ('c1', '{"summary":"new"}', 2)`,
		`INSERT INTO chapter_summaries (chapter_id, summary, updated_at) VALUES ('c2', '{"summary":"other"}', 1)`,
		`INSERT INTO sync_tree_nodes (book_id, chapter_idx, version, book_fp, chapter_fp, hash) VALUES ('b1', 0, 'v', 'x', 'y', 'z')`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	rows := func(query string) []string {
		t.Helper()
		r, err := s.db.Query(query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer r.Close()
		var out []string
		for r.Next() {
			var v string
			if err := r.Scan(&v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		return out
	}
	if got, want := strings.Join(rows(`SELECT book_id || ' ' || CAST(setting AS VARCHAR) || ' ' || source FROM book_settings ORDER BY book_id`), "; "),
		`b1 {"era":"new"} reader; b2 {"era":"other"} llm`; got != want {
		t.Errorf("book_settings = %s, want %s", got, want)
	}
	if got, want := strings.Join(rows(`SELECT chapter_id || ' ' || CAST(summary AS VARCHAR) FROM chapter_summaries ORDER BY chapter_id`), "; "),
		`c1 {"summary":"new"}; c2 {"summary":"other"}`; got != want {
		t.Errorf("chapter_summaries = %s, want %s", got, want)
	}
	// The keys now hold: a second row for a key is refused.
	if _, err := s.db.Exec(`INSERT INTO book_settings (book_id, setting, source, updated_at) VALUES ('b1', '{}', 'llm', 3)`); err == nil {
		t.Error("book_settings took a second row for one book")
	}
	if _, err := s.db.Exec(`INSERT INTO chapter_summaries (chapter_id, summary, updated_at) VALUES ('c1', '{}', 3)`); err == nil {
		t.Error("chapter_summaries took a second row for one chapter")
	}
	// The sync tree is a cache: emptied, and rebuilt on demand.
	if n := len(rows(`SELECT book_id FROM sync_tree_nodes`)); n != 0 {
		t.Errorf("sync_tree_nodes kept %d rows", n)
	}
	if err := s.PutSyncTreeNodes("b1", "v2", []SyncTreeNode{{ChapterIdx: 0, Hash: "h1"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSyncTreeNodes("b1", "v2", []SyncTreeNode{{ChapterIdx: 0, Hash: "h2"}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rows(`SELECT hash FROM sync_tree_nodes`), ","); got != "h2" {
		t.Errorf("sync_tree_nodes after two puts = %q, want one row h2", got)
	}
}
