-- chapter_text holds each chapter's paragraph count, text length and word
-- count, kept current by triggers on paragraphs, so a book listing reads
-- one row per chapter instead of every paragraph's content (2.5M
-- paragraphs: 1.5s for the word count alone). rev is a fresh random value
-- on every change to what ChapterNarration reads of a chapter's paragraphs
-- - their text, or who speaks them - which is how Cached.ChapterNarration
-- finds the chapters to re-read (see ChapterFingerprints) without a write
-- to one book dropping every other book's cached narration.

-- +goose Up
CREATE TABLE chapter_text (
	chapter_id TEXT PRIMARY KEY,
	paragraphs INTEGER NOT NULL DEFAULT 0,
	chars INTEGER NOT NULL DEFAULT 0,
	words INTEGER NOT NULL DEFAULT 0,
	rev INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

INSERT INTO chapter_text (chapter_id, paragraphs, chars, words, rev)
SELECT chapter_id, COUNT(*), SUM(LENGTH(content)),
	SUM(CASE WHEN content = '' THEN 0 ELSE LENGTH(content) - LENGTH(REPLACE(content, ' ', '')) + 1 END),
	random()
FROM paragraphs
GROUP BY chapter_id;

-- +goose StatementBegin
CREATE TRIGGER paragraphs_text_insert AFTER INSERT ON paragraphs BEGIN
	INSERT INTO chapter_text (chapter_id, paragraphs, chars, words, rev)
	VALUES (new.chapter_id, 1, LENGTH(new.content),
		CASE WHEN new.content = '' THEN 0 ELSE LENGTH(new.content) - LENGTH(REPLACE(new.content, ' ', '')) + 1 END,
		random())
	ON CONFLICT(chapter_id) DO UPDATE SET
		paragraphs = paragraphs + 1,
		chars = chars + excluded.chars,
		words = words + excluded.words,
		rev = excluded.rev;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER paragraphs_text_delete AFTER DELETE ON paragraphs BEGIN
	UPDATE chapter_text SET
		paragraphs = paragraphs - 1,
		chars = chars - LENGTH(old.content),
		words = words - CASE WHEN old.content = '' THEN 0 ELSE LENGTH(old.content) - LENGTH(REPLACE(old.content, ' ', '')) + 1 END,
		rev = random()
	WHERE chapter_id = old.chapter_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER paragraphs_text_update AFTER UPDATE OF chapter_id, content, speaker, speaker_index ON paragraphs BEGIN
	UPDATE chapter_text SET
		paragraphs = paragraphs - 1,
		chars = chars - LENGTH(old.content),
		words = words - CASE WHEN old.content = '' THEN 0 ELSE LENGTH(old.content) - LENGTH(REPLACE(old.content, ' ', '')) + 1 END,
		rev = random()
	WHERE chapter_id = old.chapter_id;
	INSERT INTO chapter_text (chapter_id, paragraphs, chars, words, rev)
	VALUES (new.chapter_id, 1, LENGTH(new.content),
		CASE WHEN new.content = '' THEN 0 ELSE LENGTH(new.content) - LENGTH(REPLACE(new.content, ' ', '')) + 1 END,
		random())
	ON CONFLICT(chapter_id) DO UPDATE SET
		paragraphs = paragraphs + 1,
		chars = chars + excluded.chars,
		words = words + excluded.words,
		rev = excluded.rev;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER chapters_text_delete AFTER DELETE ON chapters BEGIN
	DELETE FROM chapter_text WHERE chapter_id = old.id;
END;
-- +goose StatementEnd
