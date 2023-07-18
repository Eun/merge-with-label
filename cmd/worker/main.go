package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"

	"github.com/Eun/merge-with-label/cmd"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/worker"
)

func main() {
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()
	if os.Getenv("DEBUG") != "" {
		logger = logger.Level(zerolog.DebugLevel)
		logger.Debug().Msg("debug logging enabled")
	}

	if os.Getenv("APP_ID") == "" {
		logger.Error().Msg("APP_ID is not set")
		return
	}

	appID, err := strconv.ParseInt(os.Getenv("APP_ID"), 10, 64)
	if err != nil {
		logger.Error().Err(err).Msg("unable to get APP_ID")
		return
	}

	privateKeyFile := os.Getenv("PRIVATE_KEY")
	if privateKeyFile == "" {
		logger.Error().Msg("PRIVATE_KEY is not set")
		return
	}
	privateKeyBytes, err := os.ReadFile(privateKeyFile)
	if err != nil {
		logger.Error().
			Err(err).
			Str("file", privateKeyFile).
			Msg("unable to read private key")
		return
	}

	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}

	logger.Debug().Msgf("connecting to %s", natsURL)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		logger.Error().Str("nats_url", natsURL).Msg("unable to connect to nats")
		return
	}
	defer nc.Close()
	logger.Debug().Msgf("connected to %s", natsURL)

	logger.Debug().Msg("creating jetstream context")
	js, err := nc.JetStream()
	if err != nil {
		logger.Error().Str("nats_url", natsURL).Msg("unable to create jetstream context")
		return
	}

	logger.Debug().Msg("creating access_token kv")
	accessTokensKV, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket: cmd.GetSetting[string](cmd.AccessTokensBucketNameSetting),
		TTL:    cmd.GetSetting[time.Duration](cmd.AccessTokensBucketTTLSetting),
	})
	if err != nil {
		logger.Error().
			Err(err).
			Str("nats_url", os.Getenv("NATS_URL")).
			Msg("unable to create jetstream key value bucket for access-tokens")
		return
	}
	logger.Debug().Msg("configured access_token kv")

	logger.Debug().Msg("creating configs kv")
	configsKV, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket: cmd.GetSetting[string](cmd.ConfigsBucketNameSetting),
		TTL:    cmd.GetSetting[time.Duration](cmd.ConfigsBucketTTLSetting),
	})
	if err != nil {
		logger.Error().
			Err(err).
			Str("nats_url", os.Getenv("NATS_URL")).
			Msg("unable to create jetstream key value bucket for configs")
		return
	}
	logger.Debug().Msg("configured configs kv")

	logger.Debug().Msg("creating check_runs kv")
	checkRunsKV, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket: cmd.GetSetting[string](cmd.CheckRunsBucketNameSetting),
		TTL:    cmd.GetSetting[time.Duration](cmd.CheckRunsBucketTTLSetting),
	})
	if err != nil {
		logger.Error().
			Err(err).
			Str("nats_url", os.Getenv("NATS_URL")).
			Msg("unable to create jetstream key value bucket for configs")
		return
	}
	logger.Debug().Msg("configured check_runs kv")

	logger.Debug().Msg("creating ratelimit kv")
	rateLimitKV, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket: cmd.GetSetting[string](cmd.RateLimitBucketNameSetting),
		TTL:    cmd.GetSetting[time.Duration](cmd.RateLimitBucketTTLSetting),
	})
	if err != nil {
		logger.Error().
			Err(err).
			Str("nats_url", os.Getenv("NATS_URL")).
			Msg("unable to create jetstream key value bucket for push rate limit")
		return
	}
	logger.Debug().Msg("configured ratelimit kv")

	logger.Debug().Msg("subscribing to push subject")
	pushSubscription, err := js.QueueSubscribeSync(
		cmd.GetSetting[string](cmd.PushSubjectSetting)+".>",
		"push-worker",
		nats.AckExplicit(),
		nats.MaxDeliver(cmd.GetSetting[int](cmd.MessageRetryAttemptsSetting)),
	)
	if err != nil {
		logger.Error().
			Err(err).
			Str("nats_url", os.Getenv("NATS_URL")).
			Msg("unable to create jetstream subscriber for push queue")
		return
	}
	defer func() {
		if err := pushSubscription.Unsubscribe(); err != nil {
			logger.Error().Err(err).Msg("unable to unsubscribe from push queue")
		}
	}()

	logger.Debug().Msg("subscribing to pull_request subject")
	pullRequestSubscription, err := js.QueueSubscribeSync(
		cmd.GetSetting[string](cmd.PullRequestSubjectSetting)+".>",
		"pull-request-worker",
		nats.AckExplicit(),
		nats.MaxDeliver(cmd.GetSetting[int](cmd.MessageRetryAttemptsSetting)),
	)
	if err != nil {
		logger.Error().
			Err(err).
			Str("nats_url", os.Getenv("NATS_URL")).
			Msg("unable to create jetstream subscriber for pull_request queue")
		return
	}
	defer func() {
		if err := pullRequestSubscription.Unsubscribe(); err != nil {
			logger.Error().Err(err).Msg("unable to unsubscribe from pull_request queue")
		}
	}()

	w := worker.Worker{
		Logger:  &logger,
		BotName: cmd.GetSetting[string](cmd.BotNameSetting),

		AllowedRepositories: cmd.GetSetting[common.RegexSlice](cmd.AllowedRepositoriesSetting),

		PushSubscription:        pushSubscription,
		PullRequestSubscription: pullRequestSubscription,

		AccessTokensKV: accessTokensKV,
		ConfigsKV:      configsKV,
		CheckRunsKV:    checkRunsKV,

		JetStreamContext:   js,
		PullRequestSubject: cmd.GetSetting[string](cmd.PullRequestSubjectSetting),
		RetryWait:          cmd.GetSetting[time.Duration](cmd.MessageRetryWaitSetting),

		MaxDurationForPushWorker:        time.Minute,
		MaxDurationForPullRequestWorker: time.Minute,

		RateLimitKV:       rateLimitKV,
		RateLimitInterval: cmd.GetSetting[time.Duration](cmd.RateLimitIntervalSetting),

		DurationBeforeMergeAfterCheck:       cmd.GetSetting[time.Duration](cmd.DurationBeforeMergeAfterCheckSetting),
		DurationToWaitAfterUpdateBranch:     cmd.GetSetting[time.Duration](cmd.DurationToWaitAfterUpdateBranchSetting),
		MessageChannelSizePerSubjectSetting: cmd.GetSetting[int](cmd.MessageChannelSizePerSubjectSetting),

		HTTPClient: http.DefaultClient,

		AppID:      appID,
		PrivateKey: privateKeyBytes,
	}

	errChan := make(chan error)
	go func() {
		logger.Info().Msg("worker started")
		errChan <- w.Consume()
	}()

	select {
	case <-ctx.Done():
		logger.Info().Msg("shutting down")
		_ = w.Shutdown(context.Background())
	case err := <-errChan:
		if err != nil {
			logger.Error().Err(err).Msg("unable to consume")
		}
	}
}
