package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
)

func (worker *Worker) getAccessToken(
	ctx context.Context,
	rootLogger *zerolog.Logger,
	repository *common.Repository,
	installationID int64,
) (string, error) {
	key := hashForKV(repository.FullName)

	logger := rootLogger.With().
		Str("hash_key", key).
		Logger()

	entry, err := worker.AccessTokensKV.Get(key)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return "", errors.Wrap(err, "unable to get access token from kv bucket")
	}
	if entry == nil || len(entry.Value()) == 0 || errors.Is(err, nats.ErrKeyNotFound) {
		logger.Debug().
			Str("reason", "not in cache").
			Msg("creating a new access token")
		return worker.createNewAccessToken(
			ctx,
			&logger,
			repository,
			installationID,
			key,
		)
	}

	var cachedToken github.AccessToken
	if err := json.Unmarshal(entry.Value(), &cachedToken); err != nil {
		return "", errors.Wrap(err, "unable to decode access token from kv bucket")
	}

	if cachedToken.ExpiresAt.Before(time.Now()) {
		logger.Debug().
			Str("reason", "expired").
			Msg("creating a new access token")
		return worker.createNewAccessToken(
			ctx,
			&logger,
			repository,
			installationID,
			key,
		)
	}

	logger.Debug().
		Msg("got access token from cache")
	return cachedToken.Token, nil
}

func (worker *Worker) createNewAccessToken(
	ctx context.Context,
	rootLogger *zerolog.Logger,
	repository *common.Repository,
	installationID int64,
	key string,
) (string, error) {
	rootLogger.Debug().Msg("getting access_token from github")
	accessToken, err := github.GetAccessToken(ctx, worker.HTTPClient, worker.AppID, worker.PrivateKey, repository, installationID)
	if err != nil {
		return "", errors.Wrap(err, "unable to get access token")
	}

	buf, err := json.Marshal(accessToken)
	if err != nil {
		return "", errors.Wrap(err, "unable to encode access token")
	}

	rootLogger.Debug().Msg("storing access_token in cache")
	if _, err := worker.AccessTokensKV.Put(key, buf); err != nil {
		return "", errors.Wrap(err, "unable to store access token in kv bucket")
	}
	return accessToken.Token, nil
}
