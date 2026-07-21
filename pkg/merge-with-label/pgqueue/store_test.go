package pgqueue_test

import (
	"context"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
)

// newTestStore spins up a temporary Postgres container and returns a
// migrated Store and a cleanup function.
func newTestStore(t *testing.T) (*pgqueue.Store, func()) {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("get connection string: %v", err)
	}

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

	cleanup := func() {
		store.Close()
		_ = ctr.Terminate(ctx)
	}
	return store, cleanup
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
	if string(job.Payload) != string(payload) {
		t.Errorf("payload = %s, want %s", job.Payload, payload)
	}

	// Queue should be empty now.
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

	// Two inserts with the same dedup_key: only one row should exist.
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
	// First payload wins (ON CONFLICT DO NOTHING).
	if string(job.Payload) != `{"v":1}` {
		t.Errorf("unexpected payload %s", job.Payload)
	}

	// No second job.
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

	// Should not be dequeued yet.
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

	// Reschedule with a tiny delay (1 ms) so it becomes available immediately.
	dropped, err := store.RescheduleRepo(ctx, job.ID, "retry-key", payload, time.Millisecond, 5)
	if err != nil {
		t.Fatalf("RescheduleRepo: %v", err)
	}
	if dropped {
		t.Error("expected not dropped (attempt 1 of 5)")
	}

	// Should be dequeue-able again.
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
	job, _ := store.DequeueRepo(ctx) //nolint:errcheck // checked below

	// maxAttempts = 1 means drop on the very first reschedule.
	dropped, err := store.RescheduleRepo(ctx, job.ID, "exhaust-key", payload, time.Millisecond, 1)
	if err != nil {
		t.Fatalf("RescheduleRepo: %v", err)
	}
	if !dropped {
		t.Error("expected job to be dropped at maxAttempts=1")
	}

	// Queue must be empty.
	empty, _ := store.DequeueRepo(ctx) //nolint:errcheck
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
	if string(job.Payload) != string(payload) {
		t.Errorf("payload = %s, want %s", job.Payload, payload)
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

	job, _ := store.DequeuePR(ctx) //nolint:errcheck
	if job == nil {
		t.Fatal("expected one job")
	}
	if string(job.Payload) != `{"n":1}` {
		t.Errorf("unexpected payload %s", job.Payload)
	}
	second, _ := store.DequeuePR(ctx) //nolint:errcheck
	if second != nil {
		t.Error("expected queue to be empty after dedup")
	}
}

func TestQueuesAreIndependent(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// Enqueue in PR queue; repo queue must stay empty (and vice versa).
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
	if string(got) != "value" {
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

	// Store with a TTL that has already passed.
	pastTTL := -time.Millisecond
	if err := store.KVSet(ctx, "b", "expired-key", []byte("v"), pastTTL); err != nil {
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
	got, _ := store.KVGet(ctx, "b", "k") //nolint:errcheck
	if string(got) != "second" {
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
	state, _ := store.GetPRState(ctx, "r", 1) //nolint:errcheck
	if state.HeadSHA != "new-sha" {
		t.Errorf("expected overwrite, got %q", state.HeadSHA)
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

func TestPRStateScopedToRepo(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.SetPRState(ctx, "repo-A", 1, "sha-a"); err != nil {
		t.Fatal(err)
	}

	// Same PR number, different repo — must not bleed.
	state, err := store.GetPRState(ctx, "repo-B", 1)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Error("PR state must be scoped to repo_node_id")
	}
}

// -----------------------------------------------------------------------
// WaitForSchema
// -----------------------------------------------------------------------

func TestWaitForSchema(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// Schema is already applied by newTestStore; WaitForSchema must return immediately.
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

func TestKVExpiryDeletesRowLazily(t *testing.T) {
	// After KVGet on an expired key the row must be physically gone
	// (lazy delete-on-miss via the CTE in KVGet).
	store, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.KVSet(ctx, "lazy", "k", []byte("v"), -time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// First read triggers the lazy delete.
	_, _ = store.KVGet(ctx, "lazy", "k") //nolint:errcheck

	// Write a fresh value with no TTL.
	if err := store.KVSet(ctx, "lazy", "k", []byte("fresh"), 0); err != nil {
		t.Fatal(err)
	}
	got, err := store.KVGet(ctx, "lazy", "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh" {
		t.Errorf("expected fresh value after lazy delete, got %q", got)
	}
}
