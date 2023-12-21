package worker

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
)

type pullRequestWorker struct {
	*Worker
}

func (worker *pullRequestWorker) runLogic(rootLogger *zerolog.Logger, msg *common.QueuePullRequestMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), worker.MaxDurationForPullRequestWorker)
	defer cancel()
	logger := rootLogger.With().
		Str("entry", "pull_request").
		Int64("number", msg.PullRequest.Number).
		Str("repo", msg.Repository.FullName).
		Logger()

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

	if len(cfg.Merge.Labels) == 0 && len(cfg.Update.Labels) == 0 {
		logger.Debug().Msg("merge and update are disabled")
		return nil
	}

	details, err := github.GetPullRequestDetails(ctx, worker.HTTPClient, accessToken, &msg.Repository, msg.PullRequest.Number)
	if err != nil {
		return errors.Wrap(err, "error getting pull request details")
	}

	if details.State != "OPEN" {
		logger.Debug().Msg("pull request is not open anymore")
		return nil
	}

	if details.LastCommitTime.IsZero() || details.LastCommitSha == "" {
		logger.Debug().Msg("pull request did not contain commits")
		return nil
	}

	// update logic
	stopLogic, didUpdatePullRequest, err := worker.updatePullRequest(ctx, &logger, cfg, &msg.Repository, details, accessToken)
	if err != nil {
		return errors.WithStack(err)
	}
	if stopLogic {
		return nil
	}

	if didUpdatePullRequest && cfg.Merge.Labels.ContainsOneOf(details.Labels...) != "" {
		logger.Debug().Msg("not merging, because pull request was just updated")
		return pushBackError{delay: worker.DurationToWaitAfterUpdateBranch}
	}

	stopLogic, didMergePullRequest, err := worker.mergePullRequest(
		ctx,
		&logger,
		cfg,
		&msg.Repository,
		msg.PullRequest.Number,
		details,
		accessToken,
	)
	if err != nil {
		logger.Error().Err(err).Msg("merge pull request failed")
		return errors.WithStack(err)
	}
	if stopLogic {
		return nil
	}

	if didMergePullRequest && cfg.Merge.DeleteBranch {
		logger.Info().Str("branch", details.HeadRefName).Msg("deleting branch")
		if err := github.DeleteRef(ctx, worker.HTTPClient, accessToken, details.HeadRefID); err != nil {
			return errors.New("unable to delete branch")
		}
	}
	return nil
}

func (worker *pullRequestWorker) updatePullRequest(
	ctx context.Context,
	rootLogger *zerolog.Logger,
	cfg *ConfigV1,
	repository *common.Repository,
	details *github.PullRequestDetails,
	accessToken string) (stopLogic, didUpdatePullRequest bool, err error) {
	if len(cfg.Update.Labels) == 0 {
		return false, false, nil
	}

	if cfg.Update.Labels.ContainsOneOf(details.Labels...) == "" {
		return false, false, nil
	}

	if details.AheadBy == 0 {
		return false, false, nil
	}

	if details.HasConflicts {
		rootLogger.Info().Msg("not updating: pull request has conflicts")
		if err := worker.CreateOrUpdateCheckRun(
			ctx,
			rootLogger,
			accessToken,
			repository,
			details.ID,
			details.LastCommitSha,
			"COMPLETED",
			"not updating: pull request has conflicts",
			"",
		); err != nil {
			return false, false, errors.WithStack(err)
		}
		return true, false, nil
	}

	result, err := worker.shouldSkipUpdate(ctx, rootLogger, cfg, details)
	if err != nil {
		return false, false, errors.WithStack(err)
	}
	if result.SkipAction {
		if err := worker.CreateOrUpdateCheckRun(
			ctx,
			rootLogger,
			accessToken,
			repository,
			details.ID,
			details.LastCommitSha,
			"COMPLETED",
			result.Title,
			result.Summary,
		); err != nil {
			return false, false, errors.WithStack(err)
		}
		return true, false, nil
	}

	rootLogger.Info().Msg("updating pull request")
	if err := worker.CreateOrUpdateCheckRun(
		ctx,
		rootLogger,
		accessToken,
		repository,
		details.ID,
		details.LastCommitSha,
		"COMPLETED",
		"updating",
		"",
	); err != nil {
		return false, false, errors.WithStack(err)
	}
	if err := github.UpdatePullRequest(ctx, worker.HTTPClient, accessToken, details.ID, details.LastCommitSha); err != nil {
		var graphQLErrors github.GraphQLErrors
		if errors.As(err, &graphQLErrors) {
			if err := worker.CreateOrUpdateCheckRun(
				ctx,
				rootLogger,
				accessToken,
				repository,
				details.ID,
				details.LastCommitSha,
				"COMPLETED",
				"error during update",
				graphQLErrors.GetMessages(),
			); err != nil {
				return false, false, errors.WithStack(err)
			}
		}
		return false, false, errors.Wrap(err, "error updating pull request")
	}

	if err := worker.CreateOrUpdateCheckRun(
		ctx,
		rootLogger,
		accessToken,
		repository,
		details.ID,
		details.LastCommitSha,
		"COMPLETED",
		"updated",
		"",
	); err != nil {
		return false, false, errors.WithStack(err)
	}
	return false, true, nil
}

func (worker *pullRequestWorker) mergePullRequest(
	ctx context.Context,
	rootLogger *zerolog.Logger,
	cfg *ConfigV1,
	repository *common.Repository,
	number int64,
	details *github.PullRequestDetails,
	accessToken string,
) (stopLogic, didMerge bool, err error) {
	if cfg.Merge.Labels.ContainsOneOf(details.Labels...) == "" {
		return false, false, nil
	}

	result, err := worker.shouldSkipMerge(ctx, rootLogger, cfg, details)
	if err != nil {
		return false, false, errors.WithStack(err)
	}
	if result.SkipAction {
		if err := worker.CreateOrUpdateCheckRun(
			ctx,
			rootLogger,
			accessToken,
			repository,
			details.ID,
			details.LastCommitSha,
			"COMPLETED",
			result.Title,
			result.Summary,
		); err != nil {
			return false, false, errors.WithStack(err)
		}
		return true, false, nil
	}

	rootLogger.Info().Msg("merging pull request")
	if err := worker.CreateOrUpdateCheckRun(
		ctx,
		rootLogger,
		accessToken,
		repository,
		details.ID,
		details.LastCommitSha,
		"COMPLETED",
		fmt.Sprintf("merging %s into %s", details.HeadRefName, details.BaseRefName),
		"",
	); err != nil {
		return false, false, errors.WithStack(err)
	}

	if err := github.MergePullRequest(
		ctx,
		worker.HTTPClient,
		accessToken,
		details.ID,
		details.LastCommitSha,
		cfg.Merge.Strategy.GithubString(),
		fmt.Sprintf("%s (#%d)", details.Title, number),
	); err != nil {
		var graphQLErrors github.GraphQLErrors
		if errors.As(err, &graphQLErrors) {
			if err := worker.CreateOrUpdateCheckRun(
				ctx,
				rootLogger,
				accessToken,
				repository,
				details.ID,
				details.LastCommitSha,
				"COMPLETED",
				"error during merge",
				graphQLErrors.GetMessages(),
			); err != nil {
				return false, false, errors.WithStack(err)
			}
		}
		return false, false, errors.Wrap(err, "unable to merge pull request")
	}
	return false, true, nil
}
