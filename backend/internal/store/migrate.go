package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
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
// already, which applies to it as a no-op.
func migrate(db *sql.DB) error {
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, files, goose.WithTableName(migrationsTable))
	if err != nil {
		return err
	}
	results, err := provider.Up(context.Background())
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
