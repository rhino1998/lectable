package store

import (
	"context"
	"fmt"
	"io"
	"os"
)

// Backup writes a consistent copy of the database file to dst. It holds
// the store's only connection for the duration - every other query waits -
// checkpoints so the file alone holds every committed write, then copies
// the file byte for byte. The copy is written beside dst and renamed into
// place, so dst is either absent or complete. A copy is a plain DuckDB
// file: restore one by stopping the server and putting it back as
// library.duckdb (deleting any library.duckdb.wal).
func (s *DuckStore) Backup(ctx context.Context, dst string) error {
	conn, err := s.db.DB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CHECKPOINT`); err != nil {
		return fmt.Errorf("backup: checkpoint: %w", err)
	}
	return copyFile(s.path, dst)
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return fmt.Errorf("backup: copy: %w", err)
	}
	if err = out.Sync(); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err = out.Close(); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err = os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}
