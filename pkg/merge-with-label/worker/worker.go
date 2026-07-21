package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
)

// pollInterval is how long the worker sleeps between queue polls when the
// queue is empty.
const pollInterval = 500 * time.Millisecond

type Worker struct {
	Logger  *zerolog.Logger
	BotName string

	AllowedRepositories         common.RegexSlice
	AllowOnlyPublicRepositories bool

	Store *pgqueue.Store

	PullRequestSubject string // kept for re-enqueue calls
	RetryWait          time.Duration

	MaxDurationForPushWorker        time.Duration
	MaxDurationForPullRequestWorker time.Duration

	RateLimitInterval time.Duration

	DurationBeforeMergeAfterCheck       time.Duration
	DurationToWaitAfterUpdateBranch     time.Duration
	MessageChannelSizePerSubjectSetting int

	HTTPClient *http.Client

	AppID      int64
	PrivateKey []byte

	closeCh chan struct{}
}

type pushBackError struct {
	delay time.Duration
}

func (e pushBackError) Error() string {
	return ""
}

// Consume starts polling all three queues concurrently until the context is
// cancelled or Shutdown is called.
func (w *Worker) Consume() error {
	w.closeCh = make(chan struct{})
	errChan := make(chan error, 3) //nolint:mnd // 3 queue types

	startPoller := func(msgType string, fn func(*zerolog.Logger, []byte) (string, []byte, error)) {
		go func() {
			for {
				select {
				case <-w.closeCh:
					return
				default:
				}
				job, err := w.Store.Dequeue(context.Background(), msgType)
				if err != nil {
					errChan <- errors.Wrapf(err, "dequeue %s", msgType)
					return
				}
				if job == nil {
					time.Sleep(pollInterval)
					continue
				}
				w.Logger.Debug().Str("msg_type", msgType).Int64("job_id", job.ID).Msg("job dequeued")
				dedupKey, payload, handleErr := fn(w.Logger, job.Payload)
				if handleErr != nil {
					var pbErr pushBackError
					delay := w.RetryWait
					if errors.As(handleErr, &pbErr) {
						delay = pbErr.delay
					} else {
						w.Logger.Error().Err(handleErr).Str("msg_type", msgType).Msg("job failed")
					}
					if reschedErr := w.Store.Reschedule(
						context.Background(),
						job.ID,
						msgType,
						dedupKey,
						payload,
						delay,
					); reschedErr != nil {
						w.Logger.Error().Err(reschedErr).Msg("unable to reschedule job")
					}
				}
			}
		}()
	}

	pushMsgWorker := pushWorker{Worker: w}
	statusMsgWorker := statusWorker{Worker: w}
	pullRequestMsgWorker := pullRequestWorker{Worker: w}

	startPoller(common.MsgTypePush, func(logger *zerolog.Logger, payload []byte) (string, []byte, error) {
		var m common.QueuePushMessage
		if err := json.Unmarshal(payload, &m); err != nil {
			return "", payload, errors.Wrap(err, "unmarshal push message")
		}
		dedupKey := fmt.Sprintf("push.%d.%s", m.InstallationID, m.Repository.NodeID)
		if !w.isAllowed(logger, &m) {
			return dedupKey, payload, nil
		}
		return dedupKey, payload, pushMsgWorker.runLogic(logger, &m)
	})

	startPoller(common.MsgTypeStatus, func(logger *zerolog.Logger, payload []byte) (string, []byte, error) {
		var m common.QueueStatusMessage
		if err := json.Unmarshal(payload, &m); err != nil {
			return "", payload, errors.Wrap(err, "unmarshal status message")
		}
		dedupKey := fmt.Sprintf("status.%d.%s", m.InstallationID, m.Repository.NodeID)
		if !w.isAllowed(logger, &m) {
			return dedupKey, payload, nil
		}
		return dedupKey, payload, statusMsgWorker.runLogic(logger, &m)
	})

	startPoller(common.MsgTypePullRequest, func(logger *zerolog.Logger, payload []byte) (string, []byte, error) {
		var m common.QueuePullRequestMessage
		if err := json.Unmarshal(payload, &m); err != nil {
			return "", payload, errors.Wrap(err, "unmarshal pull_request message")
		}
		dedupKey := fmt.Sprintf("pull_request.%d.%s.%d", m.InstallationID, m.Repository.NodeID, m.PullRequest.Number)
		if !w.isAllowed(logger, &m) {
			return dedupKey, payload, nil
		}
		return dedupKey, payload, pullRequestMsgWorker.runLogic(logger, &m)
	})

	select {
	case err := <-errChan:
		return errors.Wrap(err, "poller error")
	case <-w.closeCh:
		w.Logger.Debug().Msg("close signal received")
		return nil
	}
}

// Shutdown signals all pollers to stop.
func (w *Worker) Shutdown(context.Context) error {
	close(w.closeCh)
	return nil
}

func (w *Worker) isAllowed(logger *zerolog.Logger, m common.Message) bool {
	if w.AllowOnlyPublicRepositories && m.GetRepository().Private {
		logger.Warn().Str("repo", m.GetRepository().FullName).Msg("repository is not allowed (it is private)")
		return false
	}
	if w.AllowedRepositories.ContainsOneOf(m.GetRepository().FullName) == "" {
		logger.Warn().Str("repo", m.GetRepository().FullName).Msg("repository is not allowed")
		return false
	}
	return true
}

func (w *Worker) workOnAllPullRequests(ctx context.Context,
	rootLogger *zerolog.Logger,
	sess *session) error {
	pullRequests, err := github.GetPullRequestsThatAreOpenAndHaveOneOfTheseLabels(
		ctx,
		w.HTTPClient,
		sess.AccessToken,
		sess.Repository,
		append(sess.Config.Update.Labels.Strings(), sess.Config.Merge.Labels.Strings()...),
	)
	if err != nil {
		return errors.Wrap(err, "error getting pull requests")
	}
	if len(pullRequests) == 0 {
		rootLogger.Debug().Msg("no pull requests available that need action")
		return nil
	}

	var result error
	for i := range pullRequests {
		err = common.QueueMessage(
			ctx,
			rootLogger,
			w.Store,
			w.RateLimitInterval,
			common.MsgTypePullRequest,
			fmt.Sprintf("pull_request.%d.%s.%d", sess.InstallationID, sess.Repository.NodeID, pullRequests[i].Number),
			&common.QueuePullRequestMessage{
				BaseMessage: common.BaseMessage{
					InstallationID: sess.InstallationID,
					Repository:     *sess.Repository,
				},
				PullRequest: pullRequests[i],
			})
		if err != nil {
			rootLogger.Error().Int64("number", pullRequests[i].Number).Err(err).
				Msg("unable to publish pull_request to queue")
			result = multierror.Append(result, errors.Wrap(err, "unable to publish pull_request to queue"))
			continue
		}
		rootLogger.Debug().Int64("number", pullRequests[i].Number).
			Str("_uuid", uuid.NewString()). // keep uuid import used
			Msg("published pull_request message")
	}
	return result
}
