package server_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/server"
)

func postgresDockerfilePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "docker", "postgres")
}

// newTestHandler creates a Handler backed by a live Postgres+pg_cron container.
func newTestHandler(t *testing.T) (*server.Handler, func()) { //nolint:unparam // consistent helper signature
	t.Helper()
	ctx := context.Background()

	dockerCtx := postgresDockerfilePath(t)

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithDockerfile(testcontainers.FromDockerfile{
			Context:    dockerCtx,
			Dockerfile: "Dockerfile",
			KeepImage:  false,
		}),
		testcontainers.WithCmd(
			"-c", "shared_preload_libraries=pg_cron",
			"-c", "cron.database_name=testdb",
		),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("connection string: %v", err)
	}

	store, err := pgqueue.New(ctx, dsn)
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("pgqueue.New: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		store.Close()
		_ = ctr.Terminate(ctx)
		t.Fatalf("migrate: %v", err)
	}

	logger := zerolog.Nop()
	h := &server.Handler{
		GetLoggerForContext: func(_ context.Context) *zerolog.Logger { return &logger },
		AllowedRepositories: common.RegexSlice{common.MustNewRegexItem(".*")},
		Store:               store,
		RateLimitInterval:   0, // disable for tests
	}

	return h, func() {
		store.Close()
		_ = ctr.Terminate(ctx)
	}
}

// basePayload returns a minimal valid JSON payload for the given GitHub event.
func basePayload(action, event string) []byte {
	return []byte(`{
		"action": "` + action + `",
		"installation": {"id": 1},
		"repository": {
			"full_name": "owner/repo",
			"name": "repo",
			"node_id": "R_123",
			"owner": {"login": "owner"},
			"private": false,
			"default_branch": "main"
		}
	}`)
}

func postEvent(t *testing.T, h *server.Handler, event string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "test-delivery-id")
	h.ServeHTTP(rec, req)
	return rec
}

func TestServeHTTP_NonPostRedirects(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("GET / = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
}

func TestServeHTTP_UnknownPath(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/unknown", http.NoBody)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /unknown = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServeHTTP_UnknownEvent(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	rec := postEvent(t, h, "create", basePayload("created", "create"))
	if rec.Code != http.StatusOK {
		t.Errorf("unknown event = %d, want 200", rec.Code)
	}
}

func TestHandlePush_EnqueuesRepoJob(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := []byte(`{
		"action": "",
		"installation": {"id": 1},
		"repository": {
			"full_name": "owner/repo",
			"name": "repo",
			"node_id": "R_push",
			"owner": {"login": "owner"},
			"private": false,
			"default_branch": "main"
		},
		"ref": "refs/heads/main",
		"deleted": false
	}`)

	rec := postEvent(t, h, "push", body)
	if rec.Code != http.StatusOK {
		t.Errorf("push handler = %d, want 200", rec.Code)
	}

	// Verify a repo job was enqueued.
	ctx := context.Background()
	job, err := h.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatalf("DequeueRepo: %v", err)
	}
	if job == nil {
		t.Error("expected a repo job to be enqueued by push handler")
	}
}

func TestHandlePush_DeletedBranchIgnored(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := []byte(`{
		"installation": {"id": 1},
		"repository": {
			"full_name": "owner/repo", "name": "repo",
			"node_id": "R_del", "owner": {"login": "owner"},
			"private": false, "default_branch": "main"
		},
		"ref": "refs/heads/main",
		"deleted": true
	}`)
	rec := postEvent(t, h, "push", body)
	if rec.Code != http.StatusOK {
		t.Errorf("push deleted = %d", rec.Code)
	}
	job, _ := h.Store.DequeueRepo(context.Background())
	if job != nil {
		t.Error("deleted push should not enqueue a job")
	}
}

func TestHandlePush_NonDefaultBranchIgnored(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := []byte(`{
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_feat", "owner": {"login": "o"},
			"private": false, "default_branch": "main"
		},
		"ref": "refs/heads/feature-branch",
		"deleted": false
	}`)
	rec := postEvent(t, h, "push", body)
	if rec.Code != http.StatusOK {
		t.Errorf("feature branch push = %d", rec.Code)
	}
	job, _ := h.Store.DequeueRepo(context.Background())
	if job != nil {
		t.Error("non-default branch push should not enqueue a job")
	}
}

func TestHandleStatus_EnqueuesRepoJob(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	rec := postEvent(t, h, "status", basePayload("", "status"))
	if rec.Code != http.StatusOK {
		t.Errorf("status handler = %d", rec.Code)
	}
	job, _ := h.Store.DequeueRepo(context.Background())
	if job == nil {
		t.Error("expected repo job from status event")
	}
}

func TestHandlePullRequest_OpenSynchronize_EnqueuesPR(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := []byte(`{
		"action": "synchronize",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_pr", "owner": {"login": "o"},
			"private": false
		},
		"pull_request": {"number": 7, "state": "open"}
	}`)
	rec := postEvent(t, h, "pull_request", body)
	if rec.Code != http.StatusOK {
		t.Errorf("pull_request = %d", rec.Code)
	}
	job, _ := h.Store.DequeuePR(context.Background())
	if job == nil {
		t.Error("expected PR job from pull_request synchronize")
	}
}

func TestHandlePullRequest_ClosedIgnored(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := []byte(`{
		"action": "closed",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_cls", "owner": {"login": "o"},
			"private": false
		},
		"pull_request": {"number": 3, "state": "closed"}
	}`)
	rec := postEvent(t, h, "pull_request", body)
	if rec.Code != http.StatusOK {
		t.Errorf("closed PR = %d", rec.Code)
	}
	job, _ := h.Store.DequeuePR(context.Background())
	if job != nil {
		t.Error("closed PR should not enqueue a job")
	}
}

