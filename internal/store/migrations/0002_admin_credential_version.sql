ALTER TABLE admin_credentials ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0);
