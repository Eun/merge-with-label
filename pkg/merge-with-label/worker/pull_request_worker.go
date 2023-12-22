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

	sess, err := worker.getSession(ctx, &logger, &msg.BaseMessage)
	if err != nil {
		return errors.Wrap(err, "unable to get session")
	}
	if sess == nil {
		return nil
	}

	details, err := github.GetPullRequestDetails(ctx, worker.HTTPClient, sess.AccessToken, &msg.Repository, msg.PullRequest.Number)
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
	stopLogic, didUpdatePullRequest, err := worker.updatePullRequest(ctx, &logger, sess, details)
	if err != nil {
		return errors.WithStack(err)
	}
	if stopLogic {
		return nil
	}

	if didUpdatePullRequest && sess.Config.Merge.Labels.ContainsOneOf(details.Labels...) != "" {
		logger.Debug().Msg("not merging, because pull request was just updated")
		return pushBackError{delay: worker.DurationToWaitAfterUpdateBranch}
	}

	stopLogic, didMergePullRequest, err := worker.mergePullRequest(
		ctx,
		&logger,
		sess,
		msg.PullRequest.Number,
		details,
	)
	if err != nil {
		logger.Error().Err(err).Msg("merge pull request failed")
		return errors.WithStack(err)
	}
	if stopLogic {
		return nil
	}

	if didMergePullRequest && sess.Config.Merge.DeleteBranch {
		logger.Info().Str("branch", details.HeadRefName).Msg("deleting branch")
		if err := github.DeleteRef(ctx, worker.HTTPClient, sess.AccessToken, details.HeadRefID); err != nil {
			return errors.New("unable to delete branch")
		}
	}
	return nil
}

func (worker *pullRequestWorker) updatePullRequest(
	ctx context.Context,
	rootLogger *zerolog.Logger,
	sess *session,
	details *github.PullRequestDetails,
) (stopLogic, didUpdatePullRequest bool, err error) {
	if len(sess.Config.Update.Labels) == 0 {
		return false, false, nil
	}

	if sess.Config.Update.Labels.ContainsOneOf(details.Labels...) == "" {
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
			sess,
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

	result, err := worker.shouldSkipUpdate(ctx, rootLogger, sess.Config, details)
	if err != nil {
		return false, false, errors.WithStack(err)
	}
	if result.SkipAction {
		if err := worker.CreateOrUpdateCheckRun(
			ctx,
			rootLogger,
			sess,
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
		sess,
		details.ID,
		details.LastCommitSha,
		"COMPLETED",
		"updating",
		"",
	); err != nil {
		return false, false, errors.WithStack(err)
	}
	if err := github.UpdatePullRequest(ctx, worker.HTTPClient, sess.AccessToken, details.ID, details.LastCommitSha); err != nil {
		var graphQLErrors github.GraphQLErrors
		if errors.As(err, &graphQLErrors) {
			if err := worker.CreateOrUpdateCheckRun(
				ctx,
				rootLogger,
				sess,
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
		sess,
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
	sess *session,
	number int64,
	details *github.PullRequestDetails,
) (stopLogic, didMerge bool, err error) {
	if sess.Config.Merge.Labels.ContainsOneOf(details.Labels...) == "" {
		return false, false, nil
	}

	result, err := worker.shouldSkipMerge(ctx, rootLogger, sess.Config, details)
	if err != nil {
		return false, false, errors.WithStack(err)
	}
	if result.SkipAction {
		if err := worker.CreateOrUpdateCheckRun(
			ctx,
			rootLogger,
			sess,
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
		sess,
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
		sess.AccessToken,
		details.ID,
		details.LastCommitSha,
		sess.Config.Merge.Strategy.GithubString(),
		fmt.Sprintf("%s (#%d)", details.Title, number),
	); err != nil {
		var graphQLErrors github.GraphQLErrors
		if errors.As(err, &graphQLErrors) {
			if err := worker.CreateOrUpdateCheckRun(
				ctx,
				rootLogger,
				sess,
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
