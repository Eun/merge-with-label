-- +goose Up
ALTER TABLE mwl_pr_state ADD COLUMN base_sha TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE mwl_pr_state DROP COLUMN base_sha;
