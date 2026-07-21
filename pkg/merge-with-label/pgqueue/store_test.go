package pgqueue_test

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// Each test uses unique keys derived from the test name so they don't
// interfere with each other when running against the shared container.

// -----------------------------------------------------------------------
// Repo queue
// -----------------------------------------------------------------------

func TestEnqueueDequeueRepo(t *testing.T) {
	ctx := context.Background()
	key := "repo-basic-" + t.Name()
	payload := []byte(`{"repo":"test"}`)

	if err := sharedStore.EnqueueRepo(ctx, key, payload, time.Time{}); err != nil {
		t.Fatalf("EnqueueRepo: %v", err)
	}
	job, err := sharedStore.DequeueRepo(ctx)
	if err != nil {
		t.Fatalf("DequeueRepo: %v", err)
	}
	if job == nil {
		t.Fatal("expected a job, got nil")
	}
	if job.Payload == nil {
		t.Error("expected non-nil payload")
	}
}

func TestEnqueueRepoDeduplicate(t *testing.T) {
	ctx := context.Background()
	key := "repo-dedup-" + t.Name()

	if err := sharedStore.EnqueueRepo(ctx, key, []byte(`{"v":1}`), time.Time{}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := sharedStore.EnqueueRepo(ctx, key, []byte(`{"v":2}`), time.Time{}); err != nil {
		t.Fatalf("second enqueue: %v", err)
	}

	job, err := sharedStore.DequeueRepo(ctx)
	if err != nil {
		t.Fatalf("DequeueRepo: %v", err)
	}
	if job == nil {
		t.Fatal("expected one job")
	}
	if !bytes.Contains(job.Payload, []byte(`"v"`)) {
		t.Errorf("unexpected payload %s", job.Payload)
	}
}

func TestRepoQueueFutureAvailableAt(t *testing.T) {
	ctx := context.Background()
	key := "repo-future-" + t.Name()

	future := time.Now().Add(10 * time.Minute)
	if err := sharedStore.EnqueueRepo(ctx, key, []byte(`{}`), future); err != nil {
		t.Fatalf("EnqueueRepo: %v", err)
	}
	// A future job must NOT be returned immediately.
	// We drain the current queue and check that this key is not returned.
	for i := range 5 {
		job, err := sharedStore.DequeueRepo(ctx)
		if err != nil {
			t.Fatalf("DequeueRepo %d: %v", i, err)
		}
		if job == nil {
			break // queue empty
		}
		if bytes.Contains(job.Payload, []byte("repo-future")) {
			t.Error("future job should not be dequeued immediately")
		}
	}
}

func TestRescheduleRepo(t *testing.T) {
	ctx := context.Background()
	key := "repo-retry-" + t.Name()
	payload := []byte(`{"key":"` + key + `"}`)

	if err := sharedStore.EnqueueRepo(ctx, key, payload, time.Time{}); err != nil {
		t.Fatalf("EnqueueRepo: %v", err)
	}
	job, err := sharedStore.DequeueRepo(ctx)
	if err != nil || job == nil {
		t.Fatalf("DequeueRepo: err=%v job=%v", err, job)
	}

	dropped, err := sharedStore.RescheduleRepo(ctx, job.ID, key, payload, time.Millisecond, 5)
	if err != nil {
		t.Fatalf("RescheduleRepo: %v", err)
	}
	if dropped {
		t.Error("expected not dropped (attempt 1 of 5)")
	}

	time.Sleep(10 * time.Millisecond)
	// Drain until we find our key or the queue is empty.
	var found bool
	for i := range 20 {
		retried, err := sharedStore.DequeueRepo(ctx)
		if err != nil {
			t.Fatalf("DequeueRepo retry %d: %v", i, err)
		}
		if retried == nil {
			break
		}
		if bytes.Contains(retried.Payload, []byte(key)) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected rescheduled job to be available")
	}
}

func TestRescheduleRepoMaxAttempts(t *testing.T) {
	ctx := context.Background()
	key := "repo-exhaust-" + t.Name()
	payload := []byte(`{}`)

	if err := sharedStore.EnqueueRepo(ctx, key, payload, time.Time{}); err != nil {
		t.Fatalf("EnqueueRepo: %v", err)
	}
	job, err := sharedStore.DequeueRepo(ctx)
	if err != nil || job == nil {
		t.Fatalf("DequeueRepo: err=%v", err)
	}

	dropped, err := sharedStore.RescheduleRepo(ctx, job.ID, key, payload, time.Millisecond, 1)
	if err != nil {
		t.Fatalf("RescheduleRepo: %v", err)
	}
	if !dropped {
		t.Error("expected job to be dropped at maxAttempts=1")
	}
}

// -----------------------------------------------------------------------
// PR queue
// -----------------------------------------------------------------------

func TestEnqueueDequeuePR(t *testing.T) {
	ctx := context.Background()
	key := "pr-basic-" + t.Name()

	if err := sharedStore.EnqueuePR(ctx, key, []byte(`{"pr":42}`), time.Time{}); err != nil {
		t.Fatalf("EnqueuePR: %v", err)
	}
	job, err := sharedStore.DequeuePR(ctx)
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
	ctx := context.Background()
	key := "pr-dedup-" + t.Name()

	if err := sharedStore.EnqueuePR(ctx, key, []byte(`{"n":1}`), time.Time{}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := sharedStore.EnqueuePR(ctx, key, []byte(`{"n":2}`), time.Time{}); err != nil {
		t.Fatalf("second: %v", err)
	}

	job, err := sharedStore.DequeuePR(ctx)
	if err != nil {
		t.Fatalf("DequeuePR: %v", err)
	}
	if job == nil {
		t.Fatal("expected one job")
	}
	if !bytes.Contains(job.Payload, []byte(`"n"`)) {
		t.Errorf("unexpected payload %s", job.Payload)
	}
}

func TestQueuesAreIndependent(t *testing.T) {
	ctx := context.Background()
	key := "independent-" + t.Name()

	if err := sharedStore.EnqueuePR(ctx, key, []byte(`{}`), time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Drain the PR queue to get our job.
	var found bool
	for {
		job, err := sharedStore.DequeuePR(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			break
		}
		found = true
	}
	if !found {
		t.Error("PR queue should have had one job")
	}
}

// -----------------------------------------------------------------------
// Key-value store
// -----------------------------------------------------------------------

func TestKVSetGet(t *testing.T) {
	ctx := context.Background()
	if err := sharedStore.KVSet(ctx, "test", t.Name(), []byte("value"), 0); err != nil {
		t.Fatalf("KVSet: %v", err)
	}
	got, err := sharedStore.KVGet(ctx, "test", t.Name())
	if err != nil {
		t.Fatalf("KVGet: %v", err)
	}
	if !bytes.Equal(got, []byte("value")) {
		t.Errorf("KVGet = %q, want %q", got, "value")
	}
}

func TestKVMiss(t *testing.T) {
	ctx := context.Background()
	got, err := sharedStore.KVGet(ctx, "test", "nonexistent-"+t.Name())
	if err != nil {
		t.Fatalf("KVGet: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil on miss, got %q", got)
	}
}

func TestKVExpiry(t *testing.T) {
	ctx := context.Background()
	if err := sharedStore.KVSet(ctx, "test", t.Name(), []byte("v"), -time.Millisecond); err != nil {
		t.Fatalf("KVSet: %v", err)
	}
	got, err := sharedStore.KVGet(ctx, "test", t.Name())
	if err != nil {
		t.Fatalf("KVGet: %v", err)
	}
	if got != nil {
		t.Error("expected nil for expired key")
	}
}

func TestKVOverwrite(t *testing.T) {
	ctx := context.Background()
	if err := sharedStore.KVSet(ctx, "test", t.Name(), []byte("first"), 0); err != nil {
		t.Fatal(err)
	}
	if err := sharedStore.KVSet(ctx, "test", t.Name(), []byte("second"), 0); err != nil {
		t.Fatal(err)
	}
	got, err := sharedStore.KVGet(ctx, "test", t.Name())
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
	ctx := context.Background()
	if err := sharedStore.SetPRState(ctx, t.Name(), 42, "sha-abc"); err != nil {
		t.Fatalf("SetPRState: %v", err)
	}
	state, err := sharedStore.GetPRState(ctx, t.Name(), 42)
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
	ctx := context.Background()
	state, err := sharedStore.GetPRState(ctx, "unknown-"+t.Name(), 1)
	if err != nil {
		t.Fatalf("GetPRState: %v", err)
	}
	if state != nil {
		t.Error("expected nil on miss")
	}
}

func TestPRStateOverwrite(t *testing.T) {
	ctx := context.Background()
	if err := sharedStore.SetPRState(ctx, t.Name(), 1, "old-sha"); err != nil {
		t.Fatal(err)
	}
	if err := sharedStore.SetPRState(ctx, t.Name(), 1, "new-sha"); err != nil {
		t.Fatal(err)
	}
	state, err := sharedStore.GetPRState(ctx, t.Name(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.HeadSHA != "new-sha" {
		t.Errorf("expected overwrite, got %q", state.HeadSHA)
	}
}

func TestPRStateScopedToRepo(t *testing.T) {
	ctx := context.Background()
	if err := sharedStore.SetPRState(ctx, "repo-A-"+t.Name(), 1, "sha-a"); err != nil {
		t.Fatal(err)
	}
	state, err := sharedStore.GetPRState(ctx, "repo-B-"+t.Name(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Error("PR state must be scoped to repo_node_id")
	}
}

func TestDeletePRState(t *testing.T) {
	ctx := context.Background()
	if err := sharedStore.SetPRState(ctx, t.Name(), 5, "sha"); err != nil {
		t.Fatal(err)
	}
	if err := sharedStore.DeletePRState(ctx, t.Name(), 5); err != nil {
		t.Fatalf("DeletePRState: %v", err)
	}
	state, err := sharedStore.GetPRState(ctx, t.Name(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Error("expected nil after delete")
	}
}

func TestDeletePRStateIdempotent(t *testing.T) {
	ctx := context.Background()
	if err := sharedStore.DeletePRState(ctx, "ghost-"+t.Name(), 99); err != nil {
		t.Errorf("DeletePRState on missing row: %v", err)
	}
}

// -----------------------------------------------------------------------
// WaitForSchema + pg_cron + UNLOGGED
// -----------------------------------------------------------------------

func TestWaitForSchema(t *testing.T) {
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- sharedStore.WaitForSchema(ctx) }()
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
	ctx := context.Background()

	var persistence string
	if err := sharedStore.QueryRow(ctx,
		`SELECT relpersistence FROM pg_class WHERE relname = 'mwl_kv'`,
	).Scan(&persistence); err != nil {
		t.Fatalf("query relpersistence: %v", err)
	}
	if persistence != "u" {
		t.Errorf("mwl_kv relpersistence = %q, want 'u' (unlogged)", persistence)
	}

	var count int
	if err := sharedStore.QueryRow(ctx,
		`SELECT COUNT(*) FROM cron.job WHERE jobname = 'mwl_kv_cleanup'`,
	).Scan(&count); err != nil {
		t.Fatalf("query cron.job: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 cron job named mwl_kv_cleanup, got %d", count)
	}
}
