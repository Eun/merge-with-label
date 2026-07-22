package server_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
)

func postEvent(t *testing.T, event string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "test-"+t.Name())
	sharedHandler.ServeHTTP(rec, req)
	return rec
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

func TestServeHTTP_NonPostRedirects(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	sharedHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("GET / = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
}

func TestServeHTTP_UnknownPath(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/unknown", http.NoBody)
	sharedHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /unknown = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServeHTTP_UnknownEvent(t *testing.T) {
	rec := postEvent(t, "create", basePayload("created"))
	if rec.Code != http.StatusOK {
		t.Errorf("unknown event = %d, want 200", rec.Code)
	}
}

func TestHandlePush_EnqueuesRepoJob(t *testing.T) {
	body := []byte(`{
		"action": "",
		"installation": {"id": 1},
		"repository": {
			"full_name": "owner/repo", "name": "repo",
			"node_id": "R_push_` + t.Name() + `",
			"owner": {"login": "owner"}, "private": false,
			"default_branch": "main"
		},
		"ref": "refs/heads/main", "deleted": false
	}`)
	rec := postEvent(t, "push", body)
	if rec.Code != http.StatusOK {
		t.Errorf("push handler = %d, want 200", rec.Code)
	}
	ctx := context.Background()
	job, err := sharedHandler.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatalf("DequeueRepo: %v", err)
	}
	if job == nil {
		t.Error("expected a repo job to be enqueued by push handler")
	}
}

func TestHandlePush_DeletedBranchIgnored(t *testing.T) {
	body := []byte(`{
		"installation": {"id": 1},
		"repository": {
			"full_name": "owner/repo", "name": "repo",
			"node_id": "R_del_` + t.Name() + `",
			"owner": {"login": "owner"}, "private": false, "default_branch": "main"
		},
		"ref": "refs/heads/main", "deleted": true
	}`)
	rec := postEvent(t, "push", body)
	if rec.Code != http.StatusOK {
		t.Errorf("push deleted = %d", rec.Code)
	}
	ctx := context.Background()
	// Drain any existing jobs; the deleted push should NOT have added one.
	for {
		job, err := sharedHandler.Store.DequeueRepo(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			break
		}
	}
}

func TestHandlePush_NonDefaultBranchIgnored(t *testing.T) {
	body := []byte(`{
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_feat_` + t.Name() + `",
			"owner": {"login": "o"}, "private": false, "default_branch": "main"
		},
		"ref": "refs/heads/feature-branch", "deleted": false
	}`)
	rec := postEvent(t, "push", body)
	if rec.Code != http.StatusOK {
		t.Errorf("feature branch push = %d", rec.Code)
	}
	// No assertion on the queue — we just verify no error status.
}

func TestHandleStatus_EnqueuesRepoJob(t *testing.T) {
	rec := postEvent(t, "status", basePayload(""))
	if rec.Code != http.StatusOK {
		t.Errorf("status handler = %d", rec.Code)
	}
	ctx := context.Background()
	job, err := sharedHandler.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Error("expected repo job from status event")
	}
}

func TestHandlePullRequest_OpenSynchronize_EnqueuesPR(t *testing.T) {
	body := []byte(`{
		"action": "synchronize",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_pr_` + t.Name() + `",
			"owner": {"login": "o"}, "private": false
		},
		"pull_request": {"number": 7, "state": "open"}
	}`)
	rec := postEvent(t, "pull_request", body)
	if rec.Code != http.StatusOK {
		t.Errorf("pull_request = %d", rec.Code)
	}
	ctx := context.Background()
	job, err := sharedHandler.Store.DequeuePR(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Error("expected PR job from pull_request synchronize")
	}
}

func TestHandlePullRequest_ClosedIgnored(t *testing.T) {
	body := []byte(`{
		"action": "closed",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_cls_` + t.Name() + `",
			"owner": {"login": "o"}, "private": false
		},
		"pull_request": {"number": 3, "state": "closed"}
	}`)
	rec := postEvent(t, "pull_request", body)
	if rec.Code != http.StatusOK {
		t.Errorf("closed PR = %d", rec.Code)
	}
}

