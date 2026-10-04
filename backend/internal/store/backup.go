package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// Backup writes a consistent copy of the database to dst through SQLite's
// online backup API, reading one snapshot on a pooled read connection:
// other readers and the writer carry on meanwhile. A page-for-page copy,
// so rowids - which paragraphs_fts is keyed by - are preserved. The copy
// is written beside dst and renamed into place, so dst is either absent or
// complete. A copy is a plain SQLite file: restore one by stopping the
// server and putting it back as library.sqlite (deleting any
// library.sqlite-wal and library.sqlite-shm).
func (s *SQLiteStore) Backup(ctx context.Context, dst string) error {
	return backupTo(ctx, s.db.read, dst)
}

// backupTo is Backup reading from src.
func backupTo(ctx context.Context, src *sql.DB, dst string) (err error) {
	tmp := dst + ".tmp"
	cleanup := func() {
		for _, f := range []string{tmp, tmp + "-wal", tmp + "-shm", tmp + "-journal"} {
			os.Remove(f)
		}
	}
	cleanup()
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	out, err := sql.Open(driverName, "file:"+tmp)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer out.Close()
	dstConn, err := out.Conn(ctx)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer dstConn.Close()
	srcConn, err := src.Conn(ctx)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer srcConn.Close()

	err = dstConn.Raw(func(d any) error {
		return srcConn.Raw(func(src any) error {
			b, err := d.(*sqlite3.SQLiteConn).Backup("main", src.(*sqlite3.SQLiteConn), "main")
			if err != nil {
				return err
			}
			if _, err := b.Step(-1); err != nil {
				b.Finish() //nolint:errcheck // the Step error is the one to report
				return err
			}
			return b.Finish()
		})
	})
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	dstConn.Close()
	if err := out.Close(); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	os.Remove(tmp + "-wal")
	os.Remove(tmp + "-shm")
	return nil
}
