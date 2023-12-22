package worker

import (
	"context"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
)

type statusWorker struct {
	*Worker
}

func (worker *statusWorker) runLogic(rootLogger *zerolog.Logger, msg *common.QueueStatusMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), worker.MaxDurationForPushWorker)
	defer cancel()
	logger := rootLogger.With().Str("entry", "status").Str("repo", msg.Repository.FullName).Logger()

	sess, err := worker.getSession(ctx, &logger, &msg.BaseMessage)
	if err != nil {
		return errors.Wrap(err, "unable to get session")
	}
	if sess == nil {
		return nil
	}

	return worker.workOnAllPullRequests(ctx, &logger, sess)
}
