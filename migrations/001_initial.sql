-- +migrate Up

CREATE TABLE IF NOT EXISTS users (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT        NOT NULL UNIQUE,
    password_hash TEXT        NOT NULL,
    is_admin      BOOLEAN     NOT NULL DEFAULT FALSE,
    is_active     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_configs (
    id                   UUID             PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              UUID             NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    sesame_email         TEXT             NOT NULL DEFAULT '',
    sesame_password_enc  TEXT             NOT NULL DEFAULT '',
    headless             BOOLEAN          NOT NULL DEFAULT TRUE,
    weekend              BOOLEAN          NOT NULL DEFAULT FALSE,
    hours_in             TEXT             NOT NULL DEFAULT '09:00',
    hours_out            TEXT             NOT NULL DEFAULT '18:00',
    location_office_lat  DOUBLE PRECISION NOT NULL DEFAULT 0,
    location_office_lon  DOUBLE PRECISION NOT NULL DEFAULT 0,
    location_home_lat    DOUBLE PRECISION NOT NULL DEFAULT 0,
    location_home_lon    DOUBLE PRECISION NOT NULL DEFAULT 0,
    office_days          TEXT             NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    UNIQUE(user_id)
);

CREATE TABLE IF NOT EXISTS day_overrides (
    id        UUID     PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id   UUID     NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    weekday   SMALLINT NOT NULL,
    hours_in  TEXT     NOT NULL DEFAULT '',
    hours_out TEXT     NOT NULL DEFAULT '',
    UNIQUE(user_id, weekday)
);

CREATE TABLE IF NOT EXISTS checkin_logs (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action       TEXT        NOT NULL CHECK (action IN ('IN','OUT')),
    status       TEXT        NOT NULL CHECK (status IN ('ok','error','skipped')),
    message      TEXT        NOT NULL DEFAULT '',
    scheduled_at TIMESTAMPTZ NOT NULL,
    executed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS checkin_logs_user_time ON checkin_logs(user_id, executed_at DESC);

CREATE TABLE IF NOT EXISTS sessions (
    token      TEXT        PRIMARY KEY,
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS sessions_expires ON sessions(expires_at);
