package worker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/hashicorp/go-multierror"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
)

type statusWorker struct {
	*Worker
}

func (worker *statusWorker) handleMessage(logger *zerolog.Logger, msg *nats.Msg) {
	if common.DelayMessageIfNeeded(logger, msg) {
		return
	}

	var m common.QueueStatusMessage
	if err := json.Unmarshal(msg.Data, &m); err != nil {
		logger.Error().Err(err).Msg("unable to decode queue message")
		if err := msg.NakWithDelay(worker.RetryWait); err != nil {
			logger.Error().Err(err).Msg("unable to nak message")
		}
		return
	}

	if worker.AllowOnlyPublicRepositories && m.Repository.Private {
		logger.Warn().Str("repo", m.Repository.FullName).Msg("repository is not allowed (it is private)")
		if err := msg.Ack(); err != nil {
			logger.Error().Err(err).Msg("unable to ack message")
		}
		return
	}

	if worker.AllowedRepositories.ContainsOneOf(m.Repository.FullName) == "" {
		logger.Warn().Str("repo", m.Repository.FullName).Msg("repository is not allowed")
		if err := msg.Ack(); err != nil {
			logger.Error().Err(err).Msg("unable to ack message")
		}
		return
	}

	err := worker.runLogic(logger, &m)
	if err != nil {
		logger.Error().Err(err).Msg("error")
		if err := msg.NakWithDelay(worker.RetryWait); err != nil {
			logger.Error().Err(err).Msg("unable to nak message")
		}
		return
	}
	if err := msg.Ack(); err != nil {
		logger.Error().Err(err).Msg("unable to ack message")
	}
}

func (worker *statusWorker) runLogic(rootLogger *zerolog.Logger, msg *common.QueueStatusMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), worker.MaxDurationForPushWorker)
	defer cancel()
	logger := rootLogger.With().Str("entry", "status").Str("repo", msg.Repository.FullName).Logger()

	accessToken, err := worker.getAccessToken(ctx, &logger, &msg.Repository, msg.InstallationID)
	if err != nil {
		return errors.Wrap(err, "unable to get access token")
	}

	sha, err := github.GetLatestBaseCommitSha(ctx, worker.HTTPClient, accessToken, &msg.Repository)
	if err != nil {
		return errors.Wrap(err, "unable to get latest base commit sha")
	}
	if sha == "" {
		logger.Debug().Msg("latest commit sha is empty")
		return nil
	}

	cfg, err := worker.getConfig(ctx, &logger, accessToken, &msg.Repository, sha)
	if err != nil {
		return errors.Wrap(err, "unable to get config")
	}
	if cfg == nil {
		logger.Debug().Msg("no config")
		return nil
	}

	if len(cfg.Update.Labels) == 0 {
		logger.Debug().Msg("update is disabled")
		return nil
	}

	pullRequests, err := github.GetPullRequestsThatAreOpenAndHaveOneOfTheseLabels(
		ctx,
		worker.HTTPClient,
		accessToken,
		&msg.Repository,
		append(cfg.Update.Labels.Strings(), cfg.Merge.Labels.Strings()...),
	)
	if err != nil {
		return errors.Wrap(err, "error getting pull requests")
	}
	if len(pullRequests) == 0 {
		logger.Debug().Msg("no pull requests available that need action")
		return nil
	}

	var result error
	for i := range pullRequests {
		err = common.QueueMessage(
			&logger,
			worker.JetStreamContext,
			worker.RateLimitKV,
			worker.RateLimitInterval,
			worker.PullRequestSubject+"."+uuid.NewString(),
			fmt.Sprintf("pull_request.%d.%s.%d", msg.InstallationID, msg.Repository.NodeID, pullRequests[i].Number),
			&common.QueuePullRequestMessage{
				InstallationID: msg.InstallationID,
				PullRequest:    pullRequests[i],
				Repository:     msg.Repository,
			})
		if err != nil {
			logger.Error().Int64("number", pullRequests[i].Number).Err(err).
				Msg("unable to publish pull_request to queue")
			result = multierror.Append(result, errors.Wrap(err, "unable to publish pull_request to queue"))
			continue
		}
		logger.Debug().Int64("number", pullRequests[i].Number).
			Msg("published pull_request message")
	}
	return result
}
