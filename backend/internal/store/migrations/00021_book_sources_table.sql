-- A book can track more than one source: a serial on Royal Road whose
-- advance chapters are posted on Patreon, a site and an archived copy of
-- it. Each is a row of book_sources (BookSource), with its own state from
-- the last check and its own plan (SourcePlan). The single source a book
-- kept in the books.source_* columns moves here as its first.
--
-- prefix is put before the source's chapter ids wherever the book stores
-- them (chapters.source_id, the plan): '' for the source a book was
-- imported from, whose ids stay as they were, and "<site>:" for one added
-- later, so two sites' ids can't be mistaken for each other.

-- +goose Up
CREATE TABLE book_sources (
    id TEXT PRIMARY KEY,
    book_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    source_id TEXT NOT NULL,
    url TEXT NOT NULL DEFAULT '',
    prefix TEXT NOT NULL DEFAULT '',
    checked_at BIGINT NOT NULL DEFAULT 0,
    remote_chapters BIGINT NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    blocked TEXT NOT NULL DEFAULT '',
    plan JSON NOT NULL DEFAULT '{}',
    added_at BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX idx_book_sources_book ON book_sources (book_id, added_at);
CREATE INDEX idx_book_sources_source ON book_sources (kind, source_id);

INSERT INTO book_sources (id, book_id, kind, source_id, url, checked_at, remote_chapters, error, blocked, plan, added_at)
SELECT lower(hex(randomblob(16))), id, source_kind, source_id, source_url, source_checked_at, source_remote_chapters, source_error, source_blocked, source_plan, added_at
FROM books WHERE source_kind <> '';

DROP INDEX idx_books_source;
ALTER TABLE books DROP COLUMN source_kind;
ALTER TABLE books DROP COLUMN source_id;
ALTER TABLE books DROP COLUMN source_url;
ALTER TABLE books DROP COLUMN source_checked_at;
ALTER TABLE books DROP COLUMN source_remote_chapters;
ALTER TABLE books DROP COLUMN source_error;
ALTER TABLE books DROP COLUMN source_blocked;
ALTER TABLE books DROP COLUMN source_plan;
