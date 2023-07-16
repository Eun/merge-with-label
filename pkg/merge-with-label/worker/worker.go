package worker

import (
	"context"
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

	AllowedRepositories common.RegexSlice

	PushSubscription        *nats.Subscription
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

	pushMsgWorker := pushWorker{
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
			pushMsgWorker.handleMessage(worker.Logger, msg)
		case msg := <-pullRequestChan:
			worker.Logger.Debug().
				Str("id", msg.Header.Get(nats.MsgIdHdr)).
				Msg("pull_request message received")
			pullRequestMsgWorker.handleMessage(worker.Logger, msg)
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
