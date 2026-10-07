-- A check of a book's source stops at the first chapter the source lists
-- but won't serve yet (early access, patrons only), so the book never
-- skips ahead of a chapter it can't read. source_blocked is that chapter's
-- title, '' when the last check wasn't stopped by one.

-- +goose Up
ALTER TABLE books ADD COLUMN source_blocked TEXT NOT NULL DEFAULT '';
