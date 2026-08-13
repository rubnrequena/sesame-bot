-- Add whatsapp_number to user_configs for post-checkin notifications
ALTER TABLE user_configs
  ADD COLUMN IF NOT EXISTS whatsapp_number TEXT NOT NULL DEFAULT '';
