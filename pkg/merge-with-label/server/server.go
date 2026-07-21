package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"golang.org/x/exp/slices"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
)

const maxBodyBytes = 1024 * 1024 * 16

var _ http.Handler = &Handler{}

type GetLoggerForContext func(ctx context.Context) *zerolog.Logger

type Handler struct {
	GetLoggerForContext         GetLoggerForContext
	AllowedRepositories         common.RegexSlice
	AllowOnlyPublicRepositories bool

	Store             *pgqueue.Store
	RateLimitInterval time.Duration
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI != "/" && r.RequestURI != "" {
		h.respond(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "https://github.com/Eun/merge-with-label", http.StatusTemporaryRedirect)
		return
	}

	defer r.Body.Close()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		h.GetLoggerForContext(r.Context()).Error().Err(err).Msg("unable to read body")
		h.respond(w, http.StatusBadRequest, "bad request")
		return
	}

	githubEvent := r.Header.Get("X-GitHub-Event")
	githubID := r.Header.Get("X-GitHub-Delivery")
	if githubID == "" {
		githubID = uuid.NewString()
	}

	logger := h.GetLoggerForContext(r.Context()).With().Str("event", githubEvent).Logger()
	if logger.GetLevel() == zerolog.TraceLevel {
		logger.Trace().Str("body", string(body)).Msg("got event")
	} else {
		logger.Debug().Msg("got event")
	}

	baseRequest := h.unmarshalAndValidateRequest(&logger, body, w)
	if baseRequest == nil {
		return
	}

	switch githubEvent {
	case "check_run":
		h.handleCheckRun(r.Context(), &logger, body, w)
	case "pull_request":
		h.handlePullRequest(r.Context(), &logger, body, w)
	case "pull_request_review":
		h.handlePullRequestReview(r.Context(), &logger, body, w)
	case "push":
		h.handlePush(r.Context(), &logger, body, w, baseRequest)
	case "status":
		h.handleRepoEvent(r.Context(), &logger, w, baseRequest, "status")
	default:
		h.respond(w, http.StatusOK, "ok")
	}
}

func (h *Handler) unmarshalAndValidateRequest(rootLogger *zerolog.Logger, body []byte, w http.ResponseWriter) *BaseRequest {
	var req BaseRequest
	if err := json.Unmarshal(body, &req); err != nil {
		rootLogger.Error().Err(err).Msg("unable to decode request")
		h.respond(w, http.StatusBadRequest, "bad request")
		return nil
	}
	if !req.IsValid(rootLogger) {
		h.respond(w, http.StatusOK, "ok")
		return nil
	}
	if h.AllowOnlyPublicRepositories && req.Repository.Private {
		rootLogger.Warn().Str("repo", req.Repository.FullName).Msg("repository is not allowed (it is private)")
		h.respond(w, http.StatusOK, "ok")
		return nil
	}
	if h.AllowedRepositories.ContainsOneOf(req.Repository.FullName) == "" {
		rootLogger.Warn().Str("repo", req.Repository.FullName).Msg("repository is not allowed")
		h.respond(w, http.StatusOK, "ok")
		return nil
	}
	return &req
}

