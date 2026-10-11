-- chapter_text.content_rev is rev for a chapter's text alone: a fresh
-- random value whenever a paragraph is added, removed or reworded, but not
-- when one is given another speaker. What the semantic index
-- (internal/semantic) compares to find chapters to embed again - with rev,
-- every attribution run and roster merge made a whole book look unembedded.

-- +goose Up
ALTER TABLE chapter_text ADD COLUMN content_rev INTEGER NOT NULL DEFAULT 0;
UPDATE chapter_text SET content_rev = rev;

-- These fire beside 00020's triggers, in either order: whichever runs
-- first creates a new chapter's row, and neither touches what the other
-- maintains.
-- +goose StatementBegin
CREATE TRIGGER paragraphs_content_insert AFTER INSERT ON paragraphs BEGIN
	INSERT INTO chapter_text (chapter_id, content_rev) VALUES (new.chapter_id, random())
	ON CONFLICT(chapter_id) DO UPDATE SET content_rev = excluded.content_rev;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER paragraphs_content_delete AFTER DELETE ON paragraphs BEGIN
	UPDATE chapter_text SET content_rev = random() WHERE chapter_id = old.chapter_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER paragraphs_content_update AFTER UPDATE OF chapter_id, content ON paragraphs BEGIN
	UPDATE chapter_text SET content_rev = random() WHERE chapter_id = old.chapter_id;
	INSERT INTO chapter_text (chapter_id, content_rev) VALUES (new.chapter_id, random())
	ON CONFLICT(chapter_id) DO UPDATE SET content_rev = excluded.content_rev;
END;
-- +goose StatementEnd
