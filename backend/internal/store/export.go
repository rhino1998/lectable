package store

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

// ExportTables is every table a book export carries (internal/bookexport),
// in the order ImportBookRows inserts them. sync_tree_nodes (a derived
// cache) and default_voice (instance-wide, not the book's) are left out.
var ExportTables = []string{
	"voice_presets", "characters", "character_voices",
	"books", "chapters", "paragraphs", "paragraph_audio",
	"images", "breaks", "bookmarks", "sfx", "music_regions",
}

// sharedExportTables are rows an importing library may already hold - a
// custom preset or a series' character roster imported before, or built
// up by another book in the same series. ImportBookRows keeps an existing
// row instead of failing on it.
var sharedExportTables = []string{"voice_presets", "characters", "character_voices"}

// exportRowQueries selects each ExportTables table's rows for one book:
// params are the book id, then characterScope (the book's SeriesScope),
// repeated as each query's placeholders need. Rows come back as DuckDB's
// own to_json of the whole row, so a column added to the schema later is
// carried without touching this.
var exportRowQueries = map[string]struct {
	where string
	args  func(bookID, scope string) []any
}{
	"books":            {`id = ?`, bookArg},
	"chapters":         {`book_id = ?`, bookArg},
	"paragraphs":       {`chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, bookArg},
	"paragraph_audio":  {`paragraph_id IN (SELECT p.id FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id WHERE c.book_id = ?)`, bookArg},
	"images":           {`chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, bookArg},
	"breaks":           {`chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, bookArg},
	"music_regions":    {`chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, bookArg},
	"bookmarks":        {`paragraph_id IN (SELECT p.id FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id WHERE c.book_id = ?)`, bookArg},
	"sfx":              {`paragraph_id IN (SELECT p.id FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id WHERE c.book_id = ?)`, bookArg},
	"characters":       {`scope = ?`, scopeArg},
	"character_voices": {`character_id IN (SELECT id FROM characters WHERE scope = ?)`, scopeArg},
	// The book's own preset plus every preset its characters are assigned
	// under any clone model. Built-in presets have no row (they're compiled
	// in), so the book's own id matching nothing is normal.
	"voice_presets": {`id = (SELECT voice_preset_id FROM books WHERE id = ?) OR id IN (SELECT cv.voice_preset_id FROM character_voices cv JOIN characters ch ON ch.id = cv.character_id WHERE ch.scope = ?)`, bothArgs},
}

func bookArg(bookID, _ string) []any      { return []any{bookID} }
func scopeArg(_, scope string) []any      { return []any{scope} }
func bothArgs(bookID, scope string) []any { return []any{bookID, scope} }

// ExportBookRows returns every ExportTables row belonging to bookID as one
// JSON object per row, keyed by table. characterScope is the book's
// SeriesScope - the roster is series-wide, so every character of the
// series comes along, not just the ones this book's lines name.
func (s *DuckStore) ExportBookRows(bookID, characterScope string) (map[string][]json.RawMessage, error) {
	out := make(map[string][]json.RawMessage, len(ExportTables))
	for _, table := range ExportTables {
		q := exportRowQueries[table]
		rows, err := s.db.Query(`SELECT to_json(t)::VARCHAR FROM `+table+` t WHERE `+q.where, q.args(bookID, characterScope)...)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", table, err)
		}
		var list []json.RawMessage
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				return nil, fmt.Errorf("export %s: %w", table, err)
			}
			list = append(list, json.RawMessage(row))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", table, err)
		}
		out[table] = list
	}
	return out, nil
}

