-- Primary keys for the three tables that were kept unique by their writers
-- alone (delete-then-insert, update-then-insert) and so had no index for
-- their by-key reads either. Each keeps its columns, in the same order.

-- +goose Up

-- A derived cache (see 00001): rebuilt by the next manifest request.
DROP TABLE sync_tree_nodes;
CREATE TABLE sync_tree_nodes (
	book_id TEXT NOT NULL,
	chapter_idx INTEGER NOT NULL,
	version TEXT NOT NULL,
	book_fp TEXT NOT NULL,
	chapter_fp TEXT NOT NULL,
	hash TEXT NOT NULL,
	PRIMARY KEY (book_id, chapter_idx)
);

-- Should a key have more than one row, the newest written is the one kept.
CREATE TABLE book_settings_keyed (
	book_id TEXT NOT NULL PRIMARY KEY,
	setting JSON NOT NULL,
	source TEXT NOT NULL,
	updated_at BIGINT NOT NULL
);
INSERT INTO book_settings_keyed (book_id, setting, source, updated_at)
	SELECT book_id, setting, source, updated_at FROM book_settings
	WHERE rowid IN (SELECT MAX(rowid) FROM book_settings GROUP BY book_id);
DROP TABLE book_settings;
ALTER TABLE book_settings_keyed RENAME TO book_settings;

CREATE TABLE chapter_summaries_keyed (
	chapter_id TEXT NOT NULL PRIMARY KEY,
	summary JSON NOT NULL,
	updated_at BIGINT NOT NULL
);
INSERT INTO chapter_summaries_keyed (chapter_id, summary, updated_at)
	SELECT chapter_id, summary, updated_at FROM chapter_summaries
	WHERE rowid IN (SELECT MAX(rowid) FROM chapter_summaries GROUP BY chapter_id);
DROP TABLE chapter_summaries;
ALTER TABLE chapter_summaries_keyed RENAME TO chapter_summaries;
