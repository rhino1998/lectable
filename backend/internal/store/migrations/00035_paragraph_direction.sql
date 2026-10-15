-- A dialogue line's free-form speech direction (internal/direction): a
-- style, point tags and scoped tags, anchored by rune offsets into the
-- line's text and the hash of the text they were written for. Only read
-- under a clone model that takes direction as text (Fish Drama 3); '{}'
-- for none. Beside paragraphs.emotion, which stays the label every other
-- clone model renders from.

-- +goose Up
ALTER TABLE paragraphs ADD COLUMN direction JSON NOT NULL DEFAULT '{}';
