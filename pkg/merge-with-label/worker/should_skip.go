package worker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"golang.org/x/exp/slices"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
)

type shouldSkipResult struct {
	SkipAction bool
	Title      string
	Summary    string
}

var statesThatAreSuccess = []string{"NEUTRAL", "SUCCESS", ""}

type shouldSkipFunc func(ctx context.Context, logger *zerolog.Logger, details *github.PullRequestDetails) (shouldSkipResult, error)

func (worker *Worker) shouldSkipMerge(
	ctx context.Context,
	logger *zerolog.Logger,
	cfg *ConfigV1,
	details *github.PullRequestDetails,
) (shouldSkipResult, error) {
	conditions := []shouldSkipFunc{
		worker.shouldSkipBecauseOfTitle(&cfg.Merge.IgnoreConfig),
		worker.shouldSkipBecauseOfLabel(&cfg.Merge.IgnoreConfig),
		worker.shouldSkipBecauseOfAuthorName(&cfg.Merge.IgnoreConfig),
		worker.shouldSkipBecauseOfHistory(&cfg.Merge),
		worker.shouldSkipBecauseOfReviews(&cfg.Merge),
		worker.shouldSkipBecauseOfChecks(&cfg.Merge),
	}

	for i := range conditions {
		result, err := conditions[i](ctx, logger, details)
		if err != nil || result.SkipAction {
			if result.Title != "" {
				result.Title = "not merging: " + result.Title
			}
			return result, errors.WithStack(err)
		}
	}
	return shouldSkipResult{SkipAction: false}, nil
}

func (worker *Worker) shouldSkipUpdate(
	ctx context.Context,
	logger *zerolog.Logger,
	cfg *ConfigV1,
	details *github.PullRequestDetails,
) (shouldSkipResult, error) {
	conditions := []shouldSkipFunc{
		worker.shouldSkipBecauseOfTitle(&cfg.Update.IgnoreConfig),
		worker.shouldSkipBecauseOfLabel(&cfg.Update.IgnoreConfig),
		worker.shouldSkipBecauseOfAuthorName(&cfg.Update.IgnoreConfig),
	}

	for _, condition := range conditions {
		result, err := condition(ctx, logger, details)
		if result.SkipAction || err != nil {
			if result.Title != "" {
				result.Title = "not updating: " + result.Title
			}
			return result, errors.WithStack(err)
		}
	}
	return shouldSkipResult{SkipAction: false}, nil
}

func (worker *Worker) shouldSkipBecauseOfTitle(cfg *IgnoreConfig) shouldSkipFunc {
	return func(_ context.Context, logger *zerolog.Logger, details *github.PullRequestDetails) (shouldSkipResult, error) {
		if ignoredBy := cfg.IsTitleIgnored(details.Title); ignoredBy != "" {
			logger.Info().
				Str("title", details.Title).
				Msg("title is in ignore list")
			return shouldSkipResult{
				SkipAction: true,
				Title:      "title is in ignore list",
				Summary:    fmt.Sprintf("`%s` is in the ignore list (`%s`, matched by `%s`)", details.Title, cfg.IgnoreWithTitles.String(), ignoredBy),
			}, nil
		}
		return shouldSkipResult{SkipAction: false}, nil
	}
}

func (worker *Worker) shouldSkipBecauseOfLabel(cfg *IgnoreConfig) shouldSkipFunc {
	return func(_ context.Context, logger *zerolog.Logger, details *github.PullRequestDetails) (shouldSkipResult, error) {
		for _, label := range details.Labels {
			if cfg.IsLabelIgnored(label) != "" {
				logger.Info().
					Str("label", label).
					Msg("label is in ignore list")
				return shouldSkipResult{
					SkipAction: true,
					Title:      "label is in ignore list",
					Summary:    fmt.Sprintf("`%s` is in the ignore list (`%s`)", label, cfg.ignoreWithLabels.String()),
				}, nil
			}
		}
		return shouldSkipResult{SkipAction: false}, nil
	}
}

func (worker *Worker) shouldSkipBecauseOfAuthorName(cfg *IgnoreConfig) shouldSkipFunc {
	return func(_ context.Context, logger *zerolog.Logger, details *github.PullRequestDetails) (shouldSkipResult, error) {
		ignoredBy := cfg.IsUserIgnored(details.Author)
		if ignoredBy == "" {
			return shouldSkipResult{SkipAction: false}, nil
		}
		logger.Info().
			Str("author", details.Author).
			Msg("author is in ignore list")
		return shouldSkipResult{
			SkipAction: true,
			Title:      "author is in ignore list",
			Summary:    fmt.Sprintf("`%s` is in the ignore list (`%s`, matched by `%s`)", details.Author, cfg.IgnoreFromUsers.String(), ignoredBy),
		}, nil
	}
}

func (worker *Worker) shouldSkipBecauseOfHistory(cfg *MergeConfigV1) shouldSkipFunc {
	return func(_ context.Context, logger *zerolog.Logger, details *github.PullRequestDetails) (shouldSkipResult, error) {
		if !cfg.RequireLinearHistory {
			return shouldSkipResult{
				SkipAction: false,
				Title:      "",
				Summary:    "",
			}, nil
		}
		if details.AheadBy == 0 {
			return shouldSkipResult{
				SkipAction: false,
				Title:      "",
				Summary:    "",
			}, nil
		}
		logger.Info().
			Msg("a linear history is required")
		return shouldSkipResult{
			SkipAction: true,
			Title:      "a linear history is required",
			Summary:    fmt.Sprintf("the branch is not upto date with the latest changes from `%s` branch", details.BaseRefName),
		}, nil
	}
}

