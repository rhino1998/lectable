-- A book imported from a website (Royal Road) remembers where it came
-- from, so newly published chapters can be added to it later
-- (BookSource): the kind of source, the work's id there, its page, and
-- how the last check went. A chapter keeps the source's own id for it
-- (chapters.source_id), which is how a check tells which chapters the
-- book already has - the source may retitle, reorder or remove them.
-- All '' / 0 for a book from an uploaded epub.

-- +goose Up
ALTER TABLE books ADD COLUMN source_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN source_id TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN source_url TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN source_checked_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE books ADD COLUMN source_remote_chapters BIGINT NOT NULL DEFAULT 0;
ALTER TABLE books ADD COLUMN source_error TEXT NOT NULL DEFAULT '';
ALTER TABLE chapters ADD COLUMN source_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_books_source ON books (source_kind, source_id);