// handleCheckRun handles check_run events. A check_run may reference zero,
// one, or many PRs. If PR numbers are present we enqueue PR-level jobs; if
// there are none we fall back to a repo-level job so the worker can fan out.
func (h *Handler) handleCheckRun(ctx context.Context, logger *zerolog.Logger, body []byte, w http.ResponseWriter) {
	var req struct {
		BaseRequest
		CheckRun struct {
			PullRequests []struct {
				Number int64 `json:"number"`
			} `json:"pull_requests"`
			CheckSuite struct {
				PullRequests []struct {
					Number int64 `json:"number"`
				} `json:"pull_requests"`
			} `json:"check_suite"`
		} `json:"check_run"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		logger.Error().Err(err).Msg("unable to decode request")
		h.respond(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.Action != "completed" {
		logger.Debug().Msg("action is not completed")
		h.respond(w, http.StatusOK, "ok")
		return
	}

	repo := h.repoFrom(&req.BaseRequest)

	// Collect unique PR numbers from both check_run and check_suite.
	seen := make(map[int64]struct{})
	for _, pr := range append(req.CheckRun.PullRequests, req.CheckRun.CheckSuite.PullRequests...) {
		if pr.Number != 0 {
			seen[pr.Number] = struct{}{}
		}
	}

	if len(seen) == 0 {
		// No PR numbers in the payload — treat as a repo-level event.
		h.enqueueRepoOrError(ctx, logger, w, repo, req.Installation.ID)
		return
	}

	for number := range seen {
		if err := common.EnqueuePR(ctx, logger, h.Store, h.RateLimitInterval, &common.QueuePRMessage{
			BaseMessage: common.BaseMessage{InstallationID: req.Installation.ID, Repository: *repo},
			PullRequest: common.PullRequest{Number: number},
		}); err != nil {
			logger.Error().Err(err).Msg("unable to queue check_run PR message")
			h.respond(w, http.StatusInternalServerError, "error")
			return
		}
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) handlePullRequest(ctx context.Context, logger *zerolog.Logger, body []byte, w http.ResponseWriter) {
	var req struct {
		BaseRequest
		PullRequest struct {
			Number int64  `json:"number"`
			State  string `json:"state"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		logger.Error().Err(err).Msg("unable to decode request")
		h.respond(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.PullRequest.Number == 0 {
		logger.Debug().Msg("no pull_request.number present in request")
		h.respond(w, http.StatusOK, "ok")
		return
	}
	if req.PullRequest.State != "open" {
		logger.Debug().Msg("pull_request.state is not `open'")
		h.respond(w, http.StatusOK, "ok")
		return
	}

	handleActions := []string{"created", "opened", "labeled", "reopened", "synchronize", "edited"}
	if slices.Index(handleActions, req.Action) == -1 {
		logger.Debug().Msgf("action is not one of %s", strings.Join(handleActions, ", "))
		h.respond(w, http.StatusOK, "ok")
		return
	}

	repo := h.repoFrom(&req.BaseRequest)
	if err := common.EnqueuePR(ctx, logger, h.Store, h.RateLimitInterval, &common.QueuePRMessage{
		BaseMessage: common.BaseMessage{InstallationID: req.Installation.ID, Repository: *repo},
		PullRequest: common.PullRequest{Number: req.PullRequest.Number},
	}); err != nil {
		logger.Error().Err(err).Msg("unable to queue pull_request message")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) handlePullRequestReview(ctx context.Context, logger *zerolog.Logger, body []byte, w http.ResponseWriter) {
	var req struct {
		BaseRequest
		PullRequest struct {
			Number int64  `json:"number"`
			State  string `json:"state"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		logger.Error().Err(err).Msg("unable to decode request")
		h.respond(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.PullRequest.Number == 0 {
		logger.Debug().Msg("no pull_request.number present in request")
		h.respond(w, http.StatusOK, "ok")
		return
	}
	if req.PullRequest.State != "open" {
		logger.Debug().Msg("pull_request.state is not `open'")
		h.respond(w, http.StatusOK, "ok")
		return
	}
	if req.Action != "submitted" {
		logger.Debug().Msg("action is not submitted")
		h.respond(w, http.StatusOK, "ok")
		return
	}

	repo := h.repoFrom(&req.BaseRequest)
	if err := common.EnqueuePR(ctx, logger, h.Store, h.RateLimitInterval, &common.QueuePRMessage{
		BaseMessage: common.BaseMessage{InstallationID: req.Installation.ID, Repository: *repo},
		PullRequest: common.PullRequest{Number: req.PullRequest.Number},
	}); err != nil {
		logger.Error().Err(err).Msg("unable to queue pull_request message")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) handlePush(ctx context.Context, logger *zerolog.Logger, body []byte, w http.ResponseWriter, base *BaseRequest) {
	var req struct {
		BaseRequest
		Deleted bool   `json:"deleted"`
		Ref     string `json:"ref"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		logger.Error().Err(err).Msg("unable to decode request")
		h.respond(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.Deleted || req.Ref != "refs/heads/"+req.Repository.DefaultBranch {
		h.respond(w, http.StatusOK, "ok")
		return
	}
	h.enqueueRepoOrError(ctx, logger, w, h.repoFrom(base), base.Installation.ID)
}

// handleRepoEvent is a shared handler for events that trigger repo-level work
// (currently: status). All callers produce identical queue rows because the
// dedup key is the same.
func (h *Handler) handleRepoEvent(ctx context.Context, logger *zerolog.Logger, w http.ResponseWriter, base *BaseRequest, _ string) {
	h.enqueueRepoOrError(ctx, logger, w, h.repoFrom(base), base.Installation.ID)
}

func (h *Handler) enqueueRepoOrError(ctx context.Context, logger *zerolog.Logger, w http.ResponseWriter, repo *common.Repository, installationID int64) {
	if err := common.EnqueueRepo(ctx, logger, h.Store, h.RateLimitInterval, &common.QueueRepoMessage{
		BaseMessage: common.BaseMessage{InstallationID: installationID, Repository: *repo},
	}); err != nil {
		logger.Error().Err(err).Msg("unable to queue repo message")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) repoFrom(req *BaseRequest) *common.Repository {
	return &common.Repository{
		NodeID:    req.Repository.NodeID,
		FullName:  req.Repository.FullName,
		Name:      req.Repository.Name,
		OwnerName: req.Repository.Owner.Login,
		Private:   req.Repository.Private,
	}
}

func (h *Handler) respond(w http.ResponseWriter, statusCode int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = fmt.Fprintf(w, `{"status": %q}`, status)
}
