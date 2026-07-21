package pgqueue_test

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
)

// postgresDockerfilePath returns the path to docker/postgres/ relative to this file.
func postgresDockerfilePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "docker", "postgres")
}

// newTestStore spins up a postgres+pg_cron container built from docker/postgres/Dockerfile
// and returns a migrated Store and a cleanup function.
func newTestStore(t *testing.T) (s *pgqueue.Store, cleanup func()) {
	t.Helper()
	ctx := context.Background()

	dockerCtx := postgresDockerfilePath(t)

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    dockerCtx,
			Dockerfile: "Dockerfile",
			KeepImage:  false,
		},
		Env: map[string]string{
			"POSTGRES_DB":       "testdb",
			"POSTGRES_USER":     "test",
			"POSTGRES_PASSWORD": "test",
		},
		Cmd: []string{
			"-c", "shared_preload_libraries=pg_cron",
			"-c", "cron.database_name=testdb",
		},
		ExposedPorts: []string{"5432/tcp"},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("get host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "5432")
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("get port: %v", err)
	}
	dsn := "postgres://test:test@" + host + ":" + port.Port() + "/testdb?sslmode=disable"

	store, err := pgqueue.New(ctx, dsn)
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("connect to postgres: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		store.Close()
		_ = ctr.Terminate(ctx)
		t.Fatalf("migrate: %v", err)
	}

	return store, func() {
		store.Close()
		_ = ctr.Terminate(ctx)
	}
}

// -----------------------------------------------------------------------
// Repo queue
// -----------------------------------------------------------------------

func TestEnqueueDequeueRepo(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	payload := []byte(`{"repo":"test"}`)
	if err := store.EnqueueRepo(ctx, "key-1", payload, time.Time{}); err != nil {
		t.Fatalf("EnqueueRepo: %v", err)
	}

	job, err := store.DequeueRepo(ctx)
	if err != nil {
		t.Fatalf("DequeueRepo: %v", err)
	}
	if job == nil {
		t.Fatal("expected a job, got nil")
	}
	if job.Payload == nil {
		t.Error("expected non-nil payload")
	}

	empty, err := store.DequeueRepo(ctx)
	if err != nil {
		t.Fatalf("DequeueRepo (empty): %v", err)
	}
	if empty != nil {
		t.Errorf("expected nil after drain, got job id=%d", empty.ID)
	}
}

