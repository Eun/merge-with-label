package worker

import (
	"context"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
)

// repoWorker handles MsgTypeRepo jobs (push, status, repo-level check_run).
// It fetches all eligible open PRs for the repo and fans them out as
// individual MsgTypePR jobs, each with their own dedup key.
type repoWorker struct {
	*Worker
}

func (worker *repoWorker) runLogic(rootLogger *zerolog.Logger, msg *common.QueueRepoMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), worker.MaxDurationForRepoWorker)
	defer cancel()
	logger := rootLogger.With().
		Str("entry", "repo").
		Str("repo", msg.Repository.FullName).
		Logger()

	sess, err := worker.getSession(ctx, &logger, &msg.BaseMessage)
	if err != nil {
		return errors.Wrap(err, "unable to get session")
	}
	if sess == nil {
		return nil
	}

	return worker.fanOutPRs(ctx, &logger, sess)
}
