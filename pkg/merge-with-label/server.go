package merge_with_label

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/pkg/errors"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/internal"
	"golang.org/x/exp/slog"
)

const maxBodyBytes = 1024 * 1024

var _ http.Handler = &Handler{}

type GetLoggerForContext func(ctx context.Context) *slog.Logger

type Handler struct {
	GetLoggerForContext GetLoggerForContext
	HTTPClient          *http.Client
	AppID               int64
	PrivateKey          []byte
}

func (h *Handler) respond(w http.ResponseWriter, statusCode int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = fmt.Fprintf(w, `{"status": %q}`, status)
}

func (h *Handler) getAccessToken(ctx context.Context, req *internal.Request) (*internal.AccessToken, error) {
	var body bytes.Buffer
	err := json.NewEncoder(&body).Encode(struct {
		Repository  string `json:"repository"`
		Permissions struct {
			PullRequests string `json:"pull_requests"`
			Contents     string `json:"contents"`
			Workflows    string `json:"workflows"`
		}
	}{
		Repository: req.Repository.FullName,
		Permissions: struct {
			PullRequests string `json:"pull_requests"`
			Contents     string `json:"contents"`
			Workflows    string `json:"workflows"`
		}{
			PullRequests: "write",
			Contents:     "write",
			Workflows:    "write",
		},
	})
	if err != nil {
		return nil, errors.Wrap(err, "unable to encode request")
	}

	r, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", req.Installation.ID), &body)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create request")
	}

	iss := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	exp := iss.Add(2 * time.Minute)
	claims := &jwt.StandardClaims{
		IssuedAt:  iss.Unix(),
		ExpiresAt: exp.Unix(),
		Issuer:    strconv.FormatInt(h.AppID, 10),
	}
	bearer := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

	key, err := jwt.ParseRSAPrivateKeyFromPEM(h.PrivateKey)
	if err != nil {
		return nil, errors.Wrap(err, "could not parse private key")
	}

	ss, err := bearer.SignedString(key)
	if err != nil {
		return nil, errors.Wrap(err, "could not sign jwt")
	}

	r.Header.Set("Authorization", "Bearer "+ss)
	r.Header.Add("Accept", "application/vnd.github+json")
	r.Header.Add("X-GitHub-Api-Version", "2022-11-28")

	resp, err := h.HTTPClient.Do(r)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer resp.Body.Close()

	body.Reset()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if resp.StatusCode != http.StatusCreated {
		h.GetLoggerForContext(ctx).ErrorCtx(
			ctx,
			"error when getting access token: expected 201 status code",
			"code", resp.StatusCode,
			"body", string(buf),
		)
		return nil, errors.New("error when getting access token: expected 201 status code")
	}

	var token internal.AccessToken
	if err := json.Unmarshal(buf, &token); err != nil {
		h.GetLoggerForContext(r.Context()).ErrorCtx(r.Context(), "unable to decode access token", "err", err, "body", string(buf))
		return nil, errors.WithStack(err)
	}

	return &token, nil
}

func (h *Handler) areChecksGreen(ctx context.Context, accessToken *internal.AccessToken, req *internal.Request) (bool, error) {
	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/commits/%s/check-runs", req.Repository.FullName, req.PullRequest.Head.SHA),
		http.NoBody,
	)
	if err != nil {
		return false, errors.WithStack(err)
	}

	r.Header.Add("Accept", "application/vnd.github+json")
	r.Header.Add("X-GitHub-Api-Version", "2022-11-28")
	r.Header.Add("Authorization", "Bearer "+accessToken.Token)

	resp, err := h.HTTPClient.Do(r)
	if err != nil {
		return false, errors.WithStack(err)
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return false, errors.WithStack(err)
	}

	if resp.StatusCode != http.StatusOK {
		h.GetLoggerForContext(ctx).ErrorCtx(
			ctx,
			"error when checking runs: expected 200 status code",
			"code", resp.StatusCode,
			"body", string(buf),
		)
		return false, errors.New("error when performing merge: expected 200 status code")
	}

	var checkRunsResponse internal.CheckRunsResponse
	if err := json.Unmarshal(buf, &checkRunsResponse); err != nil {
		h.GetLoggerForContext(ctx).ErrorCtx(
			ctx,
			"error when decoding check runs response",
			"body", string(buf),
		)
		return false, errors.WithStack(err)
	}

	for _, run := range checkRunsResponse.CheckRuns {
		if run.Status != "completed" {
			return false, nil
		}
		switch run.Conclusion {
		case "neutral", "success", "skipped":
		default:
			return false, nil
		}
	}

	return true, nil
}

