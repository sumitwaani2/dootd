-- Sessions created with the installer's one-time password may only set up
-- the admin account (docs/architecture.md §8.1).
ALTER TABLE sessions ADD COLUMN setup INTEGER NOT NULL DEFAULT 0;
