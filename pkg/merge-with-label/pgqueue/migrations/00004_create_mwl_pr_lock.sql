-- +goose Up

-- mwl_pr_state stores a fingerprint of the decision-relevant state of a PR
-- at the time it was last processed. A run that fetches the same state as the
-- last run produces the same hash and is silently skipped; a run that sees a
-- changed state (new approval, check pass, label change, commit, etc.) produces
-- a different hash and proceeds.
--
-- This satisfies three requirements:
--   1. No double-run on concurrent goroutines — first goroutine writes the hash
--      before work; a concurrent goroutine fetching the same GitHub state sees
--      the same hash and skips.
--   2. No re-run when nothing meaningful changed — an unrelated status/push event
--      fans out to all labeled PRs; the worker fetches details, computes the same
--      hash, and skips without doing any work.
--   3. Always re-run when something actionable changed — approval, check pass,
--      label change, commit — each changes a tracked field, producing a new hash.
--
-- UNLOGGED: writes are not WAL-logged so upserts are fast. On crash recovery the
-- table is truncated, which is safe — all entries are treated as cache misses and
-- the next run re-evaluates from fresh GitHub state.
CREATE UNLOGGED TABLE IF NOT EXISTS mwl_pr_state (
    repo_node_id TEXT        NOT NULL,
    pr_number    BIGINT      NOT NULL,
    state_hash   TEXT        NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (repo_node_id, pr_number)
);

-- +goose Down
DROP TABLE IF EXISTS mwl_pr_state;
