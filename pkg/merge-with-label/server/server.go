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
	"github.com/nats-io/nats.go"
	"github.com/rs/zerolog"
	"golang.org/x/exp/slices"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
)

const maxBodyBytes = 1024 * 1024 * 16

var _ http.Handler = &Handler{}

type GetLoggerForContext func(ctx context.Context) *zerolog.Logger

type Handler struct {
	GetLoggerForContext         GetLoggerForContext
	AllowedRepositories         common.RegexSlice
	AllowOnlyPublicRepositories bool

	JetStreamContext   nats.JetStreamContext
	PushSubject        string
	StatusSubject      string
	PullRequestSubject string

	RateLimitKV       nats.KeyValue
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
	switch githubEvent {
	case "check_run":
		h.handleCheckRun(&logger, githubID, body, w)
		return
	case "pull_request":
		h.handlePullRequest(&logger, githubID, body, w)
		return
	case "pull_request_review":
		h.handlePullRequestReview(&logger, githubID, body, w)
		return
	case "push":
		h.handlePush(&logger, githubID, body, w)
		return
	case "status":
		h.handleStatus(&logger, githubID, body, w)
		return
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) handleCheckRun(logger *zerolog.Logger, eventID string, body []byte, w http.ResponseWriter) {
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

	if !req.BaseRequest.IsValid(logger) {
		h.respond(w, http.StatusOK, "ok")
		return
	}

	if req.Action != "completed" {
		logger.Debug().Msg("action is not completed")
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

	pullRequests := append(req.CheckRun.PullRequests, req.CheckRun.CheckSuite.PullRequests...)
	for i := range pullRequests {
		if pullRequests[i].Number == 0 {
			logger.Debug().Msgf("no pull_requests.%d.number present in request", i)
			continue
		}

		err := h.queuePullRequestMessage(
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
				Number: pullRequests[i].Number,
			})
		if err != nil {
			logger.Error().Err(err).Msg("unable to queue message")
			h.respond(w, http.StatusInternalServerError, "error")
			return
		}
	}
	h.respond(w, http.StatusOK, "ok")
}
func (h *Handler) handlePullRequest(logger *zerolog.Logger, eventID string, body []byte, w http.ResponseWriter) {
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

	if !req.BaseRequest.IsValid(logger) {
		h.respond(w, http.StatusOK, "ok")
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

func (h *Handler) handlePullRequestReview(logger *zerolog.Logger, eventID string, body []byte, w http.ResponseWriter) {
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

	if !req.BaseRequest.IsValid(logger) {
		h.respond(w, http.StatusOK, "ok")
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

func (h *Handler) handlePush(logger *zerolog.Logger, eventID string, body []byte, w http.ResponseWriter) {
	var req struct {
		BaseRequest
	}

	if err := json.Unmarshal(body, &req); err != nil {
		logger.Error().Err(err).Msg("unable to decode request")
		h.respond(w, http.StatusBadRequest, "bad request")
		return
	}

	if !req.BaseRequest.IsValid(logger) {
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

	err := common.QueueMessage(
		logger,
		h.JetStreamContext,
		h.RateLimitKV,
		h.RateLimitInterval,
		h.PushSubject+"."+eventID,
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

func (h *Handler) handleStatus(logger *zerolog.Logger, eventID string, body []byte, w http.ResponseWriter) {
	var req struct {
		BaseRequest
	}
	if err := json.Unmarshal(body, &req); err != nil {
		logger.Error().Err(err).Msg("unable to decode request")
		h.respond(w, http.StatusBadRequest, "bad request")
		return
	}

	if !req.BaseRequest.IsValid(logger) {
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

	err := common.QueueMessage(
		logger,
		h.JetStreamContext,
		h.RateLimitKV,
		h.RateLimitInterval,
		h.StatusSubject+"."+eventID,
		fmt.Sprintf("status.%d.%s", req.Installation.ID, req.Repository.NodeID),
		&common.QueueStatusMessage{
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
		logger.Error().Err(err).Msg("unable to queue status message")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) queuePullRequestMessage(
	logger *zerolog.Logger,
	eventID string,
	repository *common.Repository,
	installationID int64,
	pullRequest *common.PullRequest,
) error {
	return common.QueueMessage(
		logger,
		h.JetStreamContext,
		h.RateLimitKV,
		h.RateLimitInterval,
		h.PullRequestSubject+"."+eventID,
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
