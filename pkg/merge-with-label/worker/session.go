package worker

import (
	"context"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
)

// session holds all necessary information for this run.
type session struct {
	Repository     *common.Repository
	InstallationID int64
	AccessToken    string
	Config         *ConfigV1
}

func (worker *Worker) getSession(ctx context.Context, rootLogger *zerolog.Logger, message *common.BaseMessage) (*session, error) {
	accessToken, err := worker.getAccessToken(ctx, rootLogger, &message.Repository, message.InstallationID)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get access token")
	}

	sha, err := github.GetLatestBaseCommitSha(ctx, worker.HTTPClient, accessToken, &message.Repository)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get latest base commit sha")
	}
	if sha == "" {
		rootLogger.Debug().Msg("latest commit sha is empty")
		return nil, nil
	}

	cfg, err := worker.getConfig(ctx, rootLogger, accessToken, &message.Repository, sha)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get config")
	}
	if cfg == nil {
		rootLogger.Debug().Msg("no config")
		return nil, nil
	}

	if len(cfg.Merge.Labels) == 0 && len(cfg.Update.Labels) == 0 {
		rootLogger.Debug().Msg("merge and update are disabled")
		return nil, nil
	}
	return &session{
		Repository:     &message.Repository,
		InstallationID: message.InstallationID,
		AccessToken:    accessToken,
		Config:         cfg,
	}, nil
}
