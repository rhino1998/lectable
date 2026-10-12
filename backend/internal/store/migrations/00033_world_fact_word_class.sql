-- What sort of word a world fact is filed under, when it is one the
-- roster must never read as a person's name (store.WordClass): 'title'
-- (what 00032's is_title said), 'race' - a kind of being ("Drake") - or
-- 'nationality' - where someone is from or whose they are ("Pallassian").
-- '' for a fact about anything else. word_class_proposed is a class a
-- pass proposes for a fact already in force, waiting for the reader:
-- '' for none, 'none' once the reader turned one down.

-- +goose Up
ALTER TABLE world_facts ADD COLUMN word_class TEXT NOT NULL DEFAULT '';
ALTER TABLE world_facts ADD COLUMN word_class_proposed TEXT NOT NULL DEFAULT '';
UPDATE world_facts SET word_class = 'title' WHERE is_title;
ALTER TABLE world_facts DROP COLUMN is_title;