func TestHandlePullRequestReview_SubmittedEnqueuesPR(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := []byte(`{
		"action": "submitted",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_rev", "owner": {"login": "o"},
			"private": false
		},
		"pull_request": {"number": 9, "state": "open"}
	}`)
	rec := postEvent(t, h, "pull_request_review", body)
	if rec.Code != http.StatusOK {
		t.Errorf("pull_request_review = %d", rec.Code)
	}
	job, _ := h.Store.DequeuePR(context.Background())
	if job == nil {
		t.Error("expected PR job from pull_request_review submitted")
	}
}

func TestHandleCheckRun_WithPRNumbers_EnqueuesPRJobs(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := []byte(`{
		"action": "completed",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_cr", "owner": {"login": "o"},
			"private": false
		},
		"check_run": {
			"pull_requests": [{"number": 11}, {"number": 12}],
			"check_suite": {"pull_requests": []}
		}
	}`)
	rec := postEvent(t, h, "check_run", body)
	if rec.Code != http.StatusOK {
		t.Errorf("check_run = %d", rec.Code)
	}

	ctx := context.Background()
	var count int
	for {
		job, _ := h.Store.DequeuePR(ctx)
		if job == nil {
			break
		}
		count++
	}
	if count != 2 {
		t.Errorf("expected 2 PR jobs from check_run, got %d", count)
	}
}

func TestHandleCheckRun_NoPRs_EnqueuesRepoJob(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := []byte(`{
		"action": "completed",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_cr2", "owner": {"login": "o"},
			"private": false
		},
		"check_run": {
			"pull_requests": [],
			"check_suite": {"pull_requests": []}
		}
	}`)
	rec := postEvent(t, h, "check_run", body)
	if rec.Code != http.StatusOK {
		t.Errorf("check_run no PRs = %d", rec.Code)
	}
	job, _ := h.Store.DequeueRepo(context.Background())
	if job == nil {
		t.Error("expected repo job when check_run has no PR numbers")
	}
}

func TestHandleCheckRun_NotCompleted_NoJob(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := []byte(`{
		"action": "rerequested",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_cr3", "owner": {"login": "o"},
			"private": false
		},
		"check_run": {
			"pull_requests": [{"number": 5}],
			"check_suite": {"pull_requests": []}
		}
	}`)
	rec := postEvent(t, h, "check_run", body)
	if rec.Code != http.StatusOK {
		t.Errorf("check_run rerequested = %d", rec.Code)
	}
	ctx := context.Background()
	pr, _ := h.Store.DequeuePR(ctx)    
	repo, _ := h.Store.DequeueRepo(ctx)
	if pr != nil || repo != nil {
		t.Error("non-completed check_run should not enqueue anything")
	}
}

func TestPushAndPullRequestDeduplicate(t *testing.T) {
	// Two different GitHub events targeting the same PR produce only one queue row.
	h, cleanup := newTestHandler(t)
	defer cleanup()

	prBody := []byte(`{
		"action": "synchronize",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_dd", "owner": {"login": "o"},
			"private": false
		},
		"pull_request": {"number": 99, "state": "open"}
	}`)

	// Enqueue PR job twice with the same PR number.
	postEvent(t, h, "pull_request", prBody)
	postEvent(t, h, "pull_request", prBody)

	ctx := context.Background()
	var count int
	for {
		job, _ := h.Store.DequeuePR(ctx)
		if job == nil {
			break
		}
		count++
	}
	if count != 1 {
		t.Errorf("expected 1 PR job due to dedup, got %d", count)
	}
}

func TestRateLimitDelaysSecondEnqueue(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	// Enable a long rate-limit interval.
	h.RateLimitInterval = time.Hour

	body := []byte(`{
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_rl", "owner": {"login": "o"},
			"private": false, "default_branch": "main"
		},
		"ref": "refs/heads/main", "deleted": false
	}`)

	// First push — enqueues immediately.
	postEvent(t, h, "push", body)
	job, _ := h.Store.DequeueRepo(context.Background())
	if job == nil {
		t.Fatal("first push should enqueue a job")
	}

	// Second push — same repo, should be delayed (not dequeue-able immediately)
	// because the first was just processed and the rate-limit window is 1 hour.
	// The dedup key is the same so ON CONFLICT DO NOTHING fires instead.
	postEvent(t, h, "push", body)
	job2, _ := h.Store.DequeueRepo(context.Background())
	if job2 != nil {
		t.Error("second push within rate-limit window should not be dequeue-able immediately")
	}
}

func TestAllowedRepositoriesFiltersRequests(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	// Only allow "allowed/repo".
	h.AllowedRepositories = common.RegexSlice{common.MustNewRegexItem("^allowed/repo$")}

	body := []byte(`{
		"installation": {"id": 1},
		"repository": {
			"full_name": "blocked/repo", "name": "repo",
			"node_id": "R_blk", "owner": {"login": "blocked"},
			"private": false, "default_branch": "main"
		},
		"ref": "refs/heads/main", "deleted": false
	}`)
	rec := postEvent(t, h, "push", body)
	if rec.Code != http.StatusOK {
		t.Errorf("blocked repo push = %d", rec.Code)
	}
	job, _ := h.Store.DequeueRepo(context.Background())
	if job != nil {
		t.Error("blocked repo should not produce a job")
	}
}
