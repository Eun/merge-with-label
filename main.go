package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	merge_with_label "github.com/Eun/merge-with-label/pkg/merge-with-label"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if personalFile := os.Getenv("PERSONAL_MODE"); personalFile != "" {
		log.Info().Msg("running in personal mode")
		data, err := os.ReadFile(personalFile)
		if err != nil {
			log.Error().Err(err).Msgf("unable to read `%s' file", personalFile)
			return
		}

		var personalData []struct {
			Repo   string `yaml:"repo"`
			Branch string `yaml:"branch"`
		}

		if err := yaml.Unmarshal(data, &personalData); err != nil {
			log.Error().Err(err).Msgf("unable to read `%s' file", personalFile)
			return
		}

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		for i := range personalData {
			parts := strings.SplitN(personalData[i].Repo, "/", 2)
			if len(parts) != 2 {
				log.Warn().Str("repo", personalData[i].Repo).Msg("invalid format -> ignored")
				continue
			}
			if personalData[i].Branch == "" {
				log.Warn().Str("repo", personalData[i].Repo).Msg("no branch present -> ignored")
				continue
			}

			repo := merge_with_label.Repository{
				FullName:     personalData[i].Repo,
				MasterBranch: personalData[i].Branch,
				Name:         parts[1],
				Owner: struct {
					Name string `json:"name"`
				}{
					Name: parts[0],
				},
			}

			logger := log.Logger.With().Str("repo", personalData[i].Repo).Logger()
			go func() {
				err := merge_with_label.PersonalMode(
					ctx,
					&logger,
					http.DefaultClient,
					&repo,
					os.Getenv("GITHUB_TOKEN"),
				)
				if err != nil {
					logger.Error().Err(err).Msg("error during personal mode")
				}
			}()
		}

		<-ctx.Done()
		return
	}

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
