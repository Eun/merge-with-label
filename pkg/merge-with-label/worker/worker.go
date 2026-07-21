package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

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

// Worker processes jobs from the two queue lanes (repo and pull_request).
type Worker struct {
	Logger  *zerolog.Logger
	BotName string

	AllowedRepositories         common.RegexSlice
	AllowOnlyPublicRepositories bool

	Store *pgqueue.Store

	RetryWait   time.Duration
	MaxAttempts int // jobs exceeding this are permanently deleted; 0 = unlimited

	// MaxConcurrentJobs limits how many jobs run in parallel per queue lane.
	// Defaults to 1 if zero, keeping existing sequential behavior.
	MaxConcurrentJobs int

	MaxDurationForRepoWorker        time.Duration
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

func (e pushBackError) Error() string { return "" }

// Consume starts polling both queue lanes concurrently until the context is
// canceled or Shutdown is called.
func (w *Worker) Consume() error {
	w.closeCh = make(chan struct{})
	errChan := make(chan error, 2) //nolint:mnd // 2 queue types

	repoMsgWorker := &repoWorker{Worker: w}
	prMsgWorker := &pullRequestWorker{Worker: w}

	maxConcurrent := w.MaxConcurrentJobs
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	// startPoller is a generic polling loop parameterised by the dequeue and
	// reschedule functions so each queue table gets its own dedicated goroutine
	// without any shared msgType string. Each dequeued job is dispatched into
	// its own goroutine; a semaphore caps parallelism to MaxConcurrentJobs so
	// a slow or permanently-failing job never blocks work on other jobs.
	type dequeueFn func(context.Context) (*pgqueue.Job, error)
	type rescheduleFn func(context.Context, int64, string, []byte, time.Duration, int) (bool, error)

	startPoller := func(
		queueName string,
		dequeueFn dequeueFn,
		rescheduleFn rescheduleFn,
		handle func(*zerolog.Logger, []byte) (dedupKey string, payload []byte, err error),
	) {
		go func() {
			sem := make(chan struct{}, maxConcurrent)
			for {
				select {
				case <-w.closeCh:
					return
				default:
				}

				// Acquire a slot before dequeueing so we don't pull a job we
				// cannot start immediately.
				select {
				case sem <- struct{}{}:
				case <-w.closeCh:
					return
				}

				job, err := dequeueFn(context.Background())
				if err != nil {
					<-sem
					errChan <- errors.Wrapf(err, "dequeue %s", queueName)
					return
				}
				if job == nil {
					<-sem
					time.Sleep(pollInterval)
					continue
				}

				w.Logger.Debug().Str("queue", queueName).Int64("job_id", job.ID).Msg("job dequeued")

				// Process in a separate goroutine so the poller loop immediately
				// picks up the next job (up to maxConcurrent).
				go func(j *pgqueue.Job) {
					defer func() { <-sem }()
					dedupKey, payload, handleErr := handle(w.Logger, j.Payload)
					if handleErr == nil {
						return
					}
					var pbErr pushBackError
					delay := w.RetryWait
					if errors.As(handleErr, &pbErr) {
						delay = pbErr.delay
					} else {
						w.Logger.Error().Err(handleErr).Str("queue", queueName).Msg("job failed")
					}
					dropped, reschedErr := rescheduleFn(
						context.Background(), j.ID, dedupKey, payload, delay, w.MaxAttempts,
					)
					if reschedErr != nil {
						w.Logger.Error().Err(reschedErr).Msg("unable to reschedule job")
					}
					if dropped {
						w.Logger.Warn().
							Str("queue", queueName).
							Str("dedup_key", dedupKey).
							Int("max_attempts", w.MaxAttempts).
							Msg("job permanently removed after max attempts")
					}
				}(job)
			}
		}()
	}

	startPoller("mwl_repo_queue", w.Store.DequeueRepo, w.Store.RescheduleRepo,
		func(logger *zerolog.Logger, payload []byte) (string, []byte, error) {
			var m common.QueueRepoMessage
			if err := json.Unmarshal(payload, &m); err != nil {
				return "", payload, errors.Wrap(err, "unmarshal repo message")
			}
			dedupKey := common.RepoDedupKey(m.Repository.NodeID)
			if !w.isAllowed(logger, &m) {
				return dedupKey, payload, nil
			}
			return dedupKey, payload, repoMsgWorker.runLogic(logger, &m)
		},
	)

	startPoller("mwl_pr_queue", w.Store.DequeuePR, w.Store.ReschedulePR,
		func(logger *zerolog.Logger, payload []byte) (string, []byte, error) {
			var m common.QueuePRMessage
			if err := json.Unmarshal(payload, &m); err != nil {
				return "", payload, errors.Wrap(err, "unmarshal PR message")
			}
			dedupKey := common.PRDedupKey(m.Repository.NodeID, m.PullRequest.Number)
			if !w.isAllowed(logger, &m) {
				return dedupKey, payload, nil
			}
			return dedupKey, payload, prMsgWorker.runLogic(logger, &m)
		},
	)

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

// fanOutPRs finds all eligible open PRs for a repo and enqueues a PR job for
// each one. Because EnqueuePR uses ON CONFLICT DO NOTHING, a PR that is
// already in the queue will not produce a duplicate row.
func (w *Worker) fanOutPRs(ctx context.Context, rootLogger *zerolog.Logger, sess *session) error {
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
		err := common.EnqueuePR(ctx, rootLogger, w.Store, w.RateLimitInterval, &common.QueuePRMessage{
			BaseMessage: common.BaseMessage{
				InstallationID: sess.InstallationID,
				Repository:     *sess.Repository,
			},
			PullRequest: pullRequests[i],
		})
		if err != nil {
			rootLogger.Error().Int64("number", pullRequests[i].Number).Err(err).Msg("unable to enqueue PR")
			result = multierror.Append(result, errors.Wrap(err, "unable to enqueue PR"))
		} else {
			rootLogger.Debug().Int64("number", pullRequests[i].Number).Msg("enqueued PR message")
		}
	}
	return result
}
