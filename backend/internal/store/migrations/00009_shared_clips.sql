-- A narration clip stored under its content key (internal/clipkey,
-- audiopath.SharedClipFile) is shared by every paragraph reading the same
-- line the same way. clip_key names the shared clip a row's audio is -
-- the file is found by it - or is '' for a clip at the paragraph's own
-- path (a voice with no preset, anything from before the shared store
-- that hasn't been adopted) and on a scare-quote merge group's pointer
-- rows, whose clip is their anchor's. It is also how the next paragraph to
-- read the line finds the clip's duration and word timings, and how
-- re-rendering a clip reaches everyone using it.

-- +goose Up
ALTER TABLE paragraph_audio ADD COLUMN clip_key TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_paragraph_audio_clip ON paragraph_audio(clip_key) WHERE clip_key <> '';
