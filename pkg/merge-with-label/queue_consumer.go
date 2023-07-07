package merge_with_label

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/adjust/rmq/v5"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"golang.org/x/exp/slices"
)

type QueueConsumer struct {
	PushBackQueue    rmq.Queue
	Logger           *zerolog.Logger
	HTTPClient       *http.Client
	AppID            int64
	PrivateKey       []byte
	RedisCacheClient *redis.Client
	MaxRetries       int
}

type QueueConsumerSession struct {
	*QueueConsumer
	ctx            context.Context
	InstallationID int64
	Repository     *Repository

	accessToken   AccessToken
	config        *ConfigV1
	latestBaseSha string
	checkRunId    string
}

const accessTokenSuffix = "token"
const configSuffix = "config"

type pushBackError struct {
	delayUntil  time.Time
	nestedError error
}

func (e pushBackError) Error() string {
	if e.nestedError == nil {
		return ""
	}
	return e.nestedError.Error()
}

func (h *QueueConsumer) Consume(delivery rmq.Delivery) {
	// always ack, we construct a new message on failure
	if err := delivery.Ack(); err != nil {
		h.Logger.Error().Err(err).Msg("unable to ack queue message")
		return
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(delivery.Payload()), &m); err != nil {
		h.Logger.Error().Err(err).Msg("unable to decode queue message into map")
		return
	}
	var hdr QueueMessage

	if err := decodeMap(m, &hdr, true); err != nil {
		h.Logger.Error().Err(err).Msg("unable to decode queue message")
		return
	}

	h.Logger.Debug().Str("id", hdr.ID).Int("type", int(hdr.Kind)).Msg("incoming message")

	var err error
	switch hdr.Kind {
	case PushRequestMessage:
		var msg QueuePushMessage
		if err := decodeMap(m, &msg, false); err != nil {
			h.Logger.Error().Err(err).Msg("unable to decode push queue message")
			return
		}
		sess := QueueConsumerSession{
			QueueConsumer:  h,
			ctx:            context.Background(),
			InstallationID: msg.InstallationID,
			Repository:     msg.Repository,
		}
		err = sess.pushLogic(&msg)
	case PullRequestMessage:
		var msg QueuePullRequestMessage
		if err := decodeMap(m, &msg, false); err != nil {
			h.Logger.Error().Err(err).Msg("unable to decode pull request queue message")
			return
		}
		sess := QueueConsumerSession{
			QueueConsumer:  h,
			ctx:            context.Background(),
			InstallationID: msg.InstallationID,
			Repository:     msg.Repository,
		}
		err = sess.pullRequestLogic(&msg)
	default:
		err = errors.New("unknown message")
	}

	if err == nil {
		return
	}

	var pbErr pushBackError
	if !errors.As(err, &pbErr) {
		h.Logger.Error().Err(err).Send()
		return
	}

	hdr.PushBackCounter++

	if hdr.PushBackCounter > h.MaxRetries {
		h.Logger.Error().Err(pbErr.nestedError).Send()
		return
	}
	m["push_back_counter"] = hdr.PushBackCounter
	m["delay_until"] = pbErr.delayUntil

	msg, err := json.Marshal(m)
	if err != nil {
		h.Logger.Error().Err(err).Msg("unable to re-encode message")
		return
	}
	h.Logger.Debug().
		Int("push_back_counter", hdr.PushBackCounter).
		Time("delay_until", pbErr.delayUntil).
		Msg("re-sending message")
	if err := h.PushBackQueue.PublishBytes(msg); err != nil {
		h.Logger.Error().Err(err).Msg("unable to re-publish message")
		return
	}
}

func (sess *QueueConsumerSession) getLatestBaseSha() (string, error) {
	if sess.latestBaseSha != "" {
		return sess.latestBaseSha, nil
	}

	accessToken, err := sess.getAccessToken()
	if err != nil {
		return "", errors.Wrap(err, "error getting access token")
	}

	sess.latestBaseSha, err = GetLatestCommitSha(sess.ctx, sess.HTTPClient, accessToken, sess.Repository)
	if err != nil {
		return "", errors.Wrap(err, "error getting latest commit sha")
	}
	return sess.latestBaseSha, nil
}

func (sess *QueueConsumerSession) getAccessToken() (string, error) {
	if sess.accessToken.Token != "" {
		return sess.accessToken.Token, nil
	}
	cachedToken, err := sess.RedisCacheClient.Get(sess.ctx, sess.Repository.FullName+":"+accessTokenSuffix).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return "", errors.Wrap(err, "unable to get cached access token")
	}
	if cachedToken == "" || err == redis.Nil {
		accessToken, err := GetAccessToken(sess.ctx, sess.HTTPClient, sess.AppID, sess.PrivateKey, sess.Repository, sess.InstallationID)
		if err != nil {
			return "", errors.Wrap(err, "unable to get access token")
		}
		if err = sess.RedisCacheClient.Set(sess.ctx, sess.Repository.FullName+":"+accessTokenSuffix, accessToken.Token, accessToken.ExpiresAt.Sub(time.Now())).Err(); err != nil {
			return "", errors.Wrap(err, "unable to cache access token")
		}
		return accessToken.Token, nil
	}
	return cachedToken, nil
}

