package semantic

import (
	"database/sql"
	"fmt"
	"hash/fnv"
	"math"
	"strings"

	_ "github.com/mattn/go-sqlite3" // the "sqlite3" driver
)

// vectorScale is what a unit vector's components are multiplied by to be
// stored one byte each. The model's components stay under 0.3 in size, so
// 400 uses the byte's range where x127 would use a third of it; the rare
// larger one is clipped.
const vectorScale = 400

// quantize packs a unit vector one signed byte a component.
func quantize(v []float32) []byte {
	out := make([]byte, len(v))
	for i, f := range v {
		q := math.Round(float64(f) * vectorScale)
		out[i] = byte(int8(max(-127, min(127, q))))
	}
	return out
}

// textHash identifies a paragraph's text, to tell whether its stored
// vector is still for what it says.
func textHash(text string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	return int64(h.Sum64())
}

// index is embeddings.sqlite: one row per embedded paragraph, and per
// chapter the chapter_text rev it was last brought up to. Everything in it
// is derived from the library and the model; deleting the file loses
// nothing but the time to embed again.
type index struct {
	db *sql.DB
}

const indexSchema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS chapters (
	chapter_id TEXT PRIMARY KEY,
	book_id TEXT NOT NULL,
	rev INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS vectors (
	paragraph_id TEXT PRIMARY KEY,
	chapter_id TEXT NOT NULL,
	book_id TEXT NOT NULL,
	hash INTEGER NOT NULL,
	vec BLOB NOT NULL
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS vectors_book ON vectors (book_id);
CREATE INDEX IF NOT EXISTS vectors_chapter ON vectors (chapter_id);
`

// openIndex opens (creating if needed) the index at path. model names the
// embedding model: vectors from another one can't be compared with its,
// so an index made with a different model is emptied.
func openIndex(path, model string) (*index, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(indexSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("semantic: create index schema: %w", err)
	}
	var had string
	switch err := db.QueryRow(`SELECT value FROM meta WHERE key = 'model'`).Scan(&had); {
	case err == sql.ErrNoRows:
	case err != nil:
		_ = db.Close()
		return nil, err
	}
	if had != model {
		if _, err := db.Exec(`DELETE FROM vectors; DELETE FROM chapters; INSERT OR REPLACE INTO meta (key, value) VALUES ('model', ?)`, model); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("semantic: reset index for model %s: %w", model, err)
		}
	}
	return &index{db: db}, nil
}

func (x *index) close() error { return x.db.Close() }

// chapterRevs maps each indexed chapter of bookID to the rev it was
// brought up to.
func (x *index) chapterRevs(bookID string) (map[string]int64, error) {
	rows, err := x.db.Query(`SELECT chapter_id, rev FROM chapters WHERE book_id = ?`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var rev int64
		if err := rows.Scan(&id, &rev); err != nil {
			return nil, err
		}
		out[id] = rev
	}
	return out, rows.Err()
}

// allChapterRevs is chapterRevs for every chapter in the index.
func (x *index) allChapterRevs() (map[string]int64, error) {
	rows, err := x.db.Query(`SELECT chapter_id, rev FROM chapters`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var rev int64
		if err := rows.Scan(&id, &rev); err != nil {
			return nil, err
		}
		out[id] = rev
	}
	return out, rows.Err()
}

// hashes maps each of chapterID's embedded paragraphs to its text hash.
func (x *index) hashes(chapterID string) (map[string]int64, error) {
	rows, err := x.db.Query(`SELECT paragraph_id, hash FROM vectors WHERE chapter_id = ?`, chapterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var hash int64
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, err
		}
		out[id] = hash
	}
	return out, rows.Err()
}

// row is one paragraph's vector to store.
type row struct {
	paragraphID string
	hash        int64
	vec         []byte
}

// saveChapter stores what a pass over one chapter produced: add (new and
// re-embedded paragraphs), remove (ones no longer embedded), and the rev
// the chapter is now up to.
func (x *index) saveChapter(bookID, chapterID string, rev int64, add []row, remove []string) error {
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range add {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO vectors (paragraph_id, chapter_id, book_id, hash, vec) VALUES (?, ?, ?, ?, ?)`,
			r.paragraphID, chapterID, bookID, r.hash, r.vec); err != nil {
			return err
		}
	}
	for _, id := range remove {
		if _, err := tx.Exec(`DELETE FROM vectors WHERE paragraph_id = ?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO chapters (chapter_id, book_id, rev) VALUES (?, ?, ?)`, chapterID, bookID, rev); err != nil {
		return err
	}
	return tx.Commit()
}

// dropChapters forgets chapters that no longer exist, vectors included.
func (x *index) dropChapters(chapterIDs []string) error {
	if len(chapterIDs) == 0 {
		return nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(chapterIDs)), ",")
	args := make([]any, len(chapterIDs))
	for i, id := range chapterIDs {
		args[i] = id
	}
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM vectors WHERE chapter_id IN (`+marks+`)`, args...); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM chapters WHERE chapter_id IN (`+marks+`)`, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// dropBook forgets a book.
func (x *index) dropBook(bookID string) error {
	_, err := x.db.Exec(`DELETE FROM vectors WHERE book_id = ?; DELETE FROM chapters WHERE book_id = ?`, bookID, bookID)
	return err
}

// bookIDs lists every book the index holds anything for.
func (x *index) bookIDs() ([]string, error) {
	rows, err := x.db.Query(`SELECT DISTINCT book_id FROM chapters`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// bookVectors is one book's vectors in memory, for scanning: paragraph i
// is ids[i] in chapter chapters[i], its vector data[i*dim:(i+1)*dim].
type bookVectors struct {
	dim      int
	ids      []string
	chapters []string
	data     []int8
}

// load reads bookID's vectors.
func (x *index) load(bookID string) (*bookVectors, error) {
	rows, err := x.db.Query(`SELECT paragraph_id, chapter_id, vec FROM vectors WHERE book_id = ?`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bv := &bookVectors{}
	var raw sql.RawBytes
	for rows.Next() {
		var id, chapterID string
		if err := rows.Scan(&id, &chapterID, &raw); err != nil {
			return nil, err
		}
		if bv.dim == 0 {
			bv.dim = len(raw)
		}
		if len(raw) != bv.dim || bv.dim == 0 {
			continue
		}
		bv.ids = append(bv.ids, id)
		bv.chapters = append(bv.chapters, chapterID)
		for _, b := range raw {
			bv.data = append(bv.data, int8(b))
		}
	}
	return bv, rows.Err()
}