func TestEnqueueRepoDeduplicate(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.EnqueueRepo(ctx, "same-key", []byte(`{"v":1}`), time.Time{}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := store.EnqueueRepo(ctx, "same-key", []byte(`{"v":2}`), time.Time{}); err != nil {
		t.Fatalf("second enqueue: %v", err)
	}

	job, err := store.DequeueRepo(ctx)
	if err != nil {
		t.Fatalf("DequeueRepo: %v", err)
	}
	if job == nil {
		t.Fatal("expected one job")
	}
	// First payload wins (ON CONFLICT DO NOTHING) — JSONB may add spaces.
	if !bytes.Contains(job.Payload, []byte(`"v"`)) {
		t.Errorf("unexpected payload %s", job.Payload)
	}

	second, err := store.DequeueRepo(ctx)
	if err != nil {
		t.Fatalf("DequeueRepo second: %v", err)
	}
	if second != nil {
		t.Error("expected queue to be empty after dedup")
	}
}

func TestRepoQueueFutureAvailableAt(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	future := time.Now().Add(10 * time.Minute)
	if err := store.EnqueueRepo(ctx, "future-key", []byte(`{}`), future); err != nil {
		t.Fatalf("EnqueueRepo: %v", err)
	}

	job, err := store.DequeueRepo(ctx)
	if err != nil {
		t.Fatalf("DequeueRepo: %v", err)
	}
	if job != nil {
		t.Error("expected nil for future job, got a job")
	}
}

func TestRescheduleRepo(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	payload := []byte(`{"repo":"retry"}`)
	if err := store.EnqueueRepo(ctx, "retry-key", payload, time.Time{}); err != nil {
		t.Fatalf("EnqueueRepo: %v", err)
	}

	job, err := store.DequeueRepo(ctx)
	if err != nil || job == nil {
		t.Fatalf("DequeueRepo: %v, job=%v", err, job)
	}

	dropped, err := store.RescheduleRepo(ctx, job.ID, "retry-key", payload, time.Millisecond, 5)
	if err != nil {
		t.Fatalf("RescheduleRepo: %v", err)
	}
	if dropped {
		t.Error("expected not dropped (attempt 1 of 5)")
	}

	retried, err := store.DequeueRepo(ctx)
	if err != nil {
		t.Fatalf("DequeueRepo retry: %v", err)
	}
	if retried == nil {
		t.Fatal("expected rescheduled job to be available")
	}
}

func TestRescheduleRepoMaxAttempts(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	payload := []byte(`{}`)
	if err := store.EnqueueRepo(ctx, "exhaust-key", payload, time.Time{}); err != nil {
		t.Fatalf("EnqueueRepo: %v", err)
	}
	job, err := store.DequeueRepo(ctx)
	if err != nil || job == nil {
		t.Fatalf("DequeueRepo: %v", err)
	}

	dropped, err := store.RescheduleRepo(ctx, job.ID, "exhaust-key", payload, time.Millisecond, 1)
	if err != nil {
		t.Fatalf("RescheduleRepo: %v", err)
	}
	if !dropped {
		t.Error("expected job to be dropped at maxAttempts=1")
	}

	empty, err := store.DequeueRepo(ctx)
	if err != nil {
		t.Fatalf("DequeueRepo after exhaust: %v", err)
	}
	if empty != nil {
		t.Error("expected empty queue after exhaustion")
	}
}

// -----------------------------------------------------------------------
// PR queue
// -----------------------------------------------------------------------

func TestEnqueueDequeuePR(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	payload := []byte(`{"pr":42}`)
	if err := store.EnqueuePR(ctx, "pr-1", payload, time.Time{}); err != nil {
		t.Fatalf("EnqueuePR: %v", err)
	}

	job, err := store.DequeuePR(ctx)
	if err != nil {
		t.Fatalf("DequeuePR: %v", err)
	}
	if job == nil {
		t.Fatal("expected a job, got nil")
	}
	if job.Payload == nil {
		t.Error("expected non-nil payload")
	}
}

func TestEnqueuePRDeduplicate(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.EnqueuePR(ctx, "pr-dup", []byte(`{"n":1}`), time.Time{}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := store.EnqueuePR(ctx, "pr-dup", []byte(`{"n":2}`), time.Time{}); err != nil {
		t.Fatalf("second: %v", err)
	}

	job, err := store.DequeuePR(ctx)
	if err != nil {
		t.Fatalf("DequeuePR: %v", err)
	}
	if job == nil {
		t.Fatal("expected one job")
	}
	if !bytes.Contains(job.Payload, []byte(`"n"`)) {
		t.Errorf("unexpected payload %s", job.Payload)
	}
	second, err := store.DequeuePR(ctx)
	if err != nil {
		t.Fatalf("second DequeuePR: %v", err)
	}
	if second != nil {
		t.Error("expected queue to be empty after dedup")
	}
}

func TestQueuesAreIndependent(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.EnqueuePR(ctx, "independent", []byte(`{}`), time.Time{}); err != nil {
		t.Fatal(err)
	}
	repoJob, err := store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repoJob != nil {
		t.Error("repo queue should be empty when only PR was enqueued")
	}
	prJob, err := store.DequeuePR(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if prJob == nil {
		t.Error("PR queue should have one job")
	}
}

// -----------------------------------------------------------------------
// Key-value store
// -----------------------------------------------------------------------

func TestKVSetGet(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.KVSet(ctx, "bucket", "key", []byte("value"), 0); err != nil {
		t.Fatalf("KVSet: %v", err)
	}
	got, err := store.KVGet(ctx, "bucket", "key")
	if err != nil {
		t.Fatalf("KVGet: %v", err)
	}
	if !bytes.Equal(got, []byte("value")) {
		t.Errorf("KVGet = %q, want %q", got, "value")
	}
}

func TestKVMiss(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	got, err := store.KVGet(ctx, "bucket", "nonexistent")
	if err != nil {
		t.Fatalf("KVGet: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil on miss, got %q", got)
	}
}

func TestKVExpiry(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.KVSet(ctx, "b", "expired-key", []byte("v"), -time.Millisecond); err != nil {
		t.Fatalf("KVSet: %v", err)
	}
	got, err := store.KVGet(ctx, "b", "expired-key")
	if err != nil {
		t.Fatalf("KVGet: %v", err)
	}
	if got != nil {
		t.Error("expected nil for expired key")
	}
}

func TestKVOverwrite(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.KVSet(ctx, "b", "k", []byte("first"), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.KVSet(ctx, "b", "k", []byte("second"), 0); err != nil {
		t.Fatal(err)
	}
	got, err := store.KVGet(ctx, "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("second")) {
		t.Errorf("expected overwrite, got %q", got)
	}
}

// -----------------------------------------------------------------------
// PR state
// -----------------------------------------------------------------------

func TestSetGetPRState(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.SetPRState(ctx, "repo-1", 42, "sha-abc"); err != nil {
		t.Fatalf("SetPRState: %v", err)
	}
	state, err := store.GetPRState(ctx, "repo-1", 42)
	if err != nil {
		t.Fatalf("GetPRState: %v", err)
	}
	if state == nil {
		t.Fatal("expected state, got nil")
	}
	if state.HeadSHA != "sha-abc" {
		t.Errorf("HeadSHA = %q, want %q", state.HeadSHA, "sha-abc")
	}
}

func TestPRStateMiss(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	state, err := store.GetPRState(ctx, "unknown-repo", 1)
	if err != nil {
		t.Fatalf("GetPRState: %v", err)
	}
	if state != nil {
		t.Error("expected nil on miss")
	}
}

func TestPRStateOverwrite(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.SetPRState(ctx, "r", 1, "old-sha"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPRState(ctx, "r", 1, "new-sha"); err != nil {
		t.Fatal(err)
	}
	state, err := store.GetPRState(ctx, "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.HeadSHA != "new-sha" {
		t.Errorf("expected overwrite, got %q", state.HeadSHA)
	}
}

func TestPRStateScopedToRepo(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.SetPRState(ctx, "repo-A", 1, "sha-a"); err != nil {
		t.Fatal(err)
	}

	state, err := store.GetPRState(ctx, "repo-B", 1)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Error("PR state must be scoped to repo_node_id")
	}
}

func TestDeletePRState(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.SetPRState(ctx, "repo-del", 5, "sha"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePRState(ctx, "repo-del", 5); err != nil {
		t.Fatalf("DeletePRState: %v", err)
	}
	state, err := store.GetPRState(ctx, "repo-del", 5)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Error("expected nil after delete")
	}
}

func TestDeletePRStateIdempotent(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.DeletePRState(ctx, "ghost-repo", 99); err != nil {
		t.Errorf("DeletePRState on missing row: %v", err)
	}
}

// -----------------------------------------------------------------------
// WaitForSchema + pg_cron + UNLOGGED
// -----------------------------------------------------------------------

func TestWaitForSchema(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	done := make(chan error, 1)
	go func() { done <- store.WaitForSchema(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("WaitForSchema returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("WaitForSchema did not return within 5s")
	}
}

func TestKVUnloggedAndCron(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	var persistence string
	if err := store.QueryRow(ctx,
		`SELECT relpersistence FROM pg_class WHERE relname = 'mwl_kv'`,
	).Scan(&persistence); err != nil {
		t.Fatalf("query relpersistence: %v", err)
	}
	if persistence != "u" {
		t.Errorf("mwl_kv relpersistence = %q, want 'u' (unlogged)", persistence)
	}

	var count int
	if err := store.QueryRow(ctx,
		`SELECT COUNT(*) FROM cron.job WHERE jobname = 'mwl_kv_cleanup'`,
	).Scan(&count); err != nil {
		t.Fatalf("query cron.job: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 cron job named mwl_kv_cleanup, got %d", count)
	}
}
