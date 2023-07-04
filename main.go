package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	merge_with_label "github.com/Eun/merge-with-label/pkg/merge-with-label"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	address := os.Getenv("ADDRESS")
	if address == "" {
		address = ":" + os.Getenv("PORT")
	}

	if address == ":" {
		address = ":8000"
	}

	if os.Getenv("APP_ID") == "" {
		log.Error().Msg("APP_ID is not set")
		return
	}

	appID, err := strconv.ParseInt(os.Getenv("APP_ID"), 10, 64)
	if err != nil {
		log.Error().Err(err).Msg("unable to get APP_ID")
		return
	}

	privateKeyFile := os.Getenv("PRIVATE_KEY")
	if privateKeyFile == "" {
		log.Error().Msg("PRIVATE_KEY is not set")
		return
	}
	privateKeyBytes, err := os.ReadFile(privateKeyFile)
	if err != nil {
		log.Error().Str("file", privateKeyFile).Msg("unable to read private key")
		return
	}

	server := http.Server{
		Addr:              address,
		ReadTimeout:       1 * time.Second,
		WriteTimeout:      1 * time.Second,
		IdleTimeout:       30 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		Handler: &merge_with_label.Handler{
			GetLoggerForContext: func(ctx context.Context) *zerolog.Logger {
				return &log.Logger
			},
			HTTPClient: http.DefaultClient,
			AppID:      appID,
			PrivateKey: privateKeyBytes,
		},
		BaseContext: func(listener net.Listener) context.Context {
			return ctx
		},
	}

	go func() {
		if err := server.ListenAndServe(); err != nil {
			log.Err(err).Str("address", address).Msg("unable to listen")
			return
		}
	}()

	<-ctx.Done()
	_ = server.Shutdown(ctx)
}
