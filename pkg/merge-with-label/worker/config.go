package worker

import (
	"context"
	"encoding/json"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

type MergeStrategy string

func (s MergeStrategy) GithubString() string {
	switch s {
	case MergeCommitStrategy:
		return "MERGE"
	case SquashMergeStrategy:
		return "SQUASH"
	case RebaseMergeStrategy:
		return "REBASE"
	}
	return ""
}

const (
	MergeCommitStrategy MergeStrategy = "commit"
	SquashMergeStrategy MergeStrategy = "squash"
	RebaseMergeStrategy MergeStrategy = "rebase"
)

type ConfigHeader struct {
	Version int `yaml:"version"`
}

type ConfigV1 struct {
	ConfigHeader
	Merge  MergeConfigV1  `yaml:"merge"`
	Update UpdateConfigV1 `yaml:"update"`
}

type MergeConfigV1 struct {
	Labels               common.RegexSlice `yaml:"labels"`
	Strategy             MergeStrategy     `yaml:"strategy"`
	RequiredApprovals    int               `yaml:"requiredApprovals"`
	RequireApprovalsFrom common.RegexSlice `yaml:"requireApprovalsFrom"`
	RequiredChecks       common.RegexSlice `yaml:"requiredChecks"`
	RequireLinearHistory bool              `yaml:"requireLinearHistory"`
	DeleteBranch         bool              `yaml:"deleteBranch"`
	IgnoreConfig
}

type UpdateConfigV1 struct {
	Labels common.RegexSlice `yaml:"labels"`
	IgnoreConfig
}

func defaultConfig() (*ConfigV1, error) {
	var cfg ConfigV1
	err := yaml.Unmarshal([]byte(`
version: 1
merge:
  labels: ["merge"]
  strategy: "squash"
  requiredChecks:
    - .*
  requireLinearHistory: false
  deleteBranch: true
update:
  labels: ["update-branch"]
  ignoreFromUsers:
    - "dependabot"
`), &cfg)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

type IgnoreConfig struct {
	IgnoreFromUsers  common.RegexSlice `yaml:"ignoreFromUsers"`
	IgnoreWithTitles common.RegexSlice `yaml:"ignoreWithTitles"`
	ignoreWithLabels common.RegexSlice `yaml:"ignoreWithLabels"`
}

func (c *IgnoreConfig) IsUserIgnored(s string) string {
	return c.IgnoreFromUsers.ContainsOneOf(s)
}

func (c *IgnoreConfig) IsTitleIgnored(s string) string {
	return c.IgnoreWithTitles.ContainsOneOf(s)
}

func (c *IgnoreConfig) IsLabelIgnored(s string) string {
	return c.ignoreWithLabels.ContainsOneOf(s)
}

type cachedConfig struct {
	*ConfigV1
	SHA string
}

func parseConfig(buf []byte) (*ConfigV1, error) {
	var hdr ConfigHeader
	if err := yaml.Unmarshal(buf, &hdr); err != nil {
		return nil, errors.Wrap(err, "unable to decode config header")
	}

	switch hdr.Version {
	case 1:
		var cfg ConfigV1
		if err := yaml.Unmarshal(buf, &cfg); err != nil {
			return nil, errors.Wrap(err, "unable to decode config")
		}
		cfg.Version = hdr.Version
		return &cfg, nil
	default:
		return nil, errors.Errorf("unknown version `%d'", hdr.Version)
	}
}

func (worker *Worker) getConfig(
	ctx context.Context,
	rootLogger *zerolog.Logger,
	accessToken string,
	repository *common.Repository,
	sha string,
) (*ConfigV1, error) {
	if sha == "" {
		return nil, nil
	}
	key := hashForKV(repository.FullName)
	logger := rootLogger.With().
		Str("hash_key", key).
		Str("sha", sha).
		Logger()

	entry, err := worker.ConfigsKV.Get(key)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return nil, errors.Wrap(err, "unable to get config from kv bucket")
	}
	if entry == nil || len(entry.Value()) == 0 || errors.Is(err, nats.ErrKeyNotFound) {
		logger.Debug().
			Str("reason", "not in cache").
			Msg("getting latest config")
		return worker.getLatestConfig(ctx, &logger, accessToken, repository, key, sha)
	}

	var config cachedConfig
	if err := json.Unmarshal(entry.Value(), &config); err != nil {
		return nil, errors.Wrap(err, "unable to decode config from kv bucket")
	}
	if config.SHA != sha {
		logger.Debug().
			Str("reason", "possible old config").
			Msg("getting latest config")
		return worker.getLatestConfig(ctx, &logger, accessToken, repository, key, sha)
	}
	logger.Debug().
		Msg("got config from cache")
	return config.ConfigV1, err
}

func (worker *Worker) getLatestConfig(
	ctx context.Context,
	rootLogger *zerolog.Logger,
	accessToken string,
	repository *common.Repository,
	key,
	sha string,
) (*ConfigV1, error) {
	rootLogger.Debug().Msg("getting latest config from github")
	buf, err := github.GetConfig(ctx, worker.HTTPClient, accessToken, repository, sha)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get config from github")
	}
	if buf == nil {
		rootLogger.Debug().Msg("no config found, returning default config")
		return defaultConfig()
	}

	cfg, err := parseConfig(buf)
	if err != nil {
		return nil, errors.Wrap(err, "unable to parse config")
	}

	buf, err = json.Marshal(&cachedConfig{
		ConfigV1: cfg,
		SHA:      sha,
	})
	if err != nil {
		return nil, errors.Wrap(err, "unable to encode config")
	}
	rootLogger.Debug().Msg("storing config in cache")
	if _, err := worker.ConfigsKV.Put(key, buf); err != nil {
		return nil, errors.Wrap(err, "unable to store access token in kv bucket")
	}
	return cfg, nil
}
