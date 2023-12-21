package worker

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
)

type pushWorker struct {
	*Worker
}

func (worker *pushWorker) runLogic(rootLogger *zerolog.Logger, msg *common.QueuePushMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), worker.MaxDurationForPushWorker)
	defer cancel()
	logger := rootLogger.With().Str("entry", "push").Str("repo", msg.Repository.FullName).Logger()

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
				BaseMessage: common.BaseMessage{
					InstallationID: msg.InstallationID,
					Repository:     msg.Repository,
				},
				PullRequest: pullRequests[i],
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
