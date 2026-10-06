-- A paragraph read from a source <table> records where it sits in it
-- (Paragraph.Table): which table and row, and each cell's span of
-- content. '{}' outside a table. Set at import, so a book imported before
-- this keeps one paragraph per cell until its chapters are reimported.

-- +goose Up
ALTER TABLE paragraphs ADD COLUMN table_pos JSON NOT NULL DEFAULT '{}';
