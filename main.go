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
	"golang.org/x/exp/slog"
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
		slog.Error("APP_ID is not set")
		return
	}

	appID, err := strconv.ParseInt(os.Getenv("APP_ID"), 10, 64)
	if err != nil {
		slog.Error("unable to get APP_ID", "error", err)
		return
	}

	privateKeyFile := os.Getenv("PRIVATE_KEY")
	if privateKeyFile == "" {
		slog.Error("PRIVATE_KEY is not set")
		return
	}
	privateKeyBytes, err := os.ReadFile(privateKeyFile)
	if err != nil {
		slog.Error("unable to read private key", "file", privateKeyFile)
		return
	}

	server := http.Server{
		Addr:              address,
		ReadTimeout:       1 * time.Second,
		WriteTimeout:      1 * time.Second,
		IdleTimeout:       30 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		Handler: &merge_with_label.Handler{
			GetLoggerForContext: func(ctx context.Context) *slog.Logger {
				return slog.Default()
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
			slog.Error("unable to listen", "address", address, "error", err)
			return
		}
	}()

	<-ctx.Done()
	_ = server.Shutdown(ctx)
}
