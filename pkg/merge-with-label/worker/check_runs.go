package worker

import (
	"context"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

func (worker *Worker) CreateOrUpdateCheckRun(
	ctx context.Context,
	rootLogger *zerolog.Logger,
	accessToken string,
	repository *common.Repository,
	pullRequestNodeID,
	sha,
	status,
	title,
	summary string,
) error {
	if sha == "" {
		return nil
	}

	logger := rootLogger.With().
		Str("sha", sha).
		Logger()

	key := hashForKV(pullRequestNodeID + sha)
	entry, err := worker.CheckRunsKV.Get(key)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return errors.Wrap(err, "unable to get check_run_id from kv bucket")
	}
	if entry == nil || len(entry.Value()) == 0 || errors.Is(err, nats.ErrKeyNotFound) {
		logger.Debug().
			Msg("creating a new check run")
		checkRunID, err := github.CreateCheckRun(
			ctx,
			worker.HTTPClient,
			accessToken,
			repository,
			sha,
			status,
			worker.BotName,
			title,
			summary,
		)
		if err != nil {
			return errors.Wrap(err, "error creating check run")
		}
		if _, err := worker.CheckRunsKV.PutString(key, checkRunID); err != nil {
			return errors.Wrap(err, "unable to store check_run_id in kv bucket")
		}
		return nil
	}

	checkRunID, err := github.UpdateCheckRun(
		ctx,
		worker.HTTPClient,
		accessToken,
		repository,
		string(entry.Value()),
		status,
		worker.BotName,
		title,
		summary,
	)
	if err != nil {
		return errors.Wrap(err, "error updating check run")
	}
	if _, err := worker.CheckRunsKV.PutString(key, checkRunID); err != nil {
		return errors.Wrap(err, "unable to store check_run_id in kv bucket")
	}
	return nil
}
