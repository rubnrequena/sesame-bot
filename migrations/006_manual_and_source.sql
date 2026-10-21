-- +migrate Up
-- Origen de cada ejecución de fichaje: 'scheduled' (programada por el scheduler),
-- 'manual' (fichaje a demanda desde la web) o 'test' (dry-run de diagnóstico).
ALTER TABLE checkin_logs ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'scheduled';

-- El dry-run de diagnóstico se registra con action='TEST' para no confundirse con
-- un fichaje real; se amplía la restricción para admitirlo.
ALTER TABLE checkin_logs DROP CONSTRAINT IF EXISTS checkin_logs_action_check;
ALTER TABLE checkin_logs ADD CONSTRAINT checkin_logs_action_check CHECK (action IN ('IN','OUT','TEST'));
