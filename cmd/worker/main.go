package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"

	"github.com/Eun/merge-with-label/cmd"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/worker"
)

func main() {
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	logger := zerolog.New(os.Stderr).Level(zerolog.InfoLevel).With().Timestamp().Logger()
	if os.Getenv("DEBUG") != "" {
		logger = logger.Level(zerolog.DebugLevel)
		logger.Debug().Msg("debug logging enabled")
	}
	if os.Getenv("TRACE") != "" {
		logger = logger.Level(zerolog.TraceLevel)
		logger.Debug().Msg("trace logging enabled")
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
	privateKeyBytes, err := os.ReadFile(privateKeyFile) //nolint:gosec // G703: path is from env, not user input
	if err != nil {
		logger.Error().
			Err(err).
			Str("file", privateKeyFile).
			Msg("unable to read private key")
		return
	}

	dsnEnv := os.Getenv("PostgresDSN")
	if dsnEnv == "" {
		dsnEnv = cmd.GetSetting[string](cmd.PostgresDSNSetting)
	}

	logger.Debug().Msg("connecting to postgres")
	store, err := pgqueue.New(ctx, dsnEnv)
	if err != nil {
		logger.Error().Err(err).Msg("unable to connect to postgres")
		return
	}
	defer store.Close()
	logger.Debug().Msg("postgres ready")

	w := worker.Worker{
		Logger:  &logger,
		BotName: cmd.GetSetting[string](cmd.BotNameSetting),

		AllowedRepositories:         cmd.GetSetting[common.RegexSlice](cmd.AllowedRepositoriesSetting),
		AllowOnlyPublicRepositories: cmd.GetSetting[bool](cmd.AllowOnlyPublicRepositories),

		Store: store,

		RetryWait:         cmd.GetSetting[time.Duration](cmd.MessageRetryWaitSetting),
		MaxAttempts:       cmd.GetSetting[int](cmd.MessageRetryAttemptsSetting),
		MaxConcurrentJobs: cmd.GetSetting[int](cmd.MaxConcurrentJobsSetting),

		MaxDurationForRepoWorker:        time.Minute,
		MaxDurationForPullRequestWorker: time.Minute,

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
