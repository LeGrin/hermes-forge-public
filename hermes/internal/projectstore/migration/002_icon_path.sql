-- 002_icon_path: adds icon_path column to projects table
-- Icon path is the URL-style path served by Hermes (e.g. /icons/hermes.png)
-- Source of truth for actual icon files is deploy/icons/ on disk
ALTER TABLE projects ADD COLUMN icon_path TEXT;