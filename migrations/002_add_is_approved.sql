-- +migrate Up
ALTER TABLE users ADD COLUMN IF NOT EXISTS is_approved BOOLEAN NOT NULL DEFAULT FALSE;
-- Approve all existing users so they keep working
UPDATE users SET is_approved = TRUE;
