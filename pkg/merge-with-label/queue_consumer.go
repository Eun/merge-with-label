package merge_with_label

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/adjust/rmq/v5"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
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
		err = h.pushLogic(context.Background(), &msg)
	case PullRequestMessage:
		var msg QueuePullRequestMessage
		if err := decodeMap(m, &msg, false); err != nil {
			h.Logger.Error().Err(err).Msg("unable to decode pull request queue message")
			return
		}
		err = h.pullRequestLogic(context.Background(), &msg)
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

func (h *QueueConsumer) getAccessToken(ctx context.Context, repository *Repository, installationID int64) (string, error) {
	cachedToken, err := h.RedisCacheClient.Get(ctx, repository.FullName).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return "", errors.Wrap(err, "unable to get cached access token")
	}
	if cachedToken == "" || err == redis.Nil {
		accessToken, err := GetAccessToken(ctx, h.HTTPClient, h.AppID, h.PrivateKey, repository, installationID)
		if err != nil {
			return "", errors.Wrap(err, "unable to get access token")
		}
		if err = h.RedisCacheClient.Set(ctx, repository.FullName, accessToken.Token, accessToken.ExpiresAt.Sub(time.Now())).Err(); err != nil {
			return "", errors.Wrap(err, "unable to cache access token")
		}
		return accessToken.Token, nil
	}
	return cachedToken, nil
}

func (h *QueueConsumer) pullRequestLogic(ctx context.Context, msg *QueuePullRequestMessage) error {
	labels := make(map[string]struct{})
	for _, label := range msg.PullRequest.Labels {
		labels[label.Name] = struct{}{}
	}

	accessToken, err := h.getAccessToken(ctx, msg.Repository, msg.InstallationID)
	if err != nil {
		return errors.Wrap(err, "error getting access token")
	}

	state, aheadBy, err := GetPullRequestDetails(ctx, h.HTTPClient, accessToken, msg.Repository, msg.PullRequest.Number)
	if err != nil {
		return errors.Wrap(err, "error getting pull request details")
	}

	if state != "OPEN" {
		h.Logger.Debug().Int("number", msg.PullRequest.Number).Msg("pull request is not open anymore")
		return nil
	}

	if _, ok := labels["auto-update"]; ok {
		if aheadBy > 0 {
			h.Logger.Info().Int("number", msg.PullRequest.Number).Msg("updating pull request")
			if err := UpdatePullRequest(ctx, h.HTTPClient, accessToken, msg.Repository, msg.PullRequest.Number); err != nil {
				return pushBackError{
					delayUntil:  time.Now().Add(time.Second * 10),
					nestedError: errors.Wrap(err, "error updating pull request"),
				}
			}
		}
	}

	if _, ok := labels["auto-merge"]; ok {
		var commitTime time.Time
		var checksStatus string
		if _, ok := labels["force-merge"]; !ok {
			var err error
			h.Logger.Debug().Int("number", msg.PullRequest.Number).Msg("getting checks for pull request")
			checksStatus, commitTime, err = GetCheckSuiteStatusForPullRequest(ctx, h.HTTPClient, accessToken, msg.Repository, msg.PullRequest.Number)
			if err != nil {
				return errors.Wrap(err, "error during getting checks")
			}
		}

		if checksStatus == "SUCCESS" || checksStatus == "" {
			if diff := commitTime.Add(time.Second * 10).Sub(time.Now()); diff > 0 {
				// it's a bit too early. block merging, push back onto the queue
				h.Logger.Debug().Int("number", msg.PullRequest.Number).Msg("delaying merge, because commit was too recent")
				return pushBackError{
					delayUntil: time.Now().Add(diff),
				}
			}
			h.Logger.Info().Int("number", msg.PullRequest.Number).Msg("merging pull request")
			if err := MergePullRequest(ctx, h.HTTPClient, accessToken, msg.Repository, msg.PullRequest); err != nil {
				return pushBackError{
					delayUntil:  time.Now().Add(time.Second * 10),
					nestedError: err,
				}
			}
		}
	}
	return nil
}

func (h *QueueConsumer) pushLogic(ctx context.Context, msg *QueuePushMessage) error {
	accessToken, err := GetAccessToken(ctx, h.HTTPClient, h.AppID, h.PrivateKey, msg.Repository, msg.InstallationID)
	if err != nil {
		return errors.Wrap(err, "error getting access token")
	}
	pullRequests, err := GetPullRequestsThatNeedToBeUpdated(ctx, h.HTTPClient, accessToken.Token, msg.Repository)
	if err != nil {
		return errors.Wrap(err, "error getting pull requests")
	}
	if len(pullRequests) == 0 {
		h.Logger.Debug().Msg("no pull requests available that need to be updated")
		return nil
	}

	gotErrors := false
	for _, number := range pullRequests {
		h.Logger.Info().Int("number", number).Msg("updating pull request")
		if err := UpdatePullRequest(ctx, h.HTTPClient, accessToken.Token, msg.Repository, number); err != nil {
			gotErrors = true
			h.Logger.Error().Err(err).Int("number", number).Msg("error updating pull request")
		}
	}
	if gotErrors {
		return pushBackError{
			delayUntil: time.Now().Add(time.Second * 10),
		}
	}
	return nil
}
