-- How long a reader must have been away from a book before the clients
-- offer its "previously on" recap unasked, in minutes: 0 offers it every
-- time the book is opened, -1 never. One library-wide setting, kept on the
-- single default_voice row with the other app-wide ones.

-- +goose Up
ALTER TABLE default_voice ADD COLUMN recap_after_minutes INTEGER NOT NULL DEFAULT 1440;
