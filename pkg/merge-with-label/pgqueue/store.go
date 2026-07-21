// Package pgqueue implements a Postgres-backed job queue with SKIP LOCKED
// dequeuing and a PR-state deduplication cache.
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

const schema = `
CREATE TABLE IF NOT EXISTS mwl_queue (
    id           BIGSERIAL PRIMARY KEY,
    msg_type     TEXT        NOT NULL,
    dedup_key    TEXT        NOT NULL,
    payload      JSONB       NOT NULL,
    attempts     INT         NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (msg_type, dedup_key)
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

// Enqueue inserts a job.  If a job with the same (msg_type, dedup_key) already
// exists in the queue it is silently ignored (ON CONFLICT DO NOTHING).
// When available_at is zero the job is immediately available.
func (s *Store) Enqueue(ctx context.Context, msgType, dedupKey string, payload []byte, availableAt time.Time) error {
	if availableAt.IsZero() {
		availableAt = time.Now()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mwl_queue (msg_type, dedup_key, payload, available_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (msg_type, dedup_key) DO NOTHING
	`, msgType, dedupKey, payload, availableAt)
	return errors.Wrap(err, "enqueue")
}

// Job is a single dequeued row.
type Job struct {
	ID      int64
	Payload []byte
}

// Dequeue claims the next available job of msgType using SKIP LOCKED.
// Returns nil, nil when the queue is empty.
func (s *Store) Dequeue(ctx context.Context, msgType string) (*Job, error) {
	var j Job
	err := s.pool.QueryRow(ctx, `
		DELETE FROM mwl_queue
		WHERE id = (
			SELECT id FROM mwl_queue
			WHERE  msg_type = $1
			  AND  available_at <= NOW()
			ORDER BY available_at, id
			LIMIT  1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, payload
	`, msgType).Scan(&j.ID, &j.Payload)
	if err != nil {
		if isNoRows(err) {
			return nil, nil //nolint:nilnil // empty queue is not an error
		}
		return nil, errors.Wrap(err, "dequeue")
	}
	return &j, nil
}

// Reschedule puts a job back with a new available_at (for retry).
// It updates the existing row by id; if the row was already deleted (race), it
// re-inserts using the original dedup_key and msg_type.
func (s *Store) Reschedule(ctx context.Context, id int64, msgType, dedupKey string, payload []byte, delay time.Duration) error {
	availableAt := time.Now().Add(delay)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mwl_queue (id, msg_type, dedup_key, payload, available_at, attempts)
		VALUES ($1, $2, $3, $4, $5, 1)
		ON CONFLICT (msg_type, dedup_key) DO UPDATE
		    SET available_at = EXCLUDED.available_at,
		        attempts     = mwl_queue.attempts + 1
	`, id, msgType, dedupKey, payload, availableAt)
	return errors.Wrap(err, "reschedule")
}

// KVGet retrieves a value from the key-value store.  Returns nil, nil when the
// key does not exist or has expired.
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

// KVSet stores a value.  Pass zero ttl for no expiry.
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
