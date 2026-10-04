package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// BookSetting is one book's setting profile - the world its story takes
// place in, written once per book by the setting pass
// (speakerattr.Client.DescribeSetting) and read by the ambience, lighting
// and characterization prompts. Per book, never per series: a series can
// move decades or worlds between books. Stored as JSON in
// book_settings.setting.
type BookSetting struct {
	Era        string `json:"era"`
	Technology string `json:"technology"`
	Place      string `json:"place"`
	Accent     string `json:"accent"`
	Genre      string `json:"genre"`
	// Narrator is a first-person book's narrator (the "I"), "" otherwise.
	Narrator string `json:"narrator"`
}

// BookSettingSource says who wrote a stored BookSetting.
type BookSettingSource string

const (
	// BookSettingAuto is the setting pass's own output, which a re-derive
	// replaces.
	BookSettingAuto BookSettingSource = "auto"
	// BookSettingManual is a reader's edit.
	BookSettingManual BookSettingSource = "manual"
)

// StoredBookSetting is a BookSetting as stored, with who wrote it and when.
type StoredBookSetting struct {
	Setting   BookSetting
	Source    BookSettingSource
	UpdatedAt int64
}

// GetBookSetting returns bookID's setting, nil if it has none yet.
func (s *SQLiteStore) GetBookSetting(bookID string) (*StoredBookSetting, error) {
	var raw string
	var out StoredBookSetting
	err := s.db.QueryRow(`SELECT CAST(setting AS VARCHAR), source, updated_at FROM book_settings WHERE book_id = ?`, bookID).Scan(&raw, &out.Source, &out.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &out.Setting); err != nil {
		return nil, fmt.Errorf("decode book setting: %w", err)
	}
	return &out, nil
}

// SetBookSetting stores bookID's setting, replacing any existing one.
func (s *SQLiteStore) SetBookSetting(bookID string, setting BookSetting, source BookSettingSource) error {
	raw, err := json.Marshal(setting)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO book_settings (book_id, setting, source, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (book_id) DO UPDATE SET setting = excluded.setting, source = excluded.source, updated_at = excluded.updated_at`,
		bookID, string(raw), string(source), time.Now().Unix())
	return err
}

// DeleteBookSetting removes bookID's setting, so the next pass needing it
// derives a fresh one.
func (s *SQLiteStore) DeleteBookSetting(bookID string) error {
	_, err := s.db.Exec(`DELETE FROM book_settings WHERE book_id = ?`, bookID)
	return err
}
