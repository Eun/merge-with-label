package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/Eun/merge-with-label/cmd"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/worker"
	"github.com/nats-io/nats.go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"
)

func main() {
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()
	if os.Getenv("DEBUG") != "" {
		logger = logger.Level(zerolog.DebugLevel)
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

	nc, err := nats.Connect(os.Getenv("NATS_URL"))
	if err != nil {
		logger.Error().
			Err(err).
			Str("nats_url", os.Getenv("NATS_URL")).
			Msg("unable to connect to nats")
		return
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		logger.Error().
			Err(err).
			Str("nats_url", os.Getenv("NATS_URL")).
			Msg("unable to create jetstream context")
		return
	}
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
