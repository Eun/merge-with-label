// Package pgqueue implements a Postgres-backed job queue with SKIP LOCKED
// dequeuing and a PR-state deduplication cache.
//
// There are two dedicated queue tables — one per work type — so each can be
// indexed, partitioned, and monitored independently:
//
//   - mwl_repo_queue     repo-level fan-out jobs (push, status, …)
//   - mwl_pr_queue       per-PR work items
package pgqueue

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pkg/errors"
)

// Store is the single entry-point for the Postgres queue and cache tables.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres using connString and runs the schema migration.
func New(ctx context.Context, connString string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, errors.Wrap(err, "unable to parse postgres connection string")
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create postgres pool")
	}

	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, errors.Wrap(err, "unable to migrate schema")
	}
	return s, nil
}

// Close releases all pool connections.
func (s *Store) Close() {
	s.pool.Close()
}

// schema creates all tables if they do not exist yet.
// Two dedicated queue tables are used instead of a single table with a
// msg_type discriminator so each queue can be optimised independently.
const schema = `
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
`

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, schema)
	return errors.Wrap(err, "migrate")
}

// Job is a single dequeued row.
type Job struct {
	ID      int64
	Payload []byte
}

// -----------------------------------------------------------------------
// Repo queue
// -----------------------------------------------------------------------

