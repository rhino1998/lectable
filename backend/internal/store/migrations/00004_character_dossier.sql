-- A character's dossier (store.CharacterDossier): what the book establishes
-- about them, distilled by one LLM call from material gathered across their
-- series, and what voice casting is written from. '{}' until
-- characterization has run with it.

-- +goose Up
ALTER TABLE characters ADD COLUMN dossier JSON NOT NULL DEFAULT '{}';
