-- A world fact about a kind of being can name a voice effect
-- (voicefx.VoiceEffect: 'insectoid', 'underwater') that every line of a
-- character of that kind is given after it is generated - what no voice
-- model produces from a casting. '' for none.

-- +goose Up
ALTER TABLE world_facts ADD COLUMN voice_effect TEXT NOT NULL DEFAULT '';
