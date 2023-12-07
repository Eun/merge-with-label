package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"

	"github.com/Eun/merge-with-label/cmd"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
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

	currentStreamConfig := &nats.StreamConfig{
		Name: cmd.GetSetting[string](cmd.StreamNameSetting),
		Subjects: []string{
			cmd.GetSetting[string](cmd.PullRequestSubjectSetting) + ".>",
			cmd.GetSetting[string](cmd.PushSubjectSetting) + ".>",
		},
		Retention: nats.WorkQueuePolicy,
		MaxAge:    cmd.GetSetting[time.Duration](cmd.MaxMessageAgeSetting),
	}

	logger.Debug().Msg("getting js info")
	info, err := js.StreamInfo(currentStreamConfig.Name)
	if err != nil && !errors.Is(err, nats.ErrStreamNotFound) {
		logger.Error().Err(err).Str("nats_url", natsURL).Msg("unable to get stream")
		return
	}
	if info != nil {
		logger.Debug().Msg("updating js stream")
		if _, err := js.UpdateStream(currentStreamConfig); err != nil {
			logger.Error().Err(err).Str("nats_url", natsURL).Msg("unable to update stream")
			return
		}
	} else {
		logger.Debug().Msg("adding js stream")
		_, err = js.AddStream(currentStreamConfig)
		if err != nil {
			logger.Error().Err(err).Str("nats_url", natsURL).Msg("unable to add stream")
			return
		}
	}
	logger.Debug().Msg("js stream is ready")

	logger.Debug().Msg("creating ratelimit kv")
	rateLimitKV, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket: cmd.GetSetting[string](cmd.RateLimitBucketNameSetting),
		TTL:    cmd.GetSetting[time.Duration](cmd.RateLimitBucketTTLSetting),
	})
	if err != nil {
		logger.Error().
			Err(err).
			Str("nats_url", natsURL).
			Msg("unable to create jetstream key value bucket for push rate limit")
		return
	}
	logger.Debug().Msg("configured ratelimit kv")

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
			AllowedRepositories:         cmd.GetSetting[common.RegexSlice](cmd.AllowedRepositoriesSetting),
			AllowOnlyPublicRepositories: cmd.GetSetting[bool](cmd.AllowOnlyPublicRepositories),

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
		logger.Info().Msgf("listening on %s", address)
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
