-- chapters.matter marks a chapter that is front or back matter (a title
-- page, the copyright page, "About the Author"), not story: background
-- narration, the LLM passes, the recap and reading progress leave it out,
-- and the clients step over it. matter_by is who said so: '' nobody yet,
-- 'pass' the book's matter pass (speakerattr.Client.FindMatter), 'reader'
-- the reader, whose word the pass never replaces. A book whose chapters
-- are all judged has had its pass (Store.BookNeedsMatterPass).

-- +goose Up
ALTER TABLE chapters ADD COLUMN matter BOOLEAN NOT NULL DEFAULT 0;
ALTER TABLE chapters ADD COLUMN matter_by TEXT NOT NULL DEFAULT '';
