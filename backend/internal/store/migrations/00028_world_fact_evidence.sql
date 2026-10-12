-- Where a proposed world fact comes from: the sentence of the book it
-- rests on, word for word, and the chapter that sentence is in ('' for a
-- fact the reader wrote). Shown beside a pending fact so the reader can
-- judge it; the chapter is also the earliest point in the series the fact
-- is known.

-- +goose Up
ALTER TABLE world_facts ADD COLUMN evidence TEXT NOT NULL DEFAULT '';
ALTER TABLE world_facts ADD COLUMN evidence_chapter_id TEXT NOT NULL DEFAULT '';
