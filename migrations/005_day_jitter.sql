-- Add jitter_minutes to day_overrides: random variation (± minutes) around each
-- scheduled check-in time for that specific day. 0 = exact time.
ALTER TABLE day_overrides
  ADD COLUMN IF NOT EXISTS jitter_minutes SMALLINT NOT NULL DEFAULT 0
    CHECK (jitter_minutes BETWEEN 0 AND 60);
