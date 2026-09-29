package dbbackup

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type fakeDB struct{}

func (fakeDB) Backup(_ context.Context, dst string) error {
	return os.WriteFile(dst, []byte("db"), 0o644)
}

func TestTakeAndPrune(t *testing.T) {
	dir := t.TempDir()
	// Unrelated files in the directory are never touched.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 3, 0, 0, 0, time.Local)
	for i := range 5 {
		if _, err := Take(context.Background(), fakeDB{}, dir, base.Add(time.Duration(i)*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := Prune(dir, 2); err != nil {
		t.Fatal(err)
	}
	names, err := list(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"library-20260904T030000.duckdb", "library-20260905T030000.duckdb"}
	if !slices.Equal(names, want) {
		t.Errorf("after prune: %v, want %v", names, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Errorf("unrelated file removed: %v", err)
	}
}

func TestDue(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.Local)
	if !due(dir, 24*time.Hour, now) {
		t.Error("empty dir should be due")
	}
	if _, err := Take(context.Background(), fakeDB{}, dir, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if due(dir, 24*time.Hour, now) {
		t.Error("2h-old backup should not be due at a 24h interval")
	}
	if !due(dir, time.Hour, now) {
		t.Error("2h-old backup should be due at a 1h interval")
	}
}
