package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBackupRestores(t *testing.T) {
	s := openTestStore(t)
	bookID, _ := oneChapterBook(t, s, "", 0, "One.", "Two.")

	dst := filepath.Join(t.TempDir(), "copy.sqlite")
	if err := s.Backup(context.Background(), dst); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// The store stays usable after a backup.
	if _, err := s.GetBook(bookID); err != nil {
		t.Fatalf("GetBook after backup: %v", err)
	}

	restored, err := Open(dst)
	if err != nil {
		t.Fatalf("Open backup: %v", err)
	}
	defer restored.Close()
	b, err := restored.GetBook(bookID)
	if err != nil {
		t.Fatalf("GetBook from backup: %v", err)
	}
	if b.Title != "Test Book" {
		t.Errorf("restored title = %q", b.Title)
	}
}
