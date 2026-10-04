-- A play names who speaks before each speech (internal/epub's play pass):
-- source_speaker is that name, kept so re-running attribution leaves the
-- line with it, and label is the speaker label as the book sets it, shown
-- before the paragraph but never spoken. Both '' outside a play.

-- +goose Up
ALTER TABLE paragraphs ADD COLUMN source_speaker TEXT NOT NULL DEFAULT '';
ALTER TABLE paragraphs ADD COLUMN label TEXT NOT NULL DEFAULT '';
