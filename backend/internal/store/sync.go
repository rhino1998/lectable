package store

import (
	"slices"
	"strings"
)

// SyncFingerprints fingerprints, in one pass, every stored row a book's
// offline-sync hash tree (see httpapi/manifest.go) is derived from, so the
// tree can be kept and only the chapters whose inputs actually moved
// rebuilt. Returns one fingerprint for everything book-wide (the book's own
// row minus its reading position and narration-rate estimate, which change
// constantly and feed no hash, plus the whole character/voice-preset/
// default-voice tables that voice resolution reads) and one per chapter
// index (the chapter row and its paragraphs, their audio rows under every
// voice, images and breaks).
//
// Deliberately over-covers rather than mirroring exactly what a hash
// reads: whole rows via rowhash over every column the table has (see
// SQLiteStore.rowHash, so a column added later is covered automatically),
// every voice's audio rather than just the resolved one, every series'
// characters. A false "changed" only costs a rebuild; a false "unchanged"
// would leave a client stale indefinitely. Alignment's word_timings is the
// one exclusion - large, hashed nowhere, and written right after every
// generation. Each part is coalesced rather than left NULL so concat_ws
// keeps its position (it skips NULLs).
func (s *SQLiteStore) SyncFingerprints(bookID string) (book string, chapters map[int]string, err error) {
	h := s.rowHash
	err = s.db.QueryRow(`
		SELECT concat_ws('|',
			coalesce((SELECT `+h("books", "b", bookVolatileColumns...)+` FROM books b WHERE id = ?), ''),
			coalesce((SELECT bit_xor(`+h("characters", "t")+`) FROM characters t), ''),
			coalesce((SELECT bit_xor(`+h("character_voices", "t")+`) FROM character_voices t), ''),
			coalesce((SELECT bit_xor(`+h("voice_presets", "t")+`) FROM voice_presets t), ''),
			coalesce((SELECT bit_xor(`+h("default_voice", "t")+`) FROM default_voice t), ''))`, bookID).Scan(&book)
	if err != nil {
		return "", nil, err
	}

	rows, err := s.db.Query(`
		WITH ch AS (SELECT * FROM chapters WHERE book_id = ?)
		SELECT ch.idx, concat_ws('|', `+h("chapters", "ch")+`, coalesce(pp.fp, ''), coalesce(pa.fp, ''),
			coalesce(im.fp, ''), coalesce(br.fp, ''))
		FROM ch
		LEFT JOIN (SELECT chapter_id, bit_xor(`+h("paragraphs", "p")+`) AS fp FROM paragraphs p
			WHERE chapter_id IN (SELECT id FROM ch) GROUP BY chapter_id) pp ON pp.chapter_id = ch.id
		LEFT JOIN (SELECT p.chapter_id, bit_xor(`+h("paragraph_audio", "a", "word_timings")+`) AS fp
			FROM paragraph_audio a
			JOIN paragraphs p ON p.id = a.paragraph_id
			WHERE p.chapter_id IN (SELECT id FROM ch) GROUP BY p.chapter_id) pa ON pa.chapter_id = ch.id
		LEFT JOIN (SELECT chapter_id, bit_xor(`+h("images", "i")+`) AS fp FROM images i
			WHERE chapter_id IN (SELECT id FROM ch) GROUP BY chapter_id) im ON im.chapter_id = ch.id
		LEFT JOIN (SELECT chapter_id, bit_xor(`+h("breaks", "k")+`) AS fp FROM breaks k
			WHERE chapter_id IN (SELECT id FROM ch) GROUP BY chapter_id) br ON br.chapter_id = ch.id`, bookID)
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

// bookVolatileColumns are the books columns no fingerprint covers: reading
// position and the narration-rate estimate change constantly and feed no
// hash.
var bookVolatileColumns = []string{"pos_chapter_idx", "pos_paragraph_idx", "pos_seconds", "estimate_sec_per_char"}

// rowHash is the SQL expression hashing one row of table (referred to as
// alias) over all its columns but exclude - see driver.go's rowhash.
func (s *SQLiteStore) rowHash(table, alias string, exclude ...string) string {
	var cols []string
	for _, c := range s.columns[table] {
		if !slices.Contains(exclude, c.Name) {
			cols = append(cols, alias+`."`+c.Name+`"`)
		}
	}
	return "rowhash(" + strings.Join(cols, ", ") + ")"
}

// SyncTreeNode is one stored chapter node of the offline-sync hash tree -
// see sync_tree_nodes.
type SyncTreeNode struct {
	ChapterIdx int
	BookFP     string
	ChapterFP  string
	Hash       string
}

// SyncTreeNodes returns bookID's stored nodes hashed by code build
// version, by chapter index. Whether each is still valid (its fingerprints
// match the current ones) is the caller's call.
func (s *SQLiteStore) SyncTreeNodes(bookID, version string) (map[int]SyncTreeNode, error) {
	rows, err := s.db.Query(`SELECT chapter_idx, book_fp, chapter_fp, hash FROM sync_tree_nodes WHERE book_id = ? AND version = ?`, bookID, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]SyncTreeNode{}
	for rows.Next() {
		var n SyncTreeNode
		if err := rows.Scan(&n.ChapterIdx, &n.BookFP, &n.ChapterFP, &n.Hash); err != nil {
			return nil, err
		}
		out[n.ChapterIdx] = n
	}
	return out, rows.Err()
}

// PutSyncTreeNodes stores nodes for bookID under version, replacing
// whatever each chapter had. One transaction, so the update-then-insert
// per node can't race another writer into a duplicate row (the store's
// single connection is held for its duration).
func (s *SQLiteStore) PutSyncTreeNodes(bookID, version string, nodes []SyncTreeNode) error {
	if len(nodes) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, n := range nodes {
		res, err := tx.Exec(
			`UPDATE sync_tree_nodes SET version = ?, book_fp = ?, chapter_fp = ?, hash = ? WHERE book_id = ? AND chapter_idx = ?`,
			version, n.BookFP, n.ChapterFP, n.Hash, bookID, n.ChapterIdx,
		)
		if err != nil {
			return err
		}
		if affected, err := res.RowsAffected(); err != nil {
			return err
		} else if affected > 0 {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO sync_tree_nodes (book_id, chapter_idx, version, book_fp, chapter_fp, hash) VALUES (?, ?, ?, ?, ?, ?)`,
			bookID, n.ChapterIdx, version, n.BookFP, n.ChapterFP, n.Hash,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneSyncTree deletes every stored node not hashed by version - left by
// an older build of the code, and never valid again.
func (s *SQLiteStore) PruneSyncTree(version string) error {
	_, err := s.db.Exec(`DELETE FROM sync_tree_nodes WHERE version <> ?`, version)
	return err
}
