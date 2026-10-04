-- The character dossier (migration 00004) was a two-call form of voice
-- casting that was tried and not adopted; the column was never written.

-- +goose Up
ALTER TABLE characters DROP COLUMN dossier;