// EnqueueRepo inserts a repo-level job.
// A second call with the same dedup_key is silently ignored (ON CONFLICT DO NOTHING).
func (s *Store) EnqueueRepo(ctx context.Context, dedupKey string, payload []byte, availableAt time.Time) error {
	if availableAt.IsZero() {
		availableAt = time.Now()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mwl_repo_queue (dedup_key, payload, available_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (dedup_key) DO NOTHING
	`, dedupKey, payload, availableAt)
	return errors.Wrap(err, "enqueue repo")
}

// DequeueRepo claims the oldest available repo job using SKIP LOCKED.
// Returns nil, nil when the queue is empty.
func (s *Store) DequeueRepo(ctx context.Context) (*Job, error) {
	return dequeue(ctx, s.pool, "mwl_repo_queue")
}

// RescheduleRepo re-queues a failed repo job, or permanently removes it when
// maxAttempts is reached. Returns (true, nil) when the job was dropped.
func (s *Store) RescheduleRepo(ctx context.Context, id int64, dedupKey string, payload []byte, delay time.Duration, maxAttempts int) (bool, error) {
	return reschedule(ctx, s.pool, "mwl_repo_queue", id, dedupKey, payload, delay, maxAttempts)
}

// -----------------------------------------------------------------------
// PR queue
// -----------------------------------------------------------------------

// EnqueuePR inserts a per-PR job.
// A second call with the same dedup_key is silently ignored (ON CONFLICT DO NOTHING).
func (s *Store) EnqueuePR(ctx context.Context, dedupKey string, payload []byte, availableAt time.Time) error {
	if availableAt.IsZero() {
		availableAt = time.Now()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mwl_pr_queue (dedup_key, payload, available_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (dedup_key) DO NOTHING
	`, dedupKey, payload, availableAt)
	return errors.Wrap(err, "enqueue pr")
}

// DequeuePR claims the oldest available PR job using SKIP LOCKED.
// Returns nil, nil when the queue is empty.
func (s *Store) DequeuePR(ctx context.Context) (*Job, error) {
	return dequeue(ctx, s.pool, "mwl_pr_queue")
}

// ReschedulePR re-queues a failed PR job, or permanently removes it when
// maxAttempts is reached. Returns (true, nil) when the job was dropped.
func (s *Store) ReschedulePR(ctx context.Context, id int64, dedupKey string, payload []byte, delay time.Duration, maxAttempts int) (bool, error) {
	return reschedule(ctx, s.pool, "mwl_pr_queue", id, dedupKey, payload, delay, maxAttempts)
}

// -----------------------------------------------------------------------
// Shared helpers (table name is a safe compile-time constant in each caller)
// -----------------------------------------------------------------------

func dequeue(ctx context.Context, pool *pgxpool.Pool, table string) (*Job, error) {
	var j Job
	// The table name is a hard-coded string constant coming from this package —
	// never from user input — so interpolation is safe here.
	err := pool.QueryRow(ctx, `
		DELETE FROM `+table+`
		WHERE id = (
			SELECT id FROM `+table+`
			WHERE  available_at <= NOW()
			ORDER BY available_at, id
			LIMIT  1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, payload
	`).Scan(&j.ID, &j.Payload)
	if err != nil {
		if isNoRows(err) {
			return nil, nil //nolint:nilnil // empty queue is not an error
		}
		return nil, errors.Wrapf(err, "dequeue from %s", table)
	}
	return &j, nil
}

func reschedule(ctx context.Context, pool *pgxpool.Pool, table string, id int64, dedupKey string, payload []byte, delay time.Duration, maxAttempts int) (dropped bool, err error) {
	var attempts int
	qErr := pool.QueryRow(ctx,
		`SELECT attempts FROM `+table+` WHERE dedup_key = $1`,
		dedupKey,
	).Scan(&attempts)
	if isNoRows(qErr) {
		return true, nil //nolint:nilnil // row already gone, treat as dropped
	}
	if qErr != nil {
		return false, errors.Wrapf(qErr, "reschedule %s: read attempts", table)
	}

	newAttempts := attempts + 1
	if maxAttempts > 0 && newAttempts >= maxAttempts {
		_, err = pool.Exec(ctx,
			`DELETE FROM `+table+` WHERE dedup_key = $1`,
			dedupKey,
		)
		return true, errors.Wrapf(err, "reschedule %s: delete exhausted job", table)
	}

	availableAt := time.Now().Add(delay)
	_, err = pool.Exec(ctx, `
		INSERT INTO `+table+` (id, dedup_key, payload, available_at, attempts)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (dedup_key) DO UPDATE
		    SET available_at = EXCLUDED.available_at,
		        attempts     = EXCLUDED.attempts
	`, id, dedupKey, payload, availableAt, newAttempts)
	return false, errors.Wrapf(err, "reschedule %s", table)
}

// -----------------------------------------------------------------------
// Key-value cache
// -----------------------------------------------------------------------

// KVGet retrieves a value. Returns nil, nil on cache miss or expiry.
func (s *Store) KVGet(ctx context.Context, bucket, key string) ([]byte, error) {
	var value []byte
	err := s.pool.QueryRow(ctx, `
		SELECT value FROM mwl_kv
		WHERE bucket = $1 AND key = $2
		  AND (expires_at IS NULL OR expires_at > NOW())
	`, bucket, key).Scan(&value)
	if err != nil {
		if isNoRows(err) {
			return nil, nil //nolint:nilnil // cache miss is not an error
		}
		return nil, errors.Wrap(err, "kv get")
	}
	return value, nil
}

// KVSet stores a value. Pass zero ttl for no expiry.
func (s *Store) KVSet(ctx context.Context, bucket, key string, value []byte, ttl time.Duration) error {
	var expiresAt *time.Time
	if ttl > 0 {
		t := time.Now().Add(ttl)
		expiresAt = &t
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mwl_kv (bucket, key, value, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (bucket, key) DO UPDATE
		    SET value      = EXCLUDED.value,
		        expires_at = EXCLUDED.expires_at
	`, bucket, key, value, expiresAt)
	return errors.Wrap(err, "kv set")
}

// -----------------------------------------------------------------------
// PR state cache
// -----------------------------------------------------------------------

// PRStateResult is returned by GetPRState.
type PRStateResult struct {
	HeadSHA   string
	UpdatedAt time.Time
}

// GetPRState returns the last-seen head SHA for a PR, or nil if not cached.
func (s *Store) GetPRState(ctx context.Context, repoNodeID string, prNumber int64) (*PRStateResult, error) {
	var r PRStateResult
	err := s.pool.QueryRow(ctx, `
		SELECT head_sha, updated_at FROM mwl_pr_state
		WHERE repo_node_id = $1 AND pr_number = $2
	`, repoNodeID, prNumber).Scan(&r.HeadSHA, &r.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return nil, nil //nolint:nilnil // cache miss is not an error
		}
		return nil, errors.Wrap(err, "get pr state")
	}
	return &r, nil
}

// SetPRState upserts the last-seen head SHA for a PR.
func (s *Store) SetPRState(ctx context.Context, repoNodeID string, prNumber int64, headSHA string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mwl_pr_state (repo_node_id, pr_number, head_sha, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (repo_node_id, pr_number) DO UPDATE
		    SET head_sha   = EXCLUDED.head_sha,
		        updated_at = NOW()
	`, repoNodeID, prNumber, headSHA)
	return errors.Wrap(err, "set pr state")
}

func isNoRows(err error) bool {
	return err != nil && err.Error() == "no rows in result set"
}
