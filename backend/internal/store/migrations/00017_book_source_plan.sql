-- A book imported from a website can be a chosen part of the work: some
-- chapters left out, and some taken from an archive after the site
-- delisted them. source_plan (store.SourcePlan, JSON) remembers both, so a
-- later check for new chapters neither adds the ones left out nor forgets
-- the delisted ones still to fetch - the site's own list no longer names
-- them. '{}' for a book imported whole from what the site lists.

-- +goose Up
ALTER TABLE books ADD COLUMN source_plan JSON NOT NULL DEFAULT '{}';
