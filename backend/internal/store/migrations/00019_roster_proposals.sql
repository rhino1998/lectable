-- Roster review proposals: one suggested fix to a series' (or book's)
-- character roster - fold one name into another, drop a wrong alias, mark a
-- description as a role, mark a name as nobody's - waiting for the reader
-- to accept or skip it (status 'pending'), or decided. A pair of names the
-- review read about and found different, or couldn't settle, is kept as
-- 'ruled_out' so it isn't asked about again. source is who proposed it
-- ('rule', 'llm', or 'agent' for one filed through the API); dedupe_key
-- names what it is about, one row per key and scope. Names are kept beside
-- the character ids so a decided row still reads after its character is
-- gone.

-- +goose Up
CREATE TABLE IF NOT EXISTS roster_proposals (
    id          TEXT PRIMARY KEY,
    scope       TEXT NOT NULL,
    kind        TEXT NOT NULL,
    subject_id  TEXT NOT NULL,
    subject     TEXT NOT NULL,
    target_id   TEXT NOT NULL DEFAULT '',
    target      TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    evidence    JSON NOT NULL DEFAULT '[]',
    source      TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending',
    dedupe_key  TEXT NOT NULL,
    created_at  BIGINT NOT NULL,
    decided_at  BIGINT NOT NULL DEFAULT 0,
    UNIQUE (scope, dedupe_key)
);
