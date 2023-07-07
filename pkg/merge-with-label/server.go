package merge_with_label

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/adjust/rmq/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

const maxBodyBytes = 1024 * 1024 * 16

var _ http.Handler = &Handler{}

type GetLoggerForContext func(ctx context.Context) *zerolog.Logger

type Handler struct {
	GetLoggerForContext GetLoggerForContext
	HTTPClient          *http.Client
	AppID               int64
	PrivateKey          []byte
	Queue               rmq.Queue
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI != "/" && r.RequestURI != "" {
		h.respond(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "https://github.com/apps/merge-with-label", http.StatusTemporaryRedirect)
		// h.respond(w, http.StatusBadRequest, "bad request")
		return
	}

	defer r.Body.Close()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		h.GetLoggerForContext(r.Context()).Error().Err(err).Msg("unable to read body")
		h.respond(w, http.StatusBadRequest, "bad request")
		return
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		h.GetLoggerForContext(r.Context()).Error().Err(err).Msg("unable to decode request")
		h.respond(w, http.StatusBadRequest, "bad request")
		return
	}

	if req.Repository == nil {
		h.GetLoggerForContext(r.Context()).Info().Msg("request didn't contain a repository item")
		h.respond(w, http.StatusOK, "ok")
		return
	}

	githubEvent := r.Header.Get("X-GitHub-Event")
	githubId := r.Header.Get("X-GitHub-Delivery")

	if githubId == "" {
		githubId = uuid.NewString()
	}

	switch githubEvent {
	case "check_run":
		switch req.Action {
		case "completed":
			h.pullRequestLogic(r.Context(), githubId, w, &req)
			return
		}

	case "pull_request":
		switch req.Action {
		case "created", "opened", "labeled", "reopened", "synchronize", "edited":
			h.pullRequestLogic(r.Context(), githubId, w, &req)
			return
		}
	case "push":
		h.pushLogic(r.Context(), githubId, w, &req)
		return
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) pullRequestLogic(ctx context.Context, id string, w http.ResponseWriter, req *Request) {
	if req.PullRequest == nil {
		h.GetLoggerForContext(ctx).Info().Msg("request didn't contain a pull_request item")
		h.respond(w, http.StatusOK, "ok")
		return
	}

	if req.PullRequest.State != "open" {
		h.respond(w, http.StatusOK, "ok")
		return
	}

	msg, err := json.Marshal(QueuePullRequestMessage{
		QueueMessage: QueueMessage{
			ID:   id,
			Kind: PullRequestMessage,
		},
		InstallationID: req.Installation.ID,
		PullRequest:    req.PullRequest,
		Repository:     req.Repository,
	})
	if err != nil {
		h.GetLoggerForContext(ctx).Error().Err(err).Msg("unable to marshal message")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}

	if err := h.Queue.PublishBytes(msg); err != nil {
		h.GetLoggerForContext(ctx).Error().Err(err).Msg("unable to publish message queue")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}
}

func (h *Handler) pushLogic(ctx context.Context, id string, w http.ResponseWriter, req *Request) {
	msg, err := json.Marshal(QueuePushMessage{
		QueueMessage: QueueMessage{
			ID:   id,
			Kind: PushRequestMessage,
		},
		InstallationID: req.Installation.ID,
		Repository:     req.Repository,
	})
	if err != nil {
		h.GetLoggerForContext(ctx).Error().Err(err).Msg("unable to marshal message")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}

	if err := h.Queue.PublishBytes(msg); err != nil {
		h.GetLoggerForContext(ctx).Error().Err(err).Msg("unable to publish message queue")
		h.respond(w, http.StatusInternalServerError, "error")
		return
	}
	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) respond(w http.ResponseWriter, statusCode int, status string) {
	if w == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = fmt.Fprintf(w, `{"status": %q}`, status)
}
