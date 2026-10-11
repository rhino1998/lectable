-- A character's voice at a later age: from chapter_id on (in book_id, and
-- in every later book of the series), their lines are voiced from instruct
-- - their casting rewritten for about age years old - instead of the
-- character's own summary. A character with no rows has one voice
-- throughout, as before. Set by the reader on the Speakers tab, or made
-- for every character a time jump ages (jump_id, '' for one set by hand);
-- see store.CharacterStage and narration.StageVoice.
--
-- book_time_jumps: years pass before chapter_id of book_id, for everyone -
-- see store.TimeJump. Removing a jump removes the stages it made.

-- +goose Up
CREATE TABLE IF NOT EXISTS character_stages (
    id            TEXT PRIMARY KEY,
    character_id  TEXT NOT NULL,
    book_id       TEXT NOT NULL,
    chapter_id    TEXT NOT NULL,
    age           INTEGER NOT NULL DEFAULT 0,
    instruct      TEXT NOT NULL DEFAULT '',
    jump_id       TEXT NOT NULL DEFAULT '',
    created_at    BIGINT NOT NULL,
    UNIQUE (character_id, chapter_id)
);

CREATE INDEX IF NOT EXISTS idx_character_stages_character ON character_stages(character_id);

CREATE TABLE IF NOT EXISTS book_time_jumps (
    id          TEXT PRIMARY KEY,
    book_id     TEXT NOT NULL,
    chapter_id  TEXT NOT NULL,
    years       INTEGER NOT NULL,
    created_at  BIGINT NOT NULL,
    UNIQUE (book_id, chapter_id)
);
