-- How a book's spoken recap is laid out in its clip: a JSON array of the
-- stretches it was spoken in, each with its text, where it starts and ends
-- in the clip, and its words' timings (httpapi's recapSegmentDTO) - what
-- the clients highlight word by word as it plays. '[]' with no clip.

-- +goose Up
ALTER TABLE book_recaps ADD COLUMN segments JSON NOT NULL DEFAULT '[]';
