package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pressly/goose/v3"
)

// migrationFiles is the library's schema history, one goose SQL migration
// per file (NNNNN_name.sql), applied in order by migrate. A schema change
// is a new file here; one a library has applied is never edited.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationsTable is where goose records the versions db has applied.
const migrationsTable = "goose_db_version"

// migrate brings db up to the newest migration, each in its own
// transaction, so one that fails leaves the library at the version before
// it. A library from before migrations has every table of the first one
// already, which applies to it as a no-op. When any are pending on a
// library that already holds tables, it is first copied to backupPath (if
// set): migrations only go forward.
func migrate(db *sql.DB, backupPath string) error {
	ctx := context.Background()
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, files, goose.WithTableName(migrationsTable))
	if err != nil {
		return err
	}
	if backupPath != "" {
		if err := backupBeforeMigrate(ctx, db, provider, backupPath); err != nil {
			return fmt.Errorf("backup before migrating: %w", err)
		}
	}
	results, err := provider.Up(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "no such module: fts5") {
			return fmt.Errorf("%w (build with -tags sqlite_fts5)", err)
		}
		return err
	}
	for _, r := range results {
		log.Printf("store: applied migration %s in %s", r.Source.Path, r.Duration.Round(1e6))
	}
	return nil
}

// backupBeforeMigrate copies db to path if it has migrations pending and
// isn't a new, empty library.
func backupBeforeMigrate(ctx context.Context, db *sql.DB, provider *goose.Provider, path string) error {
	// Counted before asking goose anything: that creates its version table.
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> ?`, migrationsTable).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		return nil
	}
	pending, err := provider.HasPending(ctx)
	if err != nil || !pending {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := backupTo(ctx, db, path); err != nil {
		return err
	}
	log.Printf("store: migrations pending, copied the library to %s first", path)
	return nil
}
