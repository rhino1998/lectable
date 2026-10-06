-- A shared narration clip whose last paragraph_audio row goes away (a
-- voice change, a chapter reimport, a re-attribution) is kept for a while,
-- so the same reading wanted again - the voice switched back, the chapter
-- reimported unchanged - is found rather than rendered. What a row knew
-- about the clip (its duration and word timings) is saved here as that
-- last row leaves; jobs.sharedClip reads it when no row is on the clip,
-- and audiomaint deletes the file once this is older than
-- clipkey.OrphanRetention. A row here is only meaningful while no
-- paragraph_audio row names its key. Merge groups aren't recorded (their
-- members' pointers go with their rows), so those are rendered again.

-- +goose Up
CREATE TABLE clip_orphans (
	key TEXT PRIMARY KEY,
	duration_seconds DOUBLE NOT NULL,
	word_timings TEXT NOT NULL,
	orphaned_at BIGINT NOT NULL
) WITHOUT ROWID;

-- +goose StatementBegin
CREATE TRIGGER paragraph_audio_clip_orphan_delete AFTER DELETE ON paragraph_audio
WHEN old.clip_key <> '' AND old.status = 'ready' AND old.pointer_offset = 0
	AND NOT EXISTS (SELECT 1 FROM paragraph_audio WHERE clip_key = old.clip_key AND clip_key <> '')
BEGIN
	INSERT OR REPLACE INTO clip_orphans (key, duration_seconds, word_timings, orphaned_at)
	VALUES (old.clip_key, old.duration_seconds, old.word_timings, CAST(strftime('%s', 'now') AS INTEGER));
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER paragraph_audio_clip_orphan_update AFTER UPDATE OF clip_key ON paragraph_audio
WHEN old.clip_key <> '' AND new.clip_key <> old.clip_key AND old.status = 'ready' AND old.pointer_offset = 0
	AND NOT EXISTS (SELECT 1 FROM paragraph_audio WHERE clip_key = old.clip_key AND clip_key <> '')
BEGIN
	INSERT OR REPLACE INTO clip_orphans (key, duration_seconds, word_timings, orphaned_at)
	VALUES (old.clip_key, old.duration_seconds, old.word_timings, CAST(strftime('%s', 'now') AS INTEGER));
END;
-- +goose StatementEnd