func (worker *Worker) buildAvailableChecksList(details *github.PullRequestDetails) string {
	if len(details.CheckStates) == 0 {
		return ""
	}

	type check struct {
		name   string
		state  string
		passed string
	}

	checks := make([]check, 0, len(details.CheckStates))
	for name, state := range details.CheckStates {
		passed := "✅"
		if slices.Index(statesThatAreSuccess, state) == -1 {
			passed = "❌"
		}
		if state == "" {
			state = "\u200e" // empty char, do not delete
		}
		checks = append(checks, check{
			name:   name,
			state:  state,
			passed: passed,
		})
	}

	sort.Slice(checks, func(i, j int) bool {
		return checks[i].name < checks[j].name
	})

	var sb strings.Builder
	sb.WriteString("## Available Checks\n")
	sb.WriteString("| Name | State | Good Enough For Merge? |\n")
	sb.WriteString("| ---- | ----- | ---------------------- |\n")

	for _, item := range checks {
		fmt.Fprintf(&sb, "| `%s` | `%s` | %s |\n", item.name, item.state, item.passed)
	}

	return sb.String()
}

func (worker *Worker) shouldSkipBecauseOfChecks(cfg *MergeConfigV1) shouldSkipFunc {
	return func(_ context.Context, logger *zerolog.Logger, details *github.PullRequestDetails) (shouldSkipResult, error) {
		if len(cfg.RequiredChecks) == 0 {
			return shouldSkipResult{
				SkipAction: false,
				Title:      "",
				Summary:    "",
			}, nil
		}

		type checkInfo struct {
			name  string
			check string
		}
		var checksNotSucceeded []checkInfo
		var checksMissing []string
		for _, re := range cfg.RequiredChecks {
			foundCheck := false
			for name, state := range details.CheckStates {
				if !re.Equal(name) {
					continue
				}
				foundCheck = true
				if slices.Index(statesThatAreSuccess, state) == -1 {
					logger.Info().
						Str("name", name).
						Str("state", state).
						Str("check", re.Text).
						Msg("check did not succeed")
					checksNotSucceeded = append(checksNotSucceeded, checkInfo{
						name:  name,
						check: re.Text,
					})
				}
			}
			if !foundCheck {
				logger.Info().
					Str("check", re.Text).
					Msg("check is missing")
				checksMissing = append(checksMissing, re.Text)
			}
		}

		if len(checksMissing) > 0 {
			lines := make([]string, len(checksMissing))
			for i := range checksMissing {
				lines[i] = fmt.Sprintf("no check matches `%s`", checksMissing[i])
			}
			lines = append(lines, "", worker.buildAvailableChecksList(details))
			return shouldSkipResult{
				SkipAction: true,
				Title:      "check(s) missing",
				Summary:    strings.Join(lines, "\n"),
			}, nil
		}

		if len(checksNotSucceeded) > 0 {
			lines := make([]string, len(checksNotSucceeded))
			for i := range checksNotSucceeded {
				lines[i] = fmt.Sprintf("check `%s` did not succeed (matched by `%s`)", checksNotSucceeded[i].name, checksNotSucceeded[i].check)
			}
			lines = append(lines, "", worker.buildAvailableChecksList(details))
			return shouldSkipResult{
				SkipAction: true,
				Title:      "check(s) did not succeeded",
				Summary:    strings.Join(lines, "\n"),
			}, nil
		}

		if diff := time.Until(details.LastCommitTime.Add(worker.DurationToWaitAfterUpdateBranch)); diff > 0 {
			// it's a bit too early. block merging, push back onto the queue
			logger.Debug().Msg("delaying merge, because commit was too recent")
			return shouldSkipResult{SkipAction: false}, pushBackError{delay: diff}
		}
		return shouldSkipResult{SkipAction: false}, nil
	}
}

func (worker *Worker) shouldSkipBecauseOfReviews(cfg *MergeConfigV1) shouldSkipFunc {
	return func(_ context.Context, logger *zerolog.Logger, details *github.PullRequestDetails) (shouldSkipResult, error) {
		if cfg.RequiredApprovals > 0 && cfg.RequiredApprovals > len(details.ApprovedBy) {
			logger.Info().
				Int("required_approvals", cfg.RequiredApprovals).
				Int("current_approvals", len(details.ApprovedBy)).
				Msg("missing required approvals")

			return shouldSkipResult{
				SkipAction: true,
				Title:      "missing required approvals",
				Summary:    fmt.Sprintf("%d approvals are required, got %d", cfg.RequiredApprovals, len(details.ApprovedBy)),
			}, nil
		}

		if len(cfg.RequireApprovalsFrom) > 0 {
			var authorsMissing []string
			for _, re := range cfg.RequireApprovalsFrom {
				foundAuthor := false
				for _, name := range details.ApprovedBy {
					if re.Equal(name) {
						foundAuthor = true
						break
					}
				}
				if !foundAuthor {
					logger.Info().
						Str("author", re.Text).
						Msg("author did not approve")
					authorsMissing = append(authorsMissing, re.Text)
				}
			}

			if len(authorsMissing) > 0 {
				lines := make([]string, len(authorsMissing))
				for i := range authorsMissing {
					lines[i] = fmt.Sprintf("`%s` didnt approved yet", authorsMissing[i])
				}
				return shouldSkipResult{
					SkipAction: true,
					Title:      "approval(s) missing",
					Summary:    strings.Join(lines, "\n"),
				}, nil
			}
		}
		return shouldSkipResult{SkipAction: false}, nil
	}
}
