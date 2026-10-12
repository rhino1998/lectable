-- What a pass found in the text and is waiting for the reader to rule on,
-- beside world facts (which keep theirs in world_facts.status).
--
-- time_jump_proposals: years seem to pass before chapter_id of book_id -
-- see store.TimeJumpProposal. Accepting one makes the book_time_jumps row
-- (at the chapter and length the reader settled on); a skipped one is kept
-- so the same gap isn't proposed again.
--
-- character_notes: one thing the text states about a character - what kind
-- of being they are, how old they are at some chapter - with the sentence
-- it rests on. See store.CharacterNote. Only an accepted note is shown to
-- casting.

-- +goose Up
CREATE TABLE IF NOT EXISTS time_jump_proposals (
    id          TEXT PRIMARY KEY,
    book_id     TEXT NOT NULL,
    chapter_id  TEXT NOT NULL,
    years       INTEGER NOT NULL,
    evidence    TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'pending',
    created_at  BIGINT NOT NULL,
    UNIQUE (book_id, chapter_id)
);

CREATE TABLE IF NOT EXISTS character_notes (
    id            TEXT PRIMARY KEY,
    character_id  TEXT NOT NULL,
    kind          TEXT NOT NULL,
    value         TEXT NOT NULL,
    chapter_id    TEXT NOT NULL DEFAULT '',
    evidence      TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'pending',
    created_at    BIGINT NOT NULL,
    UNIQUE (character_id, kind, value)
);

CREATE INDEX IF NOT EXISTS idx_character_notes_character ON character_notes(character_id);

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS character_notes_with_character AFTER DELETE ON characters
BEGIN
    DELETE FROM character_notes WHERE character_id = OLD.id;
END;
-- +goose StatementEnd
