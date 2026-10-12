-- Voice checks: what an audio-language model heard in one voice's
-- reference clip (httpapi.CheckVoices, internal/voicecheck) beside what its
-- casting asks for - one row per roster and voice preset, a character's
-- own or the derived one an age stage speaks in (stage_id). ref_identity
-- names the render of the clip that was heard (voicerefs.RefIdentity), so
-- a row stops applying once the clip is rendered again. mismatches lists
-- the traits that clearly disagree; dismissed is the reader saying the
-- voice is fine as it is.

-- +goose Up
CREATE TABLE IF NOT EXISTS voice_checks (
    scope         TEXT NOT NULL,
    preset_id     TEXT NOT NULL,
    character_id  TEXT NOT NULL,
    stage_id      TEXT NOT NULL DEFAULT '',
    ref_identity  TEXT NOT NULL,
    cast_traits   JSON NOT NULL DEFAULT '{}',
    heard_traits  JSON NOT NULL DEFAULT '{}',
    mismatches    JSON NOT NULL DEFAULT '[]',
    dismissed     BOOLEAN NOT NULL DEFAULT false,
    checked_at    BIGINT NOT NULL,
    PRIMARY KEY (scope, preset_id)
);