func (sess *QueueConsumerSession) getConfig() (*ConfigV1, error) {
	if sess.config != nil {
		return sess.config, nil
	}
	sha, err := sess.getLatestBaseSha()
	if err != nil {
		return nil, errors.Wrap(err, "error getting latest commit sha details")
	}

	if sha == "" {
		sess.Logger.Debug().Msg("latest commit sha is empty")
		return nil, nil
	}

	cachedConfigString, err := sess.RedisCacheClient.Get(sess.ctx, sess.Repository.FullName+":"+configSuffix).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, errors.Wrap(err, "unable to get cached config")
	}
	if cachedConfigString == "" || err == redis.Nil {
		return sess.getLatestConfig(sha)
	}

	var config cachedConfig
	if err := json.Unmarshal([]byte(cachedConfigString), &config); err != nil {
		return nil, errors.Wrap(err, "unable to decode cached config")
	}
	if config.SHA == sha {
		return config.ConfigV1, nil
	}

	return sess.getLatestConfig(sha)
}

func (sess *QueueConsumerSession) getLatestConfig(sha string) (*ConfigV1, error) {
	accessToken, err := sess.getAccessToken()
	if err != nil {
		return nil, errors.Wrap(err, "error getting access token")
	}

	cfg, err := GetConfig(sess.ctx, sess.HTTPClient, accessToken, sess.Repository, sha)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get config")
	}
	if cfg == nil {
		return defaultConfig()
	}

	buf, err := json.Marshal(&cachedConfig{
		ConfigV1: cfg,
		SHA:      sha,
	})
	if err != nil {
		return nil, errors.Wrap(err, "unable to encode cached config")
	}
	if err := sess.RedisCacheClient.Set(sess.ctx, sess.Repository.FullName+":"+configSuffix, buf, time.Now().Add(time.Hour*24).Sub(time.Now())).Err(); err != nil {
		return nil, errors.Wrap(err, "unable to cache config")
	}
	sess.config = cfg
	return cfg, nil
}

func (sess *QueueConsumerSession) CreateOrUpdateCheckRun(sha, status, title, summary string) error {
	if sha == "" {
		return nil
	}
	accessToken, err := sess.getAccessToken()
	if err != nil {
		return errors.Wrap(err, "error getting access token")
	}
	if sess.checkRunId == "" {
		sess.checkRunId, err = CreateCheckRun(
			sess.ctx,
			sess.HTTPClient,
			accessToken,
			sess.Repository,
			sha,
			status,
			"merge-with-label",
			title,
			summary,
		)
		if err != nil {
			return errors.Wrap(err, "error creating check run")
		}
		return err
	}
	sess.checkRunId, err = UpdateCheckRun(
		sess.ctx,
		sess.HTTPClient,
		accessToken,
		sess.Repository,
		sess.checkRunId,
		sha,
		status,
		"merge-with-label",
		title,
		summary,
	)
	if err != nil {
		return errors.Wrap(err, "error updating check run")
	}
	return nil
}

func (sess *QueueConsumerSession) updatePullRequest(prNumber int, details *PullRequestDetails) error {
	cfg, err := sess.getConfig()
	if err != nil {
		return errors.Wrap(err, "unable to get config")
	}
	if cfg.Update.Label == "" {
		return nil
	}
	if slices.Index(details.Labels, cfg.Update.Label) == -1 {
		return nil
	}
	if details.AheadBy == 0 {
		return nil
	}

	ignoredBy, err := cfg.Update.IsTitleIgnored(details.Title)
	if err != nil {
		return errors.WithStack(err)
	}
	if ignoredBy != "" {
		sess.Logger.Info().
			Str("title", details.Title).
			Msg("title is in ignore list")
		if err := sess.CreateOrUpdateCheckRun(details.LastCommitSha,
			"COMPLETED",
			"not updating: title is in ignore list",
			fmt.Sprintf("`%s` is in the ignore list (`%s`)", details.Title, cfg.Update.IgnoreWithTitles.String()),
		); err != nil {
			return errors.WithStack(err)
		}
		return nil
	}

	ignoredBy, err = cfg.Update.IsUserIgnored(details.Author)
	if err != nil {
		return errors.WithStack(err)
	}
	if ignoredBy != "" {
		sess.Logger.Info().
			Str("author", details.Author).
			Msg("author is in ignore list")
		if err := sess.CreateOrUpdateCheckRun(details.LastCommitSha,
			"COMPLETED",
			"not updating: author is in ignore list",
			fmt.Sprintf("`%s` is in the ignore list (`%s`)", details.Author, cfg.Update.IgnoreFromUsers.String()),
		); err != nil {
			return errors.WithStack(err)
		}
		return nil
	}

	accessToken, err := sess.getAccessToken()
	if err != nil {
		return errors.Wrap(err, "error getting access token")
	}

	sess.Logger.Info().Int("number", prNumber).Msg("updating pull request")
	if err := sess.CreateOrUpdateCheckRun(details.LastCommitSha,
		"COMPLETED",
		"updating",
		"",
	); err != nil {
		return errors.WithStack(err)
	}
	if err := UpdatePullRequest(sess.ctx, sess.HTTPClient, accessToken, sess.Repository, prNumber); err != nil {
		return pushBackError{
			delayUntil:  time.Now().Add(time.Second * 10),
			nestedError: errors.Wrap(err, "error updating pull request"),
		}
	}
	return nil
}