// ExportFingerprints is SyncFingerprints widened to everything a book
// export contains, and scoped to the book: the book row (minus reading
// position and the length estimate), its series' characters and their
// voice assignments and every custom preset; and per chapter its row,
// paragraphs, all paragraph_audio (word timings included - a word-level
// export uses them), images, breaks, SFX, music regions and bookmarks.
// Over-covering is harmless: it can only mark an export stale needlessly.
func (s *DuckStore) ExportFingerprints(bookID, characterScope string) (book string, chapters map[int]string, err error) {
	err = s.db.QueryRow(`
		SELECT concat_ws('|',
			coalesce((SELECT hash(b) FROM (SELECT * EXCLUDE (pos_chapter_idx, pos_paragraph_idx, pos_seconds, estimate_sec_per_char) FROM books WHERE id = ?) b)::VARCHAR, ''),
			coalesce((SELECT bit_xor(hash(t)) FROM characters t WHERE scope = ?)::VARCHAR, ''),
			coalesce((SELECT bit_xor(hash(t)) FROM character_voices t WHERE character_id IN (SELECT id FROM characters WHERE scope = ?))::VARCHAR, ''),
			coalesce((SELECT bit_xor(hash(t)) FROM voice_presets t)::VARCHAR, ''))`,
		bookID, characterScope, characterScope).Scan(&book)
	if err != nil {
		return "", nil, err
	}

	rows, err := s.db.Query(`
		WITH ch AS (SELECT * FROM chapters WHERE book_id = ?),
		pg AS (SELECT * FROM paragraphs WHERE chapter_id IN (SELECT id FROM ch))
		SELECT ch.idx, concat_ws('|', hash(ch)::VARCHAR, coalesce(pp.fp::VARCHAR, ''), coalesce(pa.fp::VARCHAR, ''),
			coalesce(im.fp::VARCHAR, ''), coalesce(br.fp::VARCHAR, ''), coalesce(sx.fp::VARCHAR, ''),
			coalesce(mr.fp::VARCHAR, ''), coalesce(bm.fp::VARCHAR, ''))
		FROM ch
		LEFT JOIN (SELECT chapter_id, bit_xor(hash(p)) AS fp FROM pg p GROUP BY chapter_id) pp ON pp.chapter_id = ch.id
		LEFT JOIN (SELECT p.chapter_id, bit_xor(hash(a)) AS fp FROM paragraph_audio a
			JOIN pg p ON p.id = a.paragraph_id GROUP BY p.chapter_id) pa ON pa.chapter_id = ch.id
		LEFT JOIN (SELECT chapter_id, bit_xor(hash(i)) AS fp FROM images i
			WHERE chapter_id IN (SELECT id FROM ch) GROUP BY chapter_id) im ON im.chapter_id = ch.id
		LEFT JOIN (SELECT chapter_id, bit_xor(hash(k)) AS fp FROM breaks k
			WHERE chapter_id IN (SELECT id FROM ch) GROUP BY chapter_id) br ON br.chapter_id = ch.id
		LEFT JOIN (SELECT p.chapter_id, bit_xor(hash(x)) AS fp FROM sfx x
			JOIN pg p ON p.id = x.paragraph_id GROUP BY p.chapter_id) sx ON sx.chapter_id = ch.id
		LEFT JOIN (SELECT chapter_id, bit_xor(hash(m)) AS fp FROM music_regions m
			WHERE chapter_id IN (SELECT id FROM ch) GROUP BY chapter_id) mr ON mr.chapter_id = ch.id
		LEFT JOIN (SELECT p.chapter_id, bit_xor(hash(b)) AS fp FROM bookmarks b
			JOIN pg p ON p.id = b.paragraph_id GROUP BY p.chapter_id) bm ON bm.chapter_id = ch.id`, bookID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	chapters = map[int]string{}
	for rows.Next() {
		var idx int
		var fp string
		if err := rows.Scan(&idx, &fp); err != nil {
			return "", nil, err
		}
		chapters[idx] = fp
	}
	return book, chapters, rows.Err()
}

// ImportBookRows inserts rows (ExportBookRows' shape, from a book export)
// in one transaction. Only columns both the export and this schema have
// are inserted - a column this schema added since takes its DEFAULT, one
// it dropped is ignored. Rows of sharedExportTables already present (by
// primary key or unique constraint) are kept as they are; any other
// conflict - the book already exists - fails the whole import. Callers
// remap ids beforehand where a shared row is matched by something other
// than its id (a series character by name).
func (s *DuckStore) ImportBookRows(rows map[string][]json.RawMessage) error {
	for table := range rows {
		if !slices.Contains(ExportTables, table) {
			return fmt.Errorf("import: unknown table %q", table)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	for _, table := range ExportTables {
		list := rows[table]
		if len(list) == 0 {
			continue
		}
		colTypes, err := tableColumnTypes(tx, table)
		if err != nil {
			return err
		}
		cols := importColumns(list, colTypes)
		if len(cols) == 0 {
			continue
		}
		path, err := writeJSONLines(list)
		if err != nil {
			return err
		}
		spec := make([]string, len(cols))
		quoted := make([]string, len(cols))
		for i, c := range cols {
			spec[i] = sqlString(c) + ": " + sqlString(colTypes[c])
			quoted[i] = `"` + c + `"`
		}
		q := `INSERT INTO ` + table + ` BY NAME SELECT ` + strings.Join(quoted, ", ") +
			` FROM read_json(` + sqlString(path) + `, format = 'newline_delimited', columns = {` + strings.Join(spec, ", ") + `})`
		if slices.Contains(sharedExportTables, table) {
			q += ` ON CONFLICT DO NOTHING`
		}
		_, err = tx.Exec(q)
		os.Remove(path)
		if err != nil {
			return fmt.Errorf("import %s: %w", table, err)
		}
	}
	return tx.Commit()
}

// tableColumnTypes maps table's columns to their DuckDB type names.
func tableColumnTypes(tx *notifyTx, table string) (map[string]string, error) {
	rows, err := tx.Query(`SELECT column_name, data_type FROM information_schema.columns WHERE table_name = ?`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		out[name] = typ
	}
	return out, rows.Err()
}

// importColumns is every column some row in list sets that the table
// also has, sorted.
func importColumns(list []json.RawMessage, colTypes map[string]string) []string {
	seen := map[string]bool{}
	for _, raw := range list {
		var row map[string]json.RawMessage
		if json.Unmarshal(raw, &row) != nil {
			continue
		}
		for k := range row {
			if _, ok := colTypes[k]; ok {
				seen[k] = true
			}
		}
	}
	cols := make([]string, 0, len(seen))
	for c := range seen {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	return cols
}

func writeJSONLines(list []json.RawMessage) (string, error) {
	f, err := os.CreateTemp("", "lectable-import-*.jsonl")
	if err != nil {
		return "", err
	}
	for _, raw := range list {
		if _, err := f.Write(append(append([]byte(nil), raw...), '\n')); err != nil {
			f.Close()
			os.Remove(f.Name())
			return "", err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// sqlString quotes s as a SQL string literal.
func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
