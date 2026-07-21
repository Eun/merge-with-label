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
	"database/sql"
	"embed"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pkg/errors"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is the single entry-point for the Postgres queue and cache tables.
type Store struct {
	pool *pgxpool.Pool
	db   *sql.DB // stdlib wrapper used only by goose
}

// New connects to Postgres using connString and returns a ready Store.
// It does NOT run migrations — call Migrate explicitly from the server.
func New(ctx context.Context, connString string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, errors.Wrap(err, "unable to parse postgres connection string")
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create postgres pool")
	}

	// stdlib.OpenDBFromPool gives goose a *sql.DB backed by the same pool.
	db := stdlib.OpenDBFromPool(pool)

	return &Store{pool: pool, db: db}, nil
}

// Close releases all pool connections.
func (s *Store) Close() {
	_ = s.db.Close()
	s.pool.Close()
}

// Migrate runs all pending goose migrations in the Up direction.
// Called by the server on startup before it begins serving requests.
func (s *Store) Migrate(ctx context.Context) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return errors.Wrap(err, "goose: set dialect")
	}
	if err := goose.UpContext(ctx, s.db, "migrations"); err != nil {
		return errors.Wrap(err, "goose: up")
	}
	return nil
}

// gooseVersion returns the current applied schema version, or -1 on error.
func (s *Store) gooseVersion(ctx context.Context) (int64, error) {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return -1, errors.Wrap(err, "goose: set dialect")
	}
	v, err := goose.GetDBVersionContext(ctx, s.db)
	if err != nil {
		return -1, errors.Wrap(err, "goose: get version")
	}
	return v, nil
}

// maxMigrationVersion returns the highest version present in the embedded
// migration files (the version the schema should be at after Migrate).
func maxMigrationVersion() (int64, error) {
	goose.SetBaseFS(migrationsFS)
	migrations, err := goose.CollectMigrations("migrations", 0, goose.MaxVersion)
	if err != nil {
		return -1, errors.Wrap(err, "goose: collect migrations")
	}
	if len(migrations) == 0 {
		return 0, nil
	}
	return migrations[len(migrations)-1].Version, nil
}

// WaitForSchema blocks until the database schema is fully up-to-date (i.e.
// the applied version equals the maximum migration version defined in the
// embedded files). It polls every second and respects ctx cancellation.
// Called by the worker on startup so it never processes jobs before the
// server has finished running the migrations.
func (s *Store) WaitForSchema(ctx context.Context) error {
	want, err := maxMigrationVersion()
	if err != nil {
		return err
	}

	for {
		got, err := s.gooseVersion(ctx)
		if err == nil && got >= want {
			return nil
		}
		if err != nil {
			// Goose version table may not exist yet — that's fine, keep waiting.
			_ = err
		}

		select {
		case <-ctx.Done():
			return errors.Wrap(ctx.Err(), "timed out waiting for schema migration")
		case <-time.After(time.Second):
		}
	}
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
func (s *Store) RescheduleRepo(
	ctx context.Context, id int64, dedupKey string, payload []byte, delay time.Duration, maxAttempts int,
) (bool, error) {
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
func (s *Store) ReschedulePR(
	ctx context.Context, id int64, dedupKey string, payload []byte, delay time.Duration, maxAttempts int,
) (bool, error) {
	return reschedule(ctx, s.pool, "mwl_pr_queue", id, dedupKey, payload, delay, maxAttempts)
}

// -----------------------------------------------------------------------
// Shared queue helpers
// -----------------------------------------------------------------------

func dequeue(ctx context.Context, pool *pgxpool.Pool, table string) (*Job, error) {
	var j Job
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

func reschedule(
	ctx context.Context, pool *pgxpool.Pool, table string,
	id int64, dedupKey string, payload []byte,
	delay time.Duration, maxAttempts int,
) (dropped bool, err error) {
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
