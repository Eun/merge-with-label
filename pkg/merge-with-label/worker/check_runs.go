package worker

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
)

const kvBucketCheckRuns = "check_runs"

func (worker *Worker) CreateOrUpdateCheckRun(
	ctx context.Context,
	rootLogger *zerolog.Logger,
	sess *session,
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
	value, err := worker.Store.KVGet(ctx, kvBucketCheckRuns, key)
	if err != nil {
		return errors.Wrap(err, "unable to get check_run_id from store")
	}

	if len(value) == 0 {
		logger.Debug().Msg("creating a new check run")
		checkRunID, err := github.CreateCheckRun(
			ctx,
			worker.HTTPClient,
			sess.AccessToken,
			sess.Repository,
			sha,
			status,
			worker.BotName,
			title,
			summary,
		)
		if err != nil {
			return errors.Wrap(err, "error creating check run")
		}
		if err := worker.Store.KVSet(ctx, kvBucketCheckRuns, key, []byte(checkRunID), 10*time.Minute); err != nil {
			return errors.Wrap(err, "unable to store check_run_id in store")
		}
		return nil
	}

	checkRunID, err := github.UpdateCheckRun(
		ctx,
		worker.HTTPClient,
		sess.AccessToken,
		sess.Repository,
		string(value),
		status,
		worker.BotName,
		title,
		summary,
	)
	if err != nil {
		return errors.Wrap(err, "error updating check run")
	}
	if err := worker.Store.KVSet(ctx, kvBucketCheckRuns, key, []byte(checkRunID), 10*time.Minute); err != nil {
		return errors.Wrap(err, "unable to store check_run_id in store")
	}
	return nil
}
