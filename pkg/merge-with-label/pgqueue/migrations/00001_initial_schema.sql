-- +goose Up

-- pg_cron must be in shared_preload_libraries before this extension can be
-- created (configured in docker/postgres/Dockerfile).
CREATE EXTENSION IF NOT EXISTS pg_cron;

CREATE TABLE IF NOT EXISTS mwl_repo_queue (
    id           BIGSERIAL    PRIMARY KEY,
    dedup_key    TEXT         NOT NULL UNIQUE,
    payload      JSONB        NOT NULL,
    attempts     INT          NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS mwl_pr_queue (
    id           BIGSERIAL    PRIMARY KEY,
    dedup_key    TEXT         NOT NULL UNIQUE,
    payload      JSONB        NOT NULL,
    attempts     INT          NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- UNLOGGED: writes are not WAL-logged so inserts/updates are significantly
-- faster. On crash recovery the table is truncated, which is acceptable
-- because mwl_kv is a cache (all entries are treated as misses on restart).
CREATE UNLOGGED TABLE IF NOT EXISTS mwl_kv (
    bucket     TEXT        NOT NULL,
    key        TEXT        NOT NULL,
    value      BYTEA       NOT NULL,
    expires_at TIMESTAMPTZ,
    PRIMARY KEY (bucket, key)
);

CREATE TABLE IF NOT EXISTS mwl_pr_state (
    repo_node_id TEXT        NOT NULL,
    pr_number    BIGINT      NOT NULL,
    head_sha     TEXT        NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (repo_node_id, pr_number)
);

-- Schedule a nightly cleanup of expired mwl_kv rows at 03:00 UTC.
-- cron.schedule_in_database targets our own database so no superuser
-- access to the postgres maintenance database is required.
SELECT cron.schedule_in_database(
    'mwl_kv_cleanup',
    '0 3 * * *',
    'DELETE FROM mwl_kv WHERE expires_at IS NOT NULL AND expires_at <= NOW()',
    current_database()
);

-- +goose Down

SELECT cron.unschedule('mwl_kv_cleanup');

DROP TABLE IF EXISTS mwl_pr_state;
DROP TABLE IF EXISTS mwl_kv;
DROP TABLE IF EXISTS mwl_pr_queue;
DROP TABLE IF EXISTS mwl_repo_queue;

DROP EXTENSION IF EXISTS pg_cron;
