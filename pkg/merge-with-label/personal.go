package merge_with_label

import (
	"context"
	"net/http"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

func PersonalMode(ctx context.Context, logger *zerolog.Logger, client *http.Client, repo *Repository, token string) error {

	// todo merge logic
	// todo ticker
	// update logic
	pullRequests, err := GetPullRequestsThatNeedToBeUpdated(ctx, client, token, repo)
	if err != nil {
		return errors.Wrap(err, "unable to get pull requests")
	}

	for _, pr := range pullRequests {
		logger.Info().Int("pr", pr).Msg("updating pr")
		if err := UpdatePullRequest(ctx, client, token, repo, pr, 0); err != nil {
			logger.Error().Int("pr", pr).Err(err).Msg("unable to update pull request")
		}
	}

	// merge logic
	return nil
}
