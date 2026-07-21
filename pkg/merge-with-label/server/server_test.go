package server_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/server"
)

func postgresDockerfilePath(t *testing.T) string {
	t.Helper()
	// os.Getwd() returns the package directory during go test.
	wd, wdErr := os.Getwd()
	if wdErr != nil {
		t.Fatalf("os.Getwd: %v", wdErr)
	}
	return filepath.Join(wd, "..", "..", "..", "docker", "postgres")
}

// newTestHandler creates a Handler backed by a live Postgres+pg_cron container.
func newTestHandler(t *testing.T) (h *server.Handler, cleanup func()) {
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

	host, cErr := ctr.Host(ctx)
	if cErr != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("get host: %v", cErr)
	}
	port, cErr := ctr.MappedPort(ctx, "5432")
	if cErr != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("get port: %v", cErr)
	}
	dsn := "postgres://test:test@" + host + ":" + port.Port() + "/testdb?sslmode=disable"

	store, cErr := pgqueue.New(ctx, dsn)
	if cErr != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("pgqueue.New: %v", cErr)
	}
	if cErr = store.Migrate(ctx); cErr != nil {
		store.Close()
		_ = ctr.Terminate(ctx)
		t.Fatalf("migrate: %v", cErr)
	}

	logger := zerolog.Nop()
	h = &server.Handler{
		GetLoggerForContext: func(_ context.Context) *zerolog.Logger { return &logger },
		AllowedRepositories: common.RegexSlice{common.MustNewRegexItem(".*")},
		Store:               store,
		RateLimitInterval:   0,
	}

	return h, func() {
		store.Close()
		_ = ctr.Terminate(ctx)
	}
}

// basePayload returns a minimal valid JSON payload for a GitHub event.
func basePayload(action string) []byte {
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
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("GET / = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
}

func TestServeHTTP_UnknownPath(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/unknown", http.NoBody)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /unknown = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServeHTTP_UnknownEvent(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	rec := postEvent(t, h, "create", basePayload("created"))
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
	ctx := context.Background()
	job, err := h.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	ctx := context.Background()
	job, err := h.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job != nil {
		t.Error("non-default branch push should not enqueue a job")
	}
}

func TestHandleStatus_EnqueuesRepoJob(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	rec := postEvent(t, h, "status", basePayload(""))
	if rec.Code != http.StatusOK {
		t.Errorf("status handler = %d", rec.Code)
	}
	ctx := context.Background()
	job, err := h.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	ctx := context.Background()
	job, err := h.Store.DequeuePR(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	ctx := context.Background()
	job, err := h.Store.DequeuePR(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	ctx := context.Background()
	job, err := h.Store.DequeuePR(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
		job, err := h.Store.DequeuePR(ctx)
		if err != nil {
			t.Fatal(err)
		}
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
	ctx := context.Background()
	job, err := h.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	pr, err := h.Store.DequeuePR(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := h.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pr != nil || repo != nil {
		t.Error("non-completed check_run should not enqueue anything")
	}
}

func TestPushAndPullRequestDeduplicate(t *testing.T) {
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

	postEvent(t, h, "pull_request", prBody)
	postEvent(t, h, "pull_request", prBody)

	ctx := context.Background()
	var count int
	for {
		job, err := h.Store.DequeuePR(ctx)
		if err != nil {
			t.Fatal(err)
		}
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

	postEvent(t, h, "push", body)
	ctx := context.Background()
	job, err := h.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Fatal("first push should enqueue a job")
	}

	postEvent(t, h, "push", body)
	job2, err := h.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job2 != nil {
		t.Error("second push within rate-limit window should not be dequeue-able immediately")
	}
}

func TestAllowedRepositoriesFiltersRequests(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
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
	ctx := context.Background()
	job, err := h.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job != nil {
		t.Error("blocked repo should not produce a job")
	}
}
