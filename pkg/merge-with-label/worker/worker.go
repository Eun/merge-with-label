package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

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
	if err := json.Unmarshal(msg.Data, m); err != nil {
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
		logger.Error().Err(err).Msg("error")
		if err := msg.NakWithDelay(worker.RetryWait); err != nil {
			logger.Error().Err(err).Msg("unable to nak message")
		}
		return
	}
	if err := msg.Ack(); err != nil {
		logger.Error().Err(err).Msg("unable to ack message")
	}
}
