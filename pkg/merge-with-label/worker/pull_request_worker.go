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

func (worker *pullRequestWorker) runLogic(rootLogger *zerolog.Logger, msg *common.QueuePRMessage) error {
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

	// Compute a fingerprint of all fields that drive the merge/update decision.
	// This fingerprint is the dedup key: two runs that see identical GitHub state
	// make identical decisions, so the second is a pure duplicate and can be
	// skipped. If anything changed (new approval, check pass, label change,
	// commit, base-branch advance) the hash differs and the run proceeds.
	//
	// Writing the hash before doing work also blocks a concurrent goroutine that
	// races on the same PR: since the queue row is deleted on dequeue the dedup
	// key is free, so a new event can enqueue a fresh row before the first run
	// finishes. That second goroutine fetches the same GitHub state, computes the
	// same hash, and skips — exactly like a duplicate.
	hash := prStateHash(details)

	lastHash, err := worker.Store.GetPRStateHash(ctx, msg.Repository.NodeID, msg.PullRequest.Number)
	if err != nil {
		return errors.Wrap(err, "unable to get pr state hash")
	}
	if lastHash == hash {
		logger.Debug().
			Str("hash", hash).
			Msg("pr decision state unchanged since last run, skipping")
		return nil
	}

	if err := worker.Store.SetPRStateHash(ctx, msg.Repository.NodeID, msg.PullRequest.Number, hash); err != nil {
		return errors.Wrap(err, "unable to set pr state hash")
	}

	logger.Info().Str("sha", details.LastCommitSha).Int("ahead_by", details.AheadBy).Msg("processing pull request")

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

	stopLogic, didMergePullRequest, err := worker.mergePullRequest(ctx, &logger, sess, msg.PullRequest.Number, details)
	if err != nil {
		logger.Error().Err(err).Msg("merge pull request failed")
		return errors.WithStack(err)
	}
	if stopLogic {
		return nil
	}

	if didMergePullRequest {
		if sess.Config.Merge.DeleteBranch {
			logger.Info().Str("branch", details.HeadRefName).Msg("deleting branch")
			if err := github.DeleteRef(ctx, worker.HTTPClient, sess.AccessToken, details.HeadRefID); err != nil {
				return errors.New("unable to delete branch")
			}
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
		if err := worker.CreateOrUpdateCheckRun(ctx, rootLogger, sess, details.ID, details.LastCommitSha,
			"COMPLETED", "not updating: pull request has conflicts", ""); err != nil {
			return false, false, errors.WithStack(err)
		}
		return true, false, nil
	}

	result, err := worker.shouldSkipUpdate(ctx, rootLogger, sess.Config, details)
	if err != nil {
		return false, false, errors.WithStack(err)
	}
	if result.SkipAction {
		if err := worker.CreateOrUpdateCheckRun(ctx, rootLogger, sess, details.ID, details.LastCommitSha,
			"COMPLETED", result.Title, result.Summary); err != nil {
			return false, false, errors.WithStack(err)
		}
		return true, false, nil
	}

	rootLogger.Info().Msg("updating pull request")
	if err := worker.CreateOrUpdateCheckRun(ctx, rootLogger, sess, details.ID, details.LastCommitSha,
		"COMPLETED", "updating", ""); err != nil {
		return false, false, errors.WithStack(err)
	}
	if err := github.UpdatePullRequest(ctx, worker.HTTPClient, sess.AccessToken, details.ID, details.LastCommitSha); err != nil {
		var graphQLErrors github.GraphQLErrors
		if errors.As(err, &graphQLErrors) {
			if err := worker.CreateOrUpdateCheckRun(ctx, rootLogger, sess, details.ID, details.LastCommitSha,
				"COMPLETED", "error during update", graphQLErrors.GetMessages()); err != nil {
				return false, false, errors.WithStack(err)
			}
		}
		return false, false, errors.Wrap(err, "error updating pull request")
	}
	if err := worker.CreateOrUpdateCheckRun(ctx, rootLogger, sess, details.ID, details.LastCommitSha,
		"COMPLETED", "updated", ""); err != nil {
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
		if err := worker.CreateOrUpdateCheckRun(ctx, rootLogger, sess, details.ID, details.LastCommitSha,
			"COMPLETED", result.Title, result.Summary); err != nil {
			return false, false, errors.WithStack(err)
		}
		return true, false, nil
	}

	rootLogger.Info().Msg("merging pull request")
	if err := worker.CreateOrUpdateCheckRun(ctx, rootLogger, sess, details.ID, details.LastCommitSha,
		"COMPLETED", fmt.Sprintf("merging %s into %s", details.HeadRefName, details.BaseRefName), ""); err != nil {
		return false, false, errors.WithStack(err)
	}

	if err := github.MergePullRequest(ctx, worker.HTTPClient, sess.AccessToken, details.ID, details.LastCommitSha,
		sess.Config.Merge.Strategy.GithubString(),
		fmt.Sprintf("%s (#%d)", details.Title, number),
	); err != nil {
		var graphQLErrors github.GraphQLErrors
		if errors.As(err, &graphQLErrors) {
			if err := worker.CreateOrUpdateCheckRun(ctx, rootLogger, sess, details.ID, details.LastCommitSha,
				"COMPLETED", "error during merge", graphQLErrors.GetMessages()); err != nil {
				return false, false, errors.WithStack(err)
			}
		}
		return false, false, errors.Wrap(err, "unable to merge pull request")
	}
	return false, true, nil
}