func (sess *QueueConsumerSession) pullRequestLogic(msg *QueuePullRequestMessage) error {
	accessToken, err := sess.getAccessToken()
	if err != nil {
		return errors.Wrap(err, "error getting access token")
	}

	cfg, err := sess.getConfig()
	if err != nil {
		return errors.Wrap(err, "unable to get config")
	}

	if cfg == nil {
		sess.Logger.Debug().Msg("no config")
		return nil
	}

	if cfg.Merge.Label == "" && cfg.Update.Label == "" {
		sess.Logger.Debug().Msg("merge and update are disabled")
		return nil
	}

	details, err := GetPullRequestDetails(sess.ctx, sess.HTTPClient, accessToken, msg.Repository, msg.PullRequest.Number)
	if err != nil {
		return errors.Wrap(err, "error getting pull request details")
	}

	if details.State != "OPEN" {
		sess.Logger.Debug().Int("number", msg.PullRequest.Number).Msg("pull request is not open anymore")
		return nil
	}

	// update logic
	err = sess.updatePullRequest(msg.PullRequest.Number, details)
	if err != nil {
		return errors.WithStack(err)
	}

	// merge logic
	err = func() error {
		if cfg.Merge.Label == "" {
			return nil
		}
		if slices.Index(details.Labels, cfg.Merge.Label) != -1 {
			return nil
		}

		ignoredBy, err := cfg.Merge.IsTitleIgnored(details.Title)
		if err != nil {
			return errors.WithStack(err)
		}
		if ignoredBy != "" {
			sess.Logger.Info().
				Str("title", details.Title).
				Msg("title is in ignore list")
			if err := sess.CreateOrUpdateCheckRun(msg.PullRequest.Head.SHA,
				"COMPLETED",
				"not merging: title is in ignore list",
				fmt.Sprintf("`%s` is in the ignore list (`%s`)", details.Title, cfg.Merge.IgnoreWithTitles.String()),
			); err != nil {
				return errors.WithStack(err)
			}
			return nil
		}

		ignoredBy, err = cfg.Merge.IsUserIgnored(details.Author)
		if err != nil {
			return errors.WithStack(err)
		}
		if ignoredBy != "" {
			sess.Logger.Info().
				Str("author", details.Author).
				Msg("author is in ignore list")
			if err := sess.CreateOrUpdateCheckRun(msg.PullRequest.Head.SHA,
				"COMPLETED",
				"not merging: author is in ignore list",
				fmt.Sprintf("`%s` is in the ignore list (`%s`)", details.Author, cfg.Merge.IgnoreFromUsers.String()),
			); err != nil {
				return errors.WithStack(err)
			}
			return nil
		}

		if cfg.Merge.RequiredApprovals > 0 && cfg.Merge.RequiredApprovals < details.ApprovedCount {
			sess.Logger.Info().
				Int("required_approvals", cfg.Merge.RequiredApprovals).
				Int("current_approvals", details.ApprovedCount).
				Msg("missing required approvals")
			if err := sess.CreateOrUpdateCheckRun(msg.PullRequest.Head.SHA,
				"COMPLETED",
				"not merging: missing required approvals",
				fmt.Sprintf("%d approvals are required, got %d", cfg.Merge.RequiredApprovals, details.ApprovedCount),
			); err != nil {
				return errors.WithStack(err)
			}
			return nil
		}

		if cfg.Merge.RequireLinearHistory && details.AheadBy > 0 {
			sess.Logger.Info().
				Msg("not merging: a linear history is required")
			if err := sess.CreateOrUpdateCheckRun(msg.PullRequest.Head.SHA,
				"COMPLETED",
				"not merging: a linear history is required",
				fmt.Sprintf("the branch is not upto date with the latest changes from `%s` branch", sess.Repository.DefaultBranch),
			); err != nil {
				return errors.WithStack(err)
			}
			return nil
		}

		if len(cfg.Merge.RequiredChecks) > 0 {
			type checkInfo struct {
				name  string
				check string
			}
			var checksNotSucceeded []checkInfo
			var checksMissing []string
			for _, check := range cfg.Merge.RequiredChecks {
				re, err := regexp.Compile(check)
				if err != nil {
					sess.Logger.Error().Err(err).Str("check", check).Msg("regex is invalid")
					continue
				}
				foundCheck := false
				for name, state := range details.CheckStates {
					if !re.MatchString(strings.ToLower(name)) {
						continue
					}
					foundCheck = true
					if state != "SUCCESS" {
						sess.Logger.Info().
							Str("name", name).
							Str("state", state).
							Str("check", check).
							Msg("check is not SUCCESS")

						checksNotSucceeded = append(checksNotSucceeded, checkInfo{
							name:  name,
							check: check,
						})
					}
				}
				if !foundCheck {
					sess.Logger.Info().
						Str("check", check).
						Msg("check is missing")
					checksMissing = append(checksMissing, check)
				}
			}

			if len(checksMissing) > 0 {
				var sb strings.Builder
				for _, info := range checksMissing {
					_, _ = fmt.Fprintf(&sb, "no check matches `%s`\n", info)
				}
				if err := sess.CreateOrUpdateCheckRun(msg.PullRequest.Head.SHA,
					"COMPLETED",
					"not merging: check(s) missing",
					sb.String(),
				); err != nil {
					return errors.WithStack(err)
				}
				return nil
			}

			if len(checksNotSucceeded) > 0 {
				var sb strings.Builder
				for _, info := range checksNotSucceeded {
					_, _ = fmt.Fprintf(&sb, "check `%s` did not succeed (matched by `%sz)\n", info.name, info.check)
				}
				if err := sess.CreateOrUpdateCheckRun(msg.PullRequest.Head.SHA,
					"COMPLETED",
					"not merging: check(s) did not succeeded",
					sb.String(),
				); err != nil {
					return errors.WithStack(err)
				}
				return nil
			}

			if diff := details.LastCommitTime.Add(time.Second * 10).Sub(time.Now()); diff > 0 {
				// it's a bit too early. block merging, push back onto the queue
				sess.Logger.Debug().Int("number", msg.PullRequest.Number).Msg("delaying merge, because commit was too recent")
				return pushBackError{
					delayUntil: time.Now().Add(diff),
				}
			}
		}

		sess.Logger.Info().Int("number", msg.PullRequest.Number).Msg("merging pull request")
		if err := sess.CreateOrUpdateCheckRun(msg.PullRequest.Head.SHA, "COMPLETED", "merging...", ""); err != nil {
			return errors.WithStack(err)
		}
		if err := MergePullRequest(sess.ctx, sess.HTTPClient, accessToken, msg.Repository, msg.PullRequest); err != nil {
			return pushBackError{
				delayUntil:  time.Now().Add(time.Second * 10),
				nestedError: err,
			}
		}
		return nil
	}()
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (sess *QueueConsumerSession) pushLogic(msg *QueuePushMessage) error {
	cfg, err := sess.getConfig()
	if err != nil {
		return errors.Wrap(err, "unable to get config")
	}

	if cfg == nil {
		sess.Logger.Debug().Msg("no config")
		return nil
	}

	if cfg.Update.Label == "" {
		sess.Logger.Debug().Msg("update is disabled")
		return nil
	}

	accessToken, err := sess.getAccessToken()
	if err != nil {
		return errors.Wrap(err, "error getting access token")
	}

	pullRequests, err := GetPullRequestsThatNeedToBeUpdated(sess.ctx, sess.HTTPClient, accessToken, msg.Repository, cfg.Update.Label)
	if err != nil {
		return errors.Wrap(err, "error getting pull requests")
	}
	if len(pullRequests) == 0 {
		sess.Logger.Debug().Msg("no pull requests available that need to be updated")
		return nil
	}

	gotErrors := false
	for _, number := range pullRequests {
		sess.Logger.Info().Int("number", number).Msg("updating pull request")
		details, err := GetPullRequestDetails(sess.ctx, sess.HTTPClient, accessToken, msg.Repository, number)
		if err != nil {
			sess.Logger.Error().Err(err).Int("number", number).Msg("error get pull request details")
			gotErrors = true
			continue
		}
		if err := sess.updatePullRequest(number, details); err != nil {
			sess.Logger.Error().Err(err).Int("number", number).Msg("unable to update pull request")
			gotErrors = true
			continue
		}
	}
	if gotErrors {
		return pushBackError{
			delayUntil: time.Now().Add(time.Second * 10),
		}
	}
	return nil
}
