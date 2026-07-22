package main

import (
	"context"
	"fmt"
	"net"
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

	// --healthcheck mode: ping our own /healthz and exit 0/1.
	if len(os.Args) == 2 && os.Args[1] == "--healthcheck" {
		healthAddress := os.Getenv("HEALTH_ADDRESS")
		if healthAddress == "" {
			healthAddress = ":" + os.Getenv("HEALTH_PORT")
		}
		if healthAddress == ":" {
			healthAddress = ":8001"
		}
		//nolint:noctx,gosec // healthcheck: address is localhost + env-controlled port, not user input
		resp, err := http.Get("http://localhost" + healthAddress + "/healthz")
		if err != nil {
			fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
			os.Exit(1)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "healthcheck failed: status %d\n", resp.StatusCode)
			os.Exit(1)
		}
		os.Exit(0)
	}

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
	privateKeyBytes, err := os.ReadFile(privateKeyFile)
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
	if dsnEnv == "" {
		logger.Error().Msg("PostgresDSN is not set")
		return
	}

	logger.Debug().Msg("connecting to postgres")
	store, err := pgqueue.New(ctx, dsnEnv)
	if err != nil {
		logger.Error().Err(err).Msg("unable to connect to postgres")
		return
	}
	defer store.Close()
	logger.Debug().Msg("postgres connected")

	logger.Info().Msg("waiting for latest database schema")
	if err := store.WaitForSchema(ctx); err != nil {
		logger.Error().Err(err).Msg("unable to wait for database schema")
		return
	}
	logger.Info().Msg("database schema is up to date")

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

	// Start a minimal health HTTP server so the Docker health check can
	// confirm the worker process is alive without any external tools.
	healthAddress := os.Getenv("HEALTH_ADDRESS")
	if healthAddress == "" {
		healthAddress = ":" + os.Getenv("HEALTH_PORT")
	}
	if healthAddress == ":" {
		healthAddress = ":8001"
	}
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(hw http.ResponseWriter, _ *http.Request) {
		hw.WriteHeader(http.StatusOK)
	})
	healthSrv := &http.Server{
		Addr:              healthAddress,
		Handler:           healthMux,
		ReadTimeout:       1 * time.Second,
		WriteTimeout:      1 * time.Second,
		IdleTimeout:       30 * time.Second, //nolint:mnd // set IdleTimeout
		ReadHeaderTimeout: 2 * time.Second,  //nolint:mnd // set ReadHeaderTimeout
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}
	go func() {
		logger.Info().Msgf("health server listening on %s", healthAddress)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error().Err(err).Msg("health server error")
		}
	}()

	errChan := make(chan error)
	go func() {
		logger.Info().Msg("worker started")
		errChan <- w.Consume()
	}()

	select {
	case <-ctx.Done():
		logger.Info().Msg("shutting down")
		_ = healthSrv.Shutdown(context.Background())
		_ = w.Shutdown(context.Background())
	case err := <-errChan:
		if err != nil {
			logger.Error().Err(err).Msg("unable to consume")
		}
	}
}
