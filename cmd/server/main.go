package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"

	"github.com/Eun/merge-with-label/cmd"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/pgqueue"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/server"
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

	address := os.Getenv("ADDRESS")
	if address == "" {
		address = ":" + os.Getenv("PORT")
	}
	if address == ":" {
		address = ":8000"
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

	logger.Info().Msg("running database migrations")
	if err := store.Migrate(ctx); err != nil {
		logger.Error().Err(err).Msg("unable to run database migrations")
		return
	}
	logger.Info().Msg("database migrations complete")

	store.StartKVCleaner(ctx)
	logger.Debug().Msg("kv cleaner started")

	srv := http.Server{
		Addr:              address,
		ReadTimeout:       1 * time.Second,
		WriteTimeout:      1 * time.Second,
		IdleTimeout:       30 * time.Second, //nolint:mnd // set IdleTimeout
		ReadHeaderTimeout: 2 * time.Second,  //nolint:mnd // set ReadHeaderTimeout
		Handler: &server.Handler{
			GetLoggerForContext: func(_ context.Context) *zerolog.Logger {
				return &logger
			},
			AllowedRepositories:         cmd.GetSetting[common.RegexSlice](cmd.AllowedRepositoriesSetting),
			AllowOnlyPublicRepositories: cmd.GetSetting[bool](cmd.AllowOnlyPublicRepositories),

			Store:             store,
			RateLimitInterval: cmd.GetSetting[time.Duration](cmd.RateLimitIntervalSetting),
		},
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	errChan := make(chan error)
	go func() {
		logger.Info().Msgf("listening on %s", address)
		errChan <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info().Msg("shutting down")
		_ = srv.Shutdown(context.Background())
	case err := <-errChan:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error().Err(err).Msgf("unable to listen on address %s", address)
		}
	}
}
