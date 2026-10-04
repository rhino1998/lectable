-- Description tagging (which narration paragraphs describe a character) was
-- retired: nothing it fed was better for it - see "Does anything still need
-- the description pass?" in docs/speakerattr.md. Chapters' "description"
-- pass flag goes with the column.

-- +goose Up
ALTER TABLE paragraphs DROP COLUMN describes_characters;
UPDATE chapters SET passes = json_remove(passes, '$.description') WHERE json_extract(passes, '$.description') IS NOT NULL;
