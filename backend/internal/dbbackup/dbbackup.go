// Package dbbackup keeps rolling copies of the library database under
// LECTABLE_DATA_DIR/backups. Audio can be regenerated; the database's
// overrides, pronunciation rules, locked voices and finished LLM passes
// can't be without hours of GPU time, so it gets copied on a schedule.
package dbbackup

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	prefix = "library-"
	suffix = ".duckdb"
	// stampLayout sorts lexically in time order, which Prune relies on.
	stampLayout = "20060102T150405"
	// checkEvery is how often Run looks at the newest backup's age, so a
	// server restarted just before one was due doesn't wait a full Interval.
	checkEvery = time.Hour
)

// Backuper copies the live database to a path - store.DuckStore.Backup.
type Backuper interface {
	Backup(ctx context.Context, dst string) error
}

// Config says where backups go, how often, and how many to keep.
type Config struct {
	Dir      string
	Interval time.Duration // <= 0 disables backups
	Keep     int           // < 1 is treated as 1
}

// Run takes a backup whenever the newest one in cfg.Dir is at least
// cfg.Interval old (or there is none), checking once at startup and then
// hourly, and prunes all but the newest cfg.Keep after each one. Returns
// when ctx is done.
func Run(ctx context.Context, db Backuper, cfg Config) {
	if cfg.Interval <= 0 {
		log.Printf("dbbackup: disabled")
		return
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		log.Printf("dbbackup: %v", err)
		return
	}
	for {
		if due(cfg.Dir, cfg.Interval, time.Now()) {
			start := time.Now()
			path, err := Take(ctx, db, cfg.Dir, start)
			if err != nil {
				log.Printf("dbbackup: %v", err)
			} else {
				log.Printf("dbbackup: wrote %s in %s", path, time.Since(start).Round(time.Millisecond))
				if err := Prune(cfg.Dir, cfg.Keep); err != nil {
					log.Printf("dbbackup: prune: %v", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(checkEvery):
		}
	}
}

// Take writes one backup into dir, named for now.
func Take(ctx context.Context, db Backuper, dir string, now time.Time) (string, error) {
	path := filepath.Join(dir, prefix+now.Format(stampLayout)+suffix)
	if err := db.Backup(ctx, path); err != nil {
		return "", err
	}
	return path, nil
}

// Prune deletes all but the newest keep backups in dir.
func Prune(dir string, keep int) error {
	names, err := list(dir)
	if err != nil {
		return err
	}
	keep = max(keep, 1)
	if len(names) <= keep {
		return nil
	}
	for _, name := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func due(dir string, interval time.Duration, now time.Time) bool {
	names, err := list(dir)
	if err != nil || len(names) == 0 {
		return true
	}
	last, err := stamp(names[len(names)-1])
	if err != nil {
		return true
	}
	return now.Sub(last) >= interval
}

// list returns dir's backup file names, oldest first.
func list(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if _, err := stamp(e.Name()); err == nil && e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

func stamp(name string) (time.Time, error) {
	s, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return time.Time{}, fmt.Errorf("not a backup: %s", name)
	}
	s, ok = strings.CutSuffix(s, suffix)
	if !ok {
		return time.Time{}, fmt.Errorf("not a backup: %s", name)
	}
	return time.ParseInLocation(stampLayout, s, time.Local)
}
