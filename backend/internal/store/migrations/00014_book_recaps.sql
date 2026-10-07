-- A book's "previously on" recap: one per book, written for the reading
-- position it was asked at (kind 'position': what happened before
-- chapter_idx/paragraph_idx) or, at a book's start, for the book before it
-- in its series (kind 'series', source_book_id). audio_voice is the
-- narrator voice its clip was spoken in ('' when it has no clip), read_at
-- the book's last_read_at when it was written. error is set, and content
-- empty, when writing it failed.

-- +goose Up
CREATE TABLE IF NOT EXISTS book_recaps (
    book_id        TEXT PRIMARY KEY,
    kind           TEXT NOT NULL,
    chapter_idx    INTEGER NOT NULL DEFAULT 0,
    paragraph_idx  INTEGER NOT NULL DEFAULT 0,
    through_idx    INTEGER NOT NULL DEFAULT 0,
    source_book_id TEXT NOT NULL DEFAULT '',
    title          TEXT NOT NULL DEFAULT '',
    content        TEXT NOT NULL DEFAULT '',
    error          TEXT NOT NULL DEFAULT '',
    audio_voice    TEXT NOT NULL DEFAULT '',
    audio_seconds  DOUBLE NOT NULL DEFAULT 0,
    read_at        BIGINT NOT NULL DEFAULT 0,
    updated_at     BIGINT NOT NULL DEFAULT 0
);
