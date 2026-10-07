-- How often, in hours, the server checks each book imported from a
-- website for new entries at its source by itself (httpapi's
-- CheckSources): 0 never. One library-wide setting, on the single
-- default_voice row with the other app-wide ones.

-- +goose Up
ALTER TABLE default_voice ADD COLUMN source_check_hours INTEGER NOT NULL DEFAULT 24;
