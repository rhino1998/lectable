-- Which take of its voice a character stage plays: each "regenerate this
-- voice" on a stage moves to the next, a fresh render of the same casting
-- under another seed (see narration.StageVoice). 0 is the first.

-- +goose Up
ALTER TABLE character_stages ADD COLUMN take INTEGER NOT NULL DEFAULT 0;