func (h *Handler) mergePullRequest(ctx context.Context, accessToken *internal.AccessToken, req *internal.Request, tryCounter int) error {
	var body bytes.Buffer

	if err := json.NewEncoder(&body).Encode(struct {
		CommitTitle   string `json:"commit_title"`
		CommitMessage string `json:"commit_message"`
		MergeMethod   string `json:"merge_method"`
	}{
		CommitTitle:   req.PullRequest.Title,
		CommitMessage: "",
		MergeMethod:   "squash",
	}); err != nil {
		return errors.WithStack(err)
	}

	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d/merge", req.Repository.FullName, req.PullRequest.Number),
		&body,
	)
	if err != nil {
		return errors.WithStack(err)
	}

	r.Header.Add("Accept", "application/vnd.github+json")
	r.Header.Add("X-GitHub-Api-Version", "2022-11-28")
	r.Header.Add("Authorization", "Bearer "+accessToken.Token)

	resp, err := h.HTTPClient.Do(r)
	if err != nil {
		return errors.WithStack(err)
	}
	defer resp.Body.Close()

	body.Reset()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return errors.WithStack(err)
	}

	if resp.StatusCode == http.StatusMethodNotAllowed {
		var mergeResponse struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(buf, &mergeResponse); err != nil {
			h.GetLoggerForContext(ctx).ErrorCtx(
				ctx,
				"error when decoding merge response",
				"body", string(buf),
			)
			return errors.WithStack(err)
		}
		if mergeResponse.Message == "Base branch was modified. Review and try the merge again." ||
			mergeResponse.Message == "Pull Request is not mergeable" {
			if tryCounter < 3 {
				return h.mergePullRequest(ctx, accessToken, req, tryCounter+1)
			}
		}
		h.GetLoggerForContext(ctx).ErrorCtx(
			ctx,
			"error when performing merge: expected 200 status code",
			"code", resp.StatusCode,
			"body", string(buf),
		)
		return errors.New("error when performing merge: expected 200 status code")
	}

	if resp.StatusCode != http.StatusOK {
		h.GetLoggerForContext(ctx).ErrorCtx(
			ctx,
			"error when performing merge: expected 200 status code",
			"code", resp.StatusCode,
			"body", string(buf),
		)
		return errors.New("error when performing merge: expected 200 status code")
	}

	var mergeResponse internal.MergeResponse
	if err := json.Unmarshal(buf, &mergeResponse); err != nil {
		h.GetLoggerForContext(ctx).ErrorCtx(
			ctx,
			"error when decoding merge response",
			"body", string(buf),
		)
		return errors.WithStack(err)
	}

	if !mergeResponse.Merged {
		return errors.New("pr was not merged")
	}
	h.GetLoggerForContext(ctx).InfoCtx(
		ctx,
		"pr merged",
		"repo", req.Repository.FullName,
		"pr", req.PullRequest.Number,
	)
	return nil
}

func (h *Handler) shouldExecuteLogic(req *internal.Request, w http.ResponseWriter, r *http.Request) bool {
	githubEvent := r.Header.Get("X-GitHub-Event")

	switch githubEvent {
	case "check_run":
		switch req.Action {
		case "completed":
			return true
		}

	case "pull_request":
		switch req.Action {
		case "created", "opened", "labeled", "reopened", "synchronize", "edited":
			return true
		}
	}
	return false
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
		h.GetLoggerForContext(r.Context()).ErrorCtx(r.Context(), "unable to read body", "err", err)
		h.respond(w, http.StatusBadRequest, "bad request")
		return
	}

	var req internal.Request
	if err := json.Unmarshal(body, &req); err != nil {
		h.GetLoggerForContext(r.Context()).ErrorCtx(r.Context(), "unable to decode request", "err", err, "body", string(body))
		h.respond(w, http.StatusBadRequest, "bad request")
		return
	}

	if req.PullRequest == nil {
		h.GetLoggerForContext(r.Context()).InfoCtx(r.Context(), "request didn't contain a pull_request item", "body", string(body))
		h.respond(w, http.StatusOK, "ok")
		return
	}

	if req.Repository == nil {
		h.GetLoggerForContext(r.Context()).InfoCtx(r.Context(), "request didn't contain a repository item", "body", string(body))
		h.respond(w, http.StatusOK, "ok")
		return
	}

	if !h.shouldExecuteLogic(&req, w, r) {
		h.respond(w, http.StatusOK, "ok")
		return
	}

	if req.PullRequest.State != "open" {
		h.respond(w, http.StatusOK, "ok")
		return
	}

	for _, label := range req.PullRequest.Labels {
		if label.Name == "merge" || label.Name == "force-merge" {
			accessToken, err := h.getAccessToken(r.Context(), &req)
			if err != nil {
				h.GetLoggerForContext(r.Context()).ErrorCtx(r.Context(), "error getting access token", "body", string(body))
				h.respond(w, http.StatusOK, "ok")
				return
			}

			checksOK := true
			if label.Name == "merge" {
				checksOK, err = h.areChecksGreen(r.Context(), accessToken, &req)
				if err != nil {
					h.GetLoggerForContext(r.Context()).ErrorCtx(r.Context(), "error during getting checks", "body", string(body))
					h.respond(w, http.StatusOK, "ok")
					return
				}
			}

			if checksOK {
				if err := h.mergePullRequest(r.Context(), accessToken, &req, 0); err != nil {
					h.GetLoggerForContext(r.Context()).ErrorCtx(r.Context(), "error during merge", "body", string(body))
				}
			}

			h.respond(w, http.StatusOK, "ok")
			return
		}
	}

	h.respond(w, http.StatusOK, "ok")
}
