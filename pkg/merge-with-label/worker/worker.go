package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/go-multierror"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
)

type Worker struct {
	Logger  *zerolog.Logger
	BotName string

	AllowedRepositories         common.RegexSlice
	AllowOnlyPublicRepositories bool

	PushSubscription        *nats.Subscription
	StatusSubscription      *nats.Subscription
	PullRequestSubscription *nats.Subscription

	AccessTokensKV nats.KeyValue
	ConfigsKV      nats.KeyValue
	CheckRunsKV    nats.KeyValue

	JetStreamContext   nats.JetStreamContext
	PullRequestSubject string
	RetryWait          time.Duration

	MaxDurationForPushWorker        time.Duration
	MaxDurationForPullRequestWorker time.Duration

	RateLimitKV       nats.KeyValue
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

func (worker *Worker) Consume() error {
	worker.closeCh = make(chan struct{})
	errChan := make(chan error)

	pushChan := make(chan *nats.Msg, worker.MessageChannelSizePerSubjectSetting)
	go func() {
		for {
			msg, err := worker.PushSubscription.NextMsgWithContext(context.Background())
			if err != nil {
				errChan <- err
				return
			}
			pushChan <- msg
		}
	}()

	statusChan := make(chan *nats.Msg, worker.MessageChannelSizePerSubjectSetting)
	go func() {
		for {
			msg, err := worker.StatusSubscription.NextMsgWithContext(context.Background())
			if err != nil {
				errChan <- err
				return
			}
			statusChan <- msg
		}
	}()

	pullRequestChan := make(chan *nats.Msg, worker.MessageChannelSizePerSubjectSetting)
	go func() {
		for {
			msg, err := worker.PullRequestSubscription.NextMsgWithContext(context.Background())
			if err != nil {
				errChan <- err
				return
			}
			pullRequestChan <- msg
		}
	}()

	pushMsgWorker := pushWorker{
		Worker: worker,
	}

	statusMsgWorker := statusWorker{
		Worker: worker,
	}

	pullRequestMsgWorker := pullRequestWorker{
		Worker: worker,
	}

	for {
		select {
		case msg := <-pushChan:
			worker.Logger.Debug().
				Msg("push message received")
			handleMessage[common.QueuePushMessage](worker, worker.Logger, msg, pushMsgWorker.runLogic)
		case msg := <-statusChan:
			worker.Logger.Debug().
				Msg("status message received")
			handleMessage[common.QueueStatusMessage](worker, worker.Logger, msg, statusMsgWorker.runLogic)
		case msg := <-pullRequestChan:
			worker.Logger.Debug().
				Str("id", msg.Header.Get(nats.MsgIdHdr)).
				Msg("pull_request message received")
			handleMessage[common.QueuePullRequestMessage](worker, worker.Logger, msg, pullRequestMsgWorker.runLogic)
		case err := <-errChan:
			return errors.Wrap(err, "error received")
		case <-worker.closeCh:
			worker.Logger.Debug().Msg("close signal received")
			return nil
		}
	}
}

func (worker *Worker) Shutdown(context.Context) error {
	worker.closeCh <- struct{}{}
	return nil
}

func handleMessage[T common.Message](worker *Worker, logger *zerolog.Logger, msg *nats.Msg, fn func(logger *zerolog.Logger, m *T) error) {
	if common.DelayMessageIfNeeded(logger, msg) {
		return
	}

	var m T
	if err := json.Unmarshal(msg.Data, &m); err != nil {
		logger.Error().Err(err).Msg("unable to decode queue message")
		if err := msg.NakWithDelay(worker.RetryWait); err != nil {
			logger.Error().Err(err).Msg("unable to nak message")
		}
		return
	}

	if worker.AllowOnlyPublicRepositories && m.GetRepository().Private {
		logger.Warn().Str("repo", m.GetRepository().FullName).Msg("repository is not allowed (it is private)")
		if err := msg.Ack(); err != nil {
			logger.Error().Err(err).Msg("unable to ack message")
		}
		return
	}

	if worker.AllowedRepositories.ContainsOneOf(m.GetRepository().FullName) == "" {
		logger.Warn().Str("repo", m.GetRepository().FullName).Msg("repository is not allowed")
		if err := msg.Ack(); err != nil {
			logger.Error().Err(err).Msg("unable to ack message")
		}
		return
	}

	err := fn(logger, &m)
	if err != nil {
		var pbErr pushBackError
		delay := worker.RetryWait
		if errors.As(err, &pbErr) {
			delay = pbErr.delay
		} else {
			logger.Error().Err(err).Msg("error")
		}
		if err := msg.NakWithDelay(delay); err != nil {
			logger.Error().Err(err).Msg("unable to nak message")
		}
		return
	}
	if err := msg.Ack(); err != nil {
		logger.Error().Err(err).Msg("unable to ack message")
	}
}

func (worker *Worker) workOnAllPullRequests(ctx context.Context,
	rootLogger *zerolog.Logger,
	sess *session) error {
	pullRequests, err := github.GetPullRequestsThatAreOpenAndHaveOneOfTheseLabels(
		ctx,
		worker.HTTPClient,
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
			rootLogger,
			worker.JetStreamContext,
			worker.RateLimitKV,
			worker.RateLimitInterval,
			worker.PullRequestSubject+"."+uuid.NewString(),
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
			Msg("published pull_request message")
	}
	return result
}
