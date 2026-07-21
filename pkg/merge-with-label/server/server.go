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
		h.handleCheckRun(r.Context(), &logger, githubID, body, w)
		return
	case "pull_request":
		h.handlePullRequest(r.Context(), &logger, githubID, body, w)
		return
	case "pull_request_review":
		h.handlePullRequestReview(r.Context(), &logger, githubID, body, w)
		return
	case "push":
		h.handlePush(r.Context(), &logger, githubID, body, w)
		return
	case "status":
		h.handleStatus(r.Context(), &logger, githubID, baseRequest, w)
		return
	}
	h.respond(w, http.StatusOK, "ok")
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

func (h *Handler) handleCheckRun(ctx context.Context, logger *zerolog.Logger, eventID string, body []byte, w http.ResponseWriter) {
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

	// remove duplicates
	pullRequests := make(map[int64]struct{})
	for _, request := range append(req.CheckRun.PullRequests, req.CheckRun.CheckSuite.PullRequests...) {
		if request.Number == 0 {
			continue
		}
		pullRequests[request.Number] = struct{}{}
	}

	for number := range pullRequests {
		err := h.queuePullRequestMessage(
			ctx,
			logger,
			eventID,
			&common.Repository{
				NodeID:    req.Repository.NodeID,
				FullName:  req.Repository.FullName,
				Name:      req.Repository.Name,
				OwnerName: req.Repository.Owner.Login,
				Private:   req.Repository.Private,
			},
			req.Installation.ID,
			&common.PullRequest{
				Number: number,
			})
		if err != nil {
			logger.Error().Err(err).Msg("unable to queue message")
			h.respond(w, http.StatusInternalServerError, "error")
			return
		}
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) handlePullRequest(ctx context.Context, logger *zerolog.Logger, eventID string, body []byte, w http.ResponseWriter) {
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
	if req.PullRequest.State == "" {
		logger.Debug().Msg("no pull_request.state present in request")
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

	if h.AllowOnlyPublicRepositories && req.Repository.Private {
		logger.Warn().Str("repo", req.Repository.FullName).Msg("repository is not allowed (it is private)")
		h.respond(w, http.StatusOK, "ok")
		return
	}

	if h.AllowedRepositories.ContainsOneOf(req.Repository.FullName) == "" {
		logger.Warn().Str("repo", req.Repository.FullName).Msg("repository is not allowed")
		h.respond(w, http.StatusOK, "ok")
		return
	}

	err := h.queuePullRequestMessage(
		ctx,
		logger,
		eventID,
		&common.Repository{
			NodeID:    req.Repository.NodeID,
			FullName:  req.Repository.FullName,
			Name:      req.Repository.Name,
			OwnerName: req.Repository.Owner.Login,
			Private:   req.Repository.Private,
		},
		req.Installation.ID,
		&common.PullRequest{
			Number: req.PullRequest.Number,
		})
	if err != nil {
		logger.Error().Err(err).Msg("unable to queue pull_request message")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) handlePullRequestReview(ctx context.Context, logger *zerolog.Logger, eventID string, body []byte, w http.ResponseWriter) {
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
	if req.PullRequest.State == "" {
		logger.Debug().Msg("no pull_request.state present in request")
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

	if h.AllowOnlyPublicRepositories && req.Repository.Private {
		logger.Warn().Str("repo", req.Repository.FullName).Msg("repository is not allowed (it is private)")
		h.respond(w, http.StatusOK, "ok")
		return
	}

	if h.AllowedRepositories.ContainsOneOf(req.Repository.FullName) == "" {
		logger.Warn().Str("repo", req.Repository.FullName).Msg("repository is not allowed")
		h.respond(w, http.StatusOK, "ok")
		return
	}

	err := h.queuePullRequestMessage(
		ctx,
		logger,
		eventID,
		&common.Repository{
			NodeID:    req.Repository.NodeID,
			FullName:  req.Repository.FullName,
			Name:      req.Repository.Name,
			OwnerName: req.Repository.Owner.Login,
			Private:   req.Repository.Private,
		},
		req.Installation.ID,
		&common.PullRequest{
			Number: req.PullRequest.Number,
		})
	if err != nil {
		logger.Error().Err(err).Msg("unable to queue pull_request message")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) handlePush(ctx context.Context, logger *zerolog.Logger, eventID string, body []byte, w http.ResponseWriter) {
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

	if req.Deleted {
		h.respond(w, http.StatusOK, "ok")
		return
	}

	if req.Ref != "refs/heads/"+req.Repository.DefaultBranch {
		h.respond(w, http.StatusOK, "ok")
		return
	}

	err := common.QueueMessage(
		ctx,
		logger,
		h.Store,
		h.RateLimitInterval,
		common.MsgTypePush,
		fmt.Sprintf("push.%d.%s", req.Installation.ID, req.Repository.NodeID),
		&common.QueuePushMessage{
			BaseMessage: common.BaseMessage{
				InstallationID: req.Installation.ID,
				Repository: common.Repository{
					NodeID:    req.Repository.NodeID,
					FullName:  req.Repository.FullName,
					Name:      req.Repository.Name,
					OwnerName: req.Repository.Owner.Login,
					Private:   req.Repository.Private,
				},
			},
		})
	if err != nil {
		logger.Error().Err(err).Msg("unable to queue push message")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) handleStatus(ctx context.Context, logger *zerolog.Logger, eventID string, baseRequest *BaseRequest, w http.ResponseWriter) {
	err := common.QueueMessage(
		ctx,
		logger,
		h.Store,
		h.RateLimitInterval,
		common.MsgTypeStatus,
		fmt.Sprintf("status.%d.%s", baseRequest.Installation.ID, baseRequest.Repository.NodeID),
		&common.QueueStatusMessage{
			BaseMessage: common.BaseMessage{
				InstallationID: baseRequest.Installation.ID,
				Repository: common.Repository{
					NodeID:    baseRequest.Repository.NodeID,
					FullName:  baseRequest.Repository.FullName,
					Name:      baseRequest.Repository.Name,
					OwnerName: baseRequest.Repository.Owner.Login,
					Private:   baseRequest.Repository.Private,
				},
			},
		})
	if err != nil {
		logger.Error().Err(err).Msg("unable to queue status message")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) queuePullRequestMessage(
	ctx context.Context,
	logger *zerolog.Logger,
	eventID string,
	repository *common.Repository,
	installationID int64,
	pullRequest *common.PullRequest,
) error {
	return common.QueueMessage(
		ctx,
		logger,
		h.Store,
		h.RateLimitInterval,
		common.MsgTypePullRequest,
		fmt.Sprintf("pull_request.%d.%s.%d", installationID, repository.NodeID, pullRequest.Number),
		&common.QueuePullRequestMessage{
			BaseMessage: common.BaseMessage{
				InstallationID: installationID,
				Repository:     *repository,
			},
			PullRequest: *pullRequest,
		})
}

func (h *Handler) respond(w http.ResponseWriter, statusCode int, status string) {
	if w == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = fmt.Fprintf(w, `{"status": %q}`, status)
}
