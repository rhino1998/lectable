-- A world fact can mark its word as a title of the story's world
-- ("Prognugator", "Ser"): something several people hold and are called
-- by, never one person's name. The roster rules read these beside their
-- own fixed list of titles (speakerattr.Titles). Such a fact may say
-- nothing else, so its fact text can be empty.

-- +goose Up
ALTER TABLE world_facts ADD COLUMN is_title BOOLEAN NOT NULL DEFAULT FALSE;
