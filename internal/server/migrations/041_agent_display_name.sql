-- A console/portal label must survive polls reporting the machine hostname.
-- Never change repo_dir when the displayed name changes.
ALTER TABLE agents ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
