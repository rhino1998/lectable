-- Keyed facts about a story's world (store.WorldFact): "kobold" -> "a
-- short-lived kind; thirteen is old", "dwarf" -> "Scottish accent". Scoped
-- like characters (SeriesScope), or 'global' for rules that hold in every
-- book unless a series has a fact of its own for the same key. A fact
-- reaches a prompt when its key or one of its aliases appears in what
-- that prompt already reads. key_norm is lower(key): one fact per key per
-- scope. source is who wrote it ('manual', 'agent', 'llm') and status
-- whether it is in force ('active') or still waiting for the reader
-- ('pending', 'skipped') - nothing but an active fact is ever shown to a
-- model.

-- +goose Up
CREATE TABLE IF NOT EXISTS world_facts (
    id          TEXT PRIMARY KEY,
    scope       TEXT NOT NULL,
    key_norm    TEXT NOT NULL,
    key         TEXT NOT NULL,
    aliases     JSON NOT NULL DEFAULT '[]',
    fact        TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT 'manual',
    status      TEXT NOT NULL DEFAULT 'active',
    created_at  BIGINT NOT NULL,
    updated_at  BIGINT NOT NULL,
    UNIQUE (scope, key_norm)
);
