-- Reading a Patreon creator's patrons-only posts takes the session of an
-- account that is a member (internal/patreon): the session_id cookie of a
-- signed-in browser, and the name of the account it belongs to, for
-- display. Library-wide, on the single default_voice row with the other
-- app-wide settings; '' when not signed in.

-- +goose Up
ALTER TABLE default_voice ADD COLUMN patreon_session TEXT NOT NULL DEFAULT '';
ALTER TABLE default_voice ADD COLUMN patreon_account TEXT NOT NULL DEFAULT '';
