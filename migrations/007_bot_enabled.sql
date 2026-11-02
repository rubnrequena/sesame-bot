-- +migrate Up
-- Interruptor self-service para pausar el fichaje automático sin desactivar la
-- cuenta web. El scheduler solo carga usuarios con bot_enabled = TRUE.
ALTER TABLE user_configs ADD COLUMN IF NOT EXISTS bot_enabled BOOLEAN NOT NULL DEFAULT TRUE;
