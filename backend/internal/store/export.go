package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ExportTables is every table a book export carries (internal/bookexport),
// in the order ImportBookRows inserts them. sync_tree_nodes (a derived
// cache) and default_voice (instance-wide, not the book's) are left out.
var ExportTables = []string{
	"voice_presets", "characters", "character_voices",
	"books", "book_settings", "chapters", "chapter_summaries", "paragraphs", "paragraph_audio",
	"images", "breaks", "bookmarks", "sfx", "music_regions", "paragraph_edits",
}

// sharedExportTables are rows an importing library may already hold - a
// custom preset or a series' character roster imported before, or built
// up by another book in the same series. ImportBookRows keeps an existing
// row instead of failing on it.
var sharedExportTables = []string{"voice_presets", "characters", "character_voices"}

// exportRowQueries selects each ExportTables table's rows for one book:
// params are the book id, then characterScope (the book's SeriesScope),
// repeated as each query's placeholders need. Rows come back as a JSON
// object of the whole row (rowJSON), so a column added to the schema later
// is carried without touching this.
var exportRowQueries = map[string]struct {
	where string
	args  func(bookID, scope string) []any
}{
	"books":             {`id = ?`, bookArg},
	"book_settings":     {`book_id = ?`, bookArg},
	"chapter_summaries": {`chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, bookArg},
	"chapters":          {`book_id = ?`, bookArg},
	"paragraphs":        {`chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, bookArg},
	"paragraph_audio":   {`paragraph_id IN (SELECT p.id FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id WHERE c.book_id = ?)`, bookArg},
	"images":            {`chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, bookArg},
	"breaks":            {`chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, bookArg},
	"music_regions":     {`chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, bookArg},
	"bookmarks":         {`paragraph_id IN (SELECT p.id FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id WHERE c.book_id = ?)`, bookArg},
	"paragraph_edits":   {`paragraph_id IN (SELECT p.id FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id WHERE c.book_id = ?)`, bookArg},
	"sfx":               {`paragraph_id IN (SELECT p.id FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id WHERE c.book_id = ?)`, bookArg},
	"characters":        {`scope = ?`, scopeArg},
	"character_voices":  {`character_id IN (SELECT id FROM characters WHERE scope = ?)`, scopeArg},
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
func (s *SQLiteStore) ExportBookRows(bookID, characterScope string) (map[string][]json.RawMessage, error) {
	out := make(map[string][]json.RawMessage, len(ExportTables))
	for _, table := range ExportTables {
		q := exportRowQueries[table]
		rows, err := s.db.Query(`SELECT `+s.rowJSON(table)+` FROM `+table+` t WHERE `+q.where, q.args(bookID, characterScope)...)
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
func (s *SQLiteStore) ExportFingerprints(bookID, characterScope string) (book string, chapters map[int]string, err error) {
	h := s.rowHash
	err = s.db.QueryRow(`
		SELECT concat_ws('|',
			coalesce((SELECT `+h("books", "b", bookVolatileColumns...)+` FROM books b WHERE id = ?), ''),
			coalesce((SELECT bit_xor(`+h("characters", "t")+`) FROM characters t WHERE scope = ?), ''),
			coalesce((SELECT bit_xor(`+h("character_voices", "t")+`) FROM character_voices t WHERE character_id IN (SELECT id FROM characters WHERE scope = ?)), ''),
			coalesce((SELECT bit_xor(`+h("voice_presets", "t")+`) FROM voice_presets t), ''))`,
		bookID, characterScope, characterScope).Scan(&book)
	if err != nil {
		return "", nil, err
	}

	rows, err := s.db.Query(`
		WITH ch AS (SELECT * FROM chapters WHERE book_id = ?),
		pg AS (SELECT * FROM paragraphs WHERE chapter_id IN (SELECT id FROM ch))
		SELECT ch.idx, concat_ws('|', `+h("chapters", "ch")+`, coalesce(pp.fp, ''), coalesce(pa.fp, ''),
			coalesce(im.fp, ''), coalesce(br.fp, ''), coalesce(sx.fp, ''),
			coalesce(mr.fp, ''), coalesce(bm.fp, ''))
		FROM ch
		LEFT JOIN (SELECT chapter_id, bit_xor(`+h("paragraphs", "p")+`) AS fp FROM pg p GROUP BY chapter_id) pp ON pp.chapter_id = ch.id
		LEFT JOIN (SELECT p.chapter_id, bit_xor(`+h("paragraph_audio", "a")+`) AS fp FROM paragraph_audio a
			JOIN pg p ON p.id = a.paragraph_id GROUP BY p.chapter_id) pa ON pa.chapter_id = ch.id
		LEFT JOIN (SELECT chapter_id, bit_xor(`+h("images", "i")+`) AS fp FROM images i
			WHERE chapter_id IN (SELECT id FROM ch) GROUP BY chapter_id) im ON im.chapter_id = ch.id
		LEFT JOIN (SELECT chapter_id, bit_xor(`+h("breaks", "k")+`) AS fp FROM breaks k
			WHERE chapter_id IN (SELECT id FROM ch) GROUP BY chapter_id) br ON br.chapter_id = ch.id
		LEFT JOIN (SELECT p.chapter_id, bit_xor(`+h("sfx", "x")+`) AS fp FROM sfx x
			JOIN pg p ON p.id = x.paragraph_id GROUP BY p.chapter_id) sx ON sx.chapter_id = ch.id
		LEFT JOIN (SELECT chapter_id, bit_xor(`+h("music_regions", "m")+`) AS fp FROM music_regions m
			WHERE chapter_id IN (SELECT id FROM ch) GROUP BY chapter_id) mr ON mr.chapter_id = ch.id
		LEFT JOIN (SELECT p.chapter_id, bit_xor(`+h("bookmarks", "b")+`) AS fp FROM bookmarks b
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

// rowJSON is the SQL expression rendering one row of table (aliased t) as
// a JSON object: a JSON column nested as JSON rather than as a string, a
// BOOLEAN as true/false, everything else as stored.
func (s *SQLiteStore) rowJSON(table string) string {
	var parts []string
	for _, c := range s.columns[table] {
		col := `t."` + c.Name + `"`
		switch c.Type {
		case "JSON":
			col = "json(" + col + ")"
		case "BOOLEAN":
			col = "json(CASE WHEN " + col + " IS NULL THEN 'null' WHEN " + col + " THEN 'true' ELSE 'false' END)"
		}
		parts = append(parts, sqlString(c.Name)+", "+col)
	}
	return "json_object(" + strings.Join(parts, ", ") + ")"
}

// ImportBookRows inserts rows (ExportBookRows' shape, from a book export)
// in one transaction. Only columns both the export and this schema have
// are inserted - a column this schema added since takes its DEFAULT, one
// it dropped is ignored. Rows of sharedExportTables already present (by
// primary key or unique constraint) are kept as they are; any other
// conflict - the book already exists - fails the whole import. Callers
// remap ids beforehand where a shared row is matched by something other
// than its id (a series character by name).
func (s *SQLiteStore) ImportBookRows(rows map[string][]json.RawMessage) error {
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
		if err := s.insertJSONRows(tx, table, rows[table], slices.Contains(sharedExportTables, table)); err != nil {
			return fmt.Errorf("import %s: %w", table, err)
		}
	}
	// An export from before paragraph_audio had them carries no
	// chapter_id/chars.
	if _, err := tx.Exec(backfillAudioChapters); err != nil {
		return fmt.Errorf("import paragraph_audio: %w", err)
	}
	return tx.Commit()
}

// backfillAudioChapters fills paragraph_audio's chapter_id/chars on rows
// that lack them and whose paragraph exists.
const backfillAudioChapters = `UPDATE paragraph_audio SET
		chapter_id = (SELECT p.chapter_id FROM paragraphs p WHERE p.id = paragraph_id),
		chars = (SELECT length(p.content) FROM paragraphs p WHERE p.id = paragraph_id)
	WHERE chapter_id = '' AND EXISTS (SELECT 1 FROM paragraphs p WHERE p.id = paragraph_id)`

// BackfillAudioChapters runs backfillAudioChapters over the whole library,
// for cmd/duck2sqlite: the DuckDB schema had neither column.
func (s *SQLiteStore) BackfillAudioChapters() (int64, error) {
	res, err := s.db.Exec(backfillAudioChapters)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Tables lists every table of the schema with its column names, for
// cmd/duck2sqlite.
func (s *SQLiteStore) Tables() map[string][]string {
	out := make(map[string][]string, len(s.columns))
	for table, cols := range s.columns {
		for _, c := range cols {
			out[table] = append(out[table], c.Name)
		}
	}
	return out
}

// LoadTable replaces table's whole contents with rows (one value per cols
// entry each) in one transaction - what cmd/duck2sqlite fills a migrated
// library through.
func (s *SQLiteStore) LoadTable(table string, cols []string, rows [][]any) error {
	known := s.columns[table]
	for _, c := range cols {
		if !slices.ContainsFunc(known, func(k tableColumn) bool { return k.Name == c }) {
			return fmt.Errorf("load %s: unknown column %q", table, c)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
		return fmt.Errorf("load %s: %w", table, err)
	}
	st, err := tx.Prepare(`INSERT INTO ` + table + ` ("` + strings.Join(cols, `", "`) + `") VALUES (?` + strings.Repeat(", ?", len(cols)-1) + `)`)
	if err != nil {
		return fmt.Errorf("load %s: %w", table, err)
	}
	defer st.Close()
	for i, row := range rows {
		if _, err := st.Exec(row...); err != nil {
			return fmt.Errorf("load %s: row %d: %w", table, i, err)
		}
	}
	return tx.Commit()
}

// insertJSONRows inserts each JSON-object row into table, binding only the
// columns that row sets and the table has.
func (s *SQLiteStore) insertJSONRows(tx *notifyTx, table string, list []json.RawMessage, keepExisting bool) error {
	types := map[string]string{}
	for _, c := range s.columns[table] {
		types[c.Name] = c.Type
	}
	stmts := map[string]*sql.Stmt{}
	defer func() {
		for _, st := range stmts {
			st.Close()
		}
	}()
	for _, raw := range list {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		cols := make([]string, 0, len(row))
		for k := range row {
			if _, ok := types[k]; ok {
				cols = append(cols, k)
			}
		}
		if len(cols) == 0 {
			continue
		}
		sort.Strings(cols)
		args := make([]any, len(cols))
		for i, c := range cols {
			v, err := importValue(row[c], types[c])
			if err != nil {
				return fmt.Errorf("column %s: %w", c, err)
			}
			args[i] = v
		}
		key := strings.Join(cols, ",")
		st := stmts[key]
		if st == nil {
			q := `INSERT INTO ` + table + ` ("` + strings.Join(cols, `", "`) + `") VALUES (?` + strings.Repeat(", ?", len(cols)-1) + `)`
			if keepExisting {
				q += ` ON CONFLICT DO NOTHING`
			}
			var err error
			if st, err = tx.Prepare(q); err != nil {
				return err
			}
			stmts[key] = st
		}
		if _, err := st.Exec(args...); err != nil {
			return err
		}
	}
	if len(list) > 0 {
		tx.touch(table)
	}
	return nil
}

// importValue converts one exported JSON value to what its column stores:
// a JSON column keeps the value's own JSON text, anything else its scalar.
func importValue(raw json.RawMessage, colType string) (any, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	if colType == "JSON" {
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return nil, err
		}
		return buf.String(), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, nil
		}
		return t.Float64()
	case string, bool:
		return t, nil
	default:
		// A nested value in a TEXT column (word_timings exported by an
		// older build as JSON): keep its text.
		return string(raw), nil
	}
}

// sqlString quotes s as a SQL string literal.
func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