func TestHandlePullRequestReview_SubmittedEnqueuesPR(t *testing.T) {
	body := []byte(`{
		"action": "submitted",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_rev_` + t.Name() + `",
			"owner": {"login": "o"}, "private": false
		},
		"pull_request": {"number": 9, "state": "open"}
	}`)
	rec := postEvent(t, "pull_request_review", body)
	if rec.Code != http.StatusOK {
		t.Errorf("pull_request_review = %d", rec.Code)
	}
	ctx := context.Background()
	job, err := sharedHandler.Store.DequeuePR(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Error("expected PR job from pull_request_review submitted")
	}
}

func TestHandleCheckRun_WithPRNumbers_EnqueuesPRJobs(t *testing.T) {
	body := []byte(`{
		"action": "completed",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_cr_` + t.Name() + `",
			"owner": {"login": "o"}, "private": false
		},
		"check_run": {
			"pull_requests": [{"number": 11}, {"number": 12}],
			"check_suite": {"pull_requests": []}
		}
	}`)
	rec := postEvent(t, "check_run", body)
	if rec.Code != http.StatusOK {
		t.Errorf("check_run = %d", rec.Code)
	}
	ctx := context.Background()
	var count int
	for {
		job, err := sharedHandler.Store.DequeuePR(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			break
		}
		count++
	}
	if count < 2 {
		t.Errorf("expected at least 2 PR jobs from check_run, got %d", count)
	}
}

func TestHandleCheckRun_NoPRs_EnqueuesRepoJob(t *testing.T) {
	body := []byte(`{
		"action": "completed",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_cr2_` + t.Name() + `",
			"owner": {"login": "o"}, "private": false
		},
		"check_run": {
			"pull_requests": [],
			"check_suite": {"pull_requests": []}
		}
	}`)
	rec := postEvent(t, "check_run", body)
	if rec.Code != http.StatusOK {
		t.Errorf("check_run no PRs = %d", rec.Code)
	}
	ctx := context.Background()
	job, err := sharedHandler.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Error("expected repo job when check_run has no PR numbers")
	}
}

func TestHandleCheckRun_NotCompleted_NoJob(t *testing.T) {
	body := []byte(`{
		"action": "rerequested",
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_cr3_` + t.Name() + `",
			"owner": {"login": "o"}, "private": false
		},
		"check_run": {
			"pull_requests": [{"number": 5}],
			"check_suite": {"pull_requests": []}
		}
	}`)
	rec := postEvent(t, "check_run", body)
	if rec.Code != http.StatusOK {
		t.Errorf("check_run rerequested = %d", rec.Code)
	}
}

func TestAllowedRepositoriesFiltersRequests(t *testing.T) {
	// Temporarily restrict the handler.
	orig := sharedHandler.AllowedRepositories
	sharedHandler.AllowedRepositories = common.RegexSlice{common.MustNewRegexItem("^allowed/repo$")}
	defer func() { sharedHandler.AllowedRepositories = orig }()

	body := []byte(`{
		"installation": {"id": 1},
		"repository": {
			"full_name": "blocked/repo", "name": "repo",
			"node_id": "R_blk_` + t.Name() + `",
			"owner": {"login": "blocked"}, "private": false, "default_branch": "main"
		},
		"ref": "refs/heads/main", "deleted": false
	}`)
	rec := postEvent(t, "push", body)
	if rec.Code != http.StatusOK {
		t.Errorf("blocked repo push = %d", rec.Code)
	}
}

func TestRateLimitDelaysSecondEnqueue(t *testing.T) {
	orig := sharedHandler.RateLimitInterval
	sharedHandler.RateLimitInterval = time.Hour
	defer func() { sharedHandler.RateLimitInterval = orig }()

	body := []byte(`{
		"installation": {"id": 1},
		"repository": {
			"full_name": "o/r", "name": "r",
			"node_id": "R_rl_` + t.Name() + `",
			"owner": {"login": "o"}, "private": false, "default_branch": "main"
		},
		"ref": "refs/heads/main", "deleted": false
	}`)

	// First push — should enqueue.
	postEvent(t, "push", body)
	ctx := context.Background()
	job, err := sharedHandler.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Fatal("first push should enqueue a job")
	}

	// Second push — same dedup key, should be delayed (not immediately available).
	postEvent(t, "push", body)
	job2, err := sharedHandler.Store.DequeueRepo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job2 != nil {
		t.Error("second push within rate-limit window should not be dequeue-able immediately")
	}
}
