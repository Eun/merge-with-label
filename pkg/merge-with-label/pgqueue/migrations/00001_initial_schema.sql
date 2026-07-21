-- +goose Up
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

CREATE TABLE IF NOT EXISTS mwl_kv (
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

-- +goose Down
DROP TABLE IF EXISTS mwl_pr_state;
DROP TABLE IF EXISTS mwl_kv;
DROP TABLE IF EXISTS mwl_pr_queue;
DROP TABLE IF EXISTS mwl_repo_queue;
