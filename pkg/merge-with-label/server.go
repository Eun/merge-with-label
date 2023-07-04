package merge_with_label

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/internal"
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
}

func (h *Handler) respond(w http.ResponseWriter, statusCode int, status string) {
	if w == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = fmt.Fprintf(w, `{"status": %q}`, status)
}

type LogicToExecute int

const (
	NoLogic LogicToExecute = iota
	RunPullRequestLogic
	RunPushLogic
)

func (h *Handler) shouldExecuteLogic(req *internal.Request, r *http.Request) LogicToExecute {
	githubEvent := r.Header.Get("X-GitHub-Event")

	switch githubEvent {
	case "check_run":
		switch req.Action {
		case "completed":
			return RunPullRequestLogic
		}

	case "pull_request":
		switch req.Action {
		case "created", "opened", "labeled", "reopened", "synchronize", "edited":
			return RunPullRequestLogic
		}
	case "push":
		return RunPushLogic
	}
	return NoLogic
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

	var req internal.Request
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

	logicToExecute := h.shouldExecuteLogic(&req, r)
	if logicToExecute == NoLogic {

	}

	switch logicToExecute {
	case NoLogic:
		h.respond(w, http.StatusOK, "ok")
		return
	case RunPullRequestLogic:
		h.pullRequestLogic(r.Context(), w, &req)
		return
	case RunPushLogic:
		h.pushLogic(r.Context(), w, &req)
		return
	}

}

func (h *Handler) pullRequestLogic(ctx context.Context, w http.ResponseWriter, req *internal.Request) {
	if req.PullRequest == nil {
		h.GetLoggerForContext(ctx).Info().Msg("request didn't contain a pull_request item")
		h.respond(w, http.StatusOK, "ok")
		return
	}

	if req.PullRequest.State != "open" {
		h.respond(w, http.StatusOK, "ok")
		return
	}

	for _, label := range req.PullRequest.Labels {
		if label.Name == "auto-merge" || label.Name == "force-merge" {
			accessToken, err := GetAccessToken(ctx, h.HTTPClient, h.AppID, h.PrivateKey, req)
			if err != nil {
				h.GetLoggerForContext(ctx).Error().Err(err).Msg("error getting access token")
				h.respond(w, http.StatusOK, "ok")
				return
			}

			var commitTime time.Time
			var checksStatus = "SUCCESS"
			if label.Name == "auto-merge" {
				checksStatus, commitTime, err = GetCheckSuiteStatusForPullRequest(ctx, h.HTTPClient, accessToken, req.Repository, req.PullRequest.Number)
				if err != nil {
					h.GetLoggerForContext(ctx).Error().Err(err).Msg("error during getting checks")
					h.respond(w, http.StatusOK, "ok")
					return
				}
			}

			if checksStatus == "SUCCESS" {
				if time.Now().Sub(commitTime) < time.Second*10 {
					// it's a bit too early. block merging
					h.respond(w, http.StatusOK, "ok")
					return
				}

				if err := MergePullRequest(ctx, h.HTTPClient, accessToken, req, 0); err != nil {
					h.GetLoggerForContext(ctx).Error().Err(err).Msg("error during merge")
				}
			}

			h.respond(w, http.StatusOK, "ok")
			return
		}
	}

	h.respond(w, http.StatusOK, "ok")
}

func (h *Handler) pushLogic(ctx context.Context, w http.ResponseWriter, req *internal.Request) {
	accessToken, err := GetAccessToken(ctx, h.HTTPClient, h.AppID, h.PrivateKey, req)
	if err != nil {
		h.GetLoggerForContext(ctx).Error().Err(err).Msg("error getting access token")
		h.respond(w, http.StatusOK, "ok")
		return
	}
	pullRequests, err := GetPullRequestsThatNeedToBeUpdated(ctx, h.HTTPClient, accessToken, req)
	if err != nil {
		h.GetLoggerForContext(ctx).Error().Err(err).Msg("error getting pull requests")
		h.respond(w, http.StatusOK, "ok")
		return
	}
	if len(pullRequests) == 0 {
		h.respond(w, http.StatusOK, "ok")
		return
	}

	for _, number := range pullRequests {
		if err := UpdatePullRequest(ctx, h.HTTPClient, accessToken, req.Repository, number, 0); err != nil {
			h.GetLoggerForContext(ctx).Error().Err(err).Msg("error updating pull request")
		}
	}
	h.respond(w, http.StatusOK, "ok")
}
