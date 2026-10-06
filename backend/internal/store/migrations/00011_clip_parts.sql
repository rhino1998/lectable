-- A scare-quote merge group's one clip plays for several paragraphs, each
-- later one a pointer row saying where its words start in it. Those rows
-- go away with the anchor's, so until now an orphaned group clip (see
-- clip_orphans) couldn't be taken up again. The anchor's row now also
-- carries its members' parts (clip_parts: a JSON array of store.ClipPart,
-- one per member after the anchor; '' for a clip of one paragraph), and
-- the orphan record keeps them.

-- +goose Up
ALTER TABLE paragraph_audio ADD COLUMN clip_parts TEXT NOT NULL DEFAULT '';
ALTER TABLE clip_orphans ADD COLUMN parts TEXT NOT NULL DEFAULT '';

DROP TRIGGER paragraph_audio_clip_orphan_delete;
DROP TRIGGER paragraph_audio_clip_orphan_update;

-- +goose StatementBegin
CREATE TRIGGER paragraph_audio_clip_orphan_delete AFTER DELETE ON paragraph_audio
WHEN old.clip_key <> '' AND old.status = 'ready' AND old.pointer_offset = 0
	AND NOT EXISTS (SELECT 1 FROM paragraph_audio WHERE clip_key = old.clip_key AND clip_key <> '')
BEGIN
	INSERT OR REPLACE INTO clip_orphans (key, duration_seconds, word_timings, orphaned_at, parts)
	VALUES (old.clip_key, old.duration_seconds, old.word_timings, CAST(strftime('%s', 'now') AS INTEGER), old.clip_parts);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER paragraph_audio_clip_orphan_update AFTER UPDATE OF clip_key ON paragraph_audio
WHEN old.clip_key <> '' AND new.clip_key <> old.clip_key AND old.status = 'ready' AND old.pointer_offset = 0
	AND NOT EXISTS (SELECT 1 FROM paragraph_audio WHERE clip_key = old.clip_key AND clip_key <> '')
BEGIN
	INSERT OR REPLACE INTO clip_orphans (key, duration_seconds, word_timings, orphaned_at, parts)
	VALUES (old.clip_key, old.duration_seconds, old.word_timings, CAST(strftime('%s', 'now') AS INTEGER), old.clip_parts);
END;
-- +goose StatementEnd
