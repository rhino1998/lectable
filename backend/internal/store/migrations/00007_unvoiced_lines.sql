-- An ellipsis-only line ("…", "“...”") is a beat of silence, not something
-- a character says (Paragraph.Unvoiced): attribution no longer gives it a
-- speaker or an emotion, and its clip is silence written without a TTS
-- call. This clears the speakers and emotions such lines already had and
-- drops their rendered audio so it is made again as silence.

-- +goose Up
CREATE TEMP TABLE unvoiced_paragraphs AS
SELECT id FROM paragraphs
WHERE trim(content, ' .…"“”„«»''‘’') = ''
	AND (content LIKE '%…%' OR replace(content, ' ', '') LIKE '%..%');

UPDATE paragraphs SET speaker = 'Narrator', speaker_index = 0, emotion = ''
WHERE is_quote AND id IN (SELECT id FROM unvoiced_paragraphs);

DELETE FROM paragraph_edits WHERE paragraph_id IN (SELECT id FROM unvoiced_paragraphs);

DELETE FROM paragraph_audio WHERE paragraph_id IN (SELECT id FROM unvoiced_paragraphs);

DROP TABLE unvoiced_paragraphs;
