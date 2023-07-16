package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/Eun/merge-with-label/cmd"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/server"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
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

	address := os.Getenv("ADDRESS")
	if address == "" {
		address = ":" + os.Getenv("PORT")
	}

	if address == ":" {
		address = ":8000"
	}

	nc, err := nats.Connect(os.Getenv("NATS_URL"))
	if err != nil {
		logger.Error().Str("nats_url", os.Getenv("NATS_URL")).Msg("unable to connect to nats")
		return
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		logger.Error().Str("nats_url", os.Getenv("NATS_URL")).Msg("unable to create jetstream context")
		return
	}

	currentStreamConfig := &nats.StreamConfig{
		Name: cmd.GetSetting[string](cmd.StreamNameSetting),
		Subjects: []string{
			cmd.GetSetting[string](cmd.PullRequestSubjectSetting) + ".>",
			cmd.GetSetting[string](cmd.PushSubjectSetting) + ".>",
		},
		Retention: nats.WorkQueuePolicy,
		MaxAge:    cmd.GetSetting[time.Duration](cmd.MaxMessageAgeSetting),
	}

	info, err := js.StreamInfo(currentStreamConfig.Name)
	if err != nil && !errors.Is(err, nats.ErrStreamNotFound) {
		logger.Error().Err(err).Str("nats_url", os.Getenv("NATS_URL")).Msg("unable to get stream")
		return
	}
	if info != nil {
		if _, err := js.UpdateStream(currentStreamConfig); err != nil {
			logger.Error().Err(err).Str("nats_url", os.Getenv("NATS_URL")).Msg("unable to update stream")
			return
		}
	} else {
		_, err = js.AddStream(currentStreamConfig)
		if err != nil {
			logger.Error().Err(err).Str("nats_url", os.Getenv("NATS_URL")).Msg("unable to add stream")
			return
		}
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

	srv := http.Server{
		Addr:              address,
		ReadTimeout:       1 * time.Second,
		WriteTimeout:      1 * time.Second,
		IdleTimeout:       30 * time.Second, //nolint:gomnd // set IdleTimeout
		ReadHeaderTimeout: 2 * time.Second,  //nolint:gomnd // set ReadHeaderTimeout
		Handler: &server.Handler{
			GetLoggerForContext: func(ctx context.Context) *zerolog.Logger {
				return &logger
			},
			AllowedRepositories: cmd.GetSetting[common.RegexSlice](cmd.AllowedRepositoriesSetting),

			JetStreamContext:   js,
			PullRequestSubject: cmd.GetSetting[string](cmd.PullRequestSubjectSetting),
			PushSubject:        cmd.GetSetting[string](cmd.PushSubjectSetting),

			RateLimitKV:       rateLimitKV,
			RateLimitInterval: cmd.GetSetting[time.Duration](cmd.RateLimitIntervalSetting),
		},
		BaseContext: func(listener net.Listener) context.Context {
			return ctx
		},
	}

	errChan := make(chan error)
	go func() {
		errChan <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info().Msg("shutting down")
		_ = srv.Shutdown(context.Background())
	case err := <-errChan:
		if err != nil {
			logger.Error().Err(err).Msgf("unable to listen on address %s", address)
		}
	}
}
