-- +goose Up
DROP TABLE IF EXISTS mwl_pr_state;

-- +goose Down
CREATE TABLE IF NOT EXISTS mwl_pr_state (
    repo_node_id TEXT        NOT NULL,
    pr_number    BIGINT      NOT NULL,
    head_sha     TEXT        NOT NULL,
    base_sha     TEXT        NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (repo_node_id, pr_number)
);
