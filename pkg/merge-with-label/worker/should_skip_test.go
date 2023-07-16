package worker

import (
	"context"
	"testing"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
	"github.com/rs/zerolog/log"
)

func Test_shouldSkipBecauseOfReviews(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *MergeConfigV1
		details        *github.PullRequestDetails
		wantSkipAction bool
		wantErr        bool
	}{
		{
			name:           "skip action when no review is present and 1 is required",
			cfg:            &MergeConfigV1{RequiredApprovals: 1},
			details:        &github.PullRequestDetails{},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when 1 review is present and 2 are required",
			cfg:            &MergeConfigV1{RequiredApprovals: 2},
			details:        &github.PullRequestDetails{ApprovedBy: []string{"user1"}},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when no review is present and 1 is required by a specific reviewer",
			cfg:            &MergeConfigV1{RequireApprovalsFrom: common.RegexSlice{common.MustNewRegexItem("owner")}},
			details:        &github.PullRequestDetails{},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when one review is present, but its not from the required reviewer",
			cfg:            &MergeConfigV1{RequireApprovalsFrom: common.RegexSlice{common.MustNewRegexItem("owner")}},
			details:        &github.PullRequestDetails{ApprovedBy: []string{"user"}},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when one review is present and required, but its not from the required reviewer",
			cfg:            &MergeConfigV1{RequiredApprovals: 1, RequireApprovalsFrom: common.RegexSlice{common.MustNewRegexItem("owner")}},
			details:        &github.PullRequestDetails{ApprovedBy: []string{"user"}},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when one review is present, but two are required by specific users",
			cfg:            &MergeConfigV1{RequireApprovalsFrom: common.RegexSlice{common.MustNewRegexItem("contributor"), common.MustNewRegexItem("owner")}},
			details:        &github.PullRequestDetails{ApprovedBy: []string{"contributor"}},
			wantSkipAction: true,
			wantErr:        false,
		},

		{
			name:           "dont skip action when two reviews are set and required",
			cfg:            &MergeConfigV1{RequiredApprovals: 2},
			details:        &github.PullRequestDetails{ApprovedBy: []string{"user", "contributor"}},
			wantSkipAction: false,
			wantErr:        false,
		},
		{
			name:           "dont skip action when more reviews are set than required",
			cfg:            &MergeConfigV1{RequiredApprovals: 1},
			details:        &github.PullRequestDetails{ApprovedBy: []string{"user", "contributor"}},
			wantSkipAction: false,
			wantErr:        false,
		},
		{
			name:           "dont skip action when all of the required reviewers reviewed",
			cfg:            &MergeConfigV1{RequireApprovalsFrom: common.RegexSlice{common.MustNewRegexItem("contributor"), common.MustNewRegexItem("owner")}},
			details:        &github.PullRequestDetails{ApprovedBy: []string{"owner", "contributor"}},
			wantSkipAction: false,
			wantErr:        false,
		},
		{
			name:           "dont skip action when all of the required reviewers reviewed and the amount of reviewers is also met",
			cfg:            &MergeConfigV1{RequiredApprovals: 2, RequireApprovalsFrom: common.RegexSlice{common.MustNewRegexItem("owner")}},
			details:        &github.PullRequestDetails{ApprovedBy: []string{"owner", "contributor"}},
			wantSkipAction: false,
			wantErr:        false,
		},
	}
	worker := Worker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := worker.shouldSkipBecauseOfReviews(tt.cfg)(context.Background(), &log.Logger, tt.details)
			if (err != nil) != tt.wantErr {
				t.Errorf("shouldSkipBecauseOfReviews() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got.SkipAction != tt.wantSkipAction {
				t.Errorf("shouldSkipBecauseOfReviews() got = %v, wantSkipAction %v", got, tt.wantSkipAction)
			}
		})
	}
}

func Test_shouldSkipBecauseOfChecks(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *MergeConfigV1
		details        *github.PullRequestDetails
		wantSkipAction bool
		wantErr        bool
	}{
		{
			name:           "skip action when no check is present and 1 is required by a specific reviewer",
			cfg:            &MergeConfigV1{RequiredChecks: common.RegexSlice{common.MustNewRegexItem("check1")}},
			details:        &github.PullRequestDetails{CheckStates: map[string]string{}},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when one check is present, but it is not SUCCESS",
			cfg:            &MergeConfigV1{RequiredChecks: common.RegexSlice{common.MustNewRegexItem("check1")}},
			details:        &github.PullRequestDetails{CheckStates: map[string]string{"check1": "FAILED"}},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "dont skip action when no required checks are defined",
			cfg:            &MergeConfigV1{},
			details:        &github.PullRequestDetails{CheckStates: map[string]string{"check1": "SUCCESS"}},
			wantSkipAction: false,
			wantErr:        false,
		},
		{
			name:           "dont skip action when all checks are present and they are either SUCCESS, NEUTRAL or (empty)",
			cfg:            &MergeConfigV1{RequiredChecks: common.RegexSlice{common.MustNewRegexItem("check1"), common.MustNewRegexItem("check2"), common.MustNewRegexItem("check3")}},
			details:        &github.PullRequestDetails{CheckStates: map[string]string{"check1": "SUCCESS", "check2": "NEUTRAL", "check3": ""}},
			wantSkipAction: false,
			wantErr:        false,
		},
	}
	worker := Worker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := worker.shouldSkipBecauseOfChecks(tt.cfg)(context.Background(), &log.Logger, tt.details)
			if (err != nil) != tt.wantErr {
				t.Errorf("shouldSkipBecauseOfChecks() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got.SkipAction != tt.wantSkipAction {
				t.Errorf("shouldSkipBecauseOfChecks() got = %v, wantSkipAction %v", got, tt.wantSkipAction)
			}
		})
	}
}

func Test_shouldSkipBecauseOfLabel(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *IgnoreConfig
		details        *github.PullRequestDetails
		wantSkipAction bool
		wantErr        bool
	}{
		{
			name:           "skip action when no-merge label is present and configured",
			cfg:            &IgnoreConfig{ignoreWithLabels: common.RegexSlice{common.MustNewRegexItem("no-merge")}},
			details:        &github.PullRequestDetails{Labels: []string{"no-merge"}},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when no-merge label is present and configured, but it is uppercase",
			cfg:            &IgnoreConfig{ignoreWithLabels: common.RegexSlice{common.MustNewRegexItem("no-merge")}},
			details:        &github.PullRequestDetails{Labels: []string{"NO-MERGE"}},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip skip action when no-merge label is present and configured using regex",
			cfg:            &IgnoreConfig{ignoreWithLabels: common.RegexSlice{common.MustNewRegexItem("no-merge-.+")}},
			details:        &github.PullRequestDetails{Labels: []string{"no-merge-until-now"}},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when no-merge label is present and a slice is configured",
			cfg:            &IgnoreConfig{ignoreWithLabels: common.RegexSlice{common.MustNewRegexItem("never-merge"), common.MustNewRegexItem("no-merge")}},
			details:        &github.PullRequestDetails{Labels: []string{"no-merge"}},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when merge and no-merge label are present and a slice is configured",
			cfg:            &IgnoreConfig{ignoreWithLabels: common.RegexSlice{common.MustNewRegexItem("never-merge"), common.MustNewRegexItem("no-merge")}},
			details:        &github.PullRequestDetails{Labels: []string{"merge", "no-merge"}},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "dont skip action when no-merge label is present, but not configured",
			cfg:            &IgnoreConfig{},
			details:        &github.PullRequestDetails{Labels: []string{"no-merge"}},
			wantSkipAction: false,
			wantErr:        false,
		},
		{
			name:           "dont skip action when no-merge label is present, but never-merge label was configured",
			cfg:            &IgnoreConfig{ignoreWithLabels: common.RegexSlice{common.MustNewRegexItem("never-merge")}},
			details:        &github.PullRequestDetails{Labels: []string{"no-merge"}},
			wantSkipAction: false,
			wantErr:        false,
		},
	}
	worker := Worker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := worker.shouldSkipBecauseOfLabel(tt.cfg)(context.Background(), &log.Logger, tt.details)
			if (err != nil) != tt.wantErr {
				t.Errorf("shouldSkipBecauseOfLabel() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got.SkipAction != tt.wantSkipAction {
				t.Errorf("shouldSkipBecauseOfLabel() got = %v, wantSkipAction %v", got, tt.wantSkipAction)
			}
		})
	}
}

func Test_shouldSkipBecauseOfHistory(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *MergeConfigV1
		details        *github.PullRequestDetails
		wantSkipAction bool
		wantErr        bool
	}{
		{
			name:           "skip action when linear history is required and pull request is ahead",
			cfg:            &MergeConfigV1{RequireLinearHistory: true},
			details:        &github.PullRequestDetails{AheadBy: 1},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "dont skip action when linear history is required and pull request is not ahead",
			cfg:            &MergeConfigV1{RequireLinearHistory: true},
			details:        &github.PullRequestDetails{AheadBy: 0},
			wantSkipAction: false,
			wantErr:        false,
		},
		{
			name:           "dont skip action when linear history is not required and pull request is not ahead",
			cfg:            &MergeConfigV1{RequireLinearHistory: false},
			details:        &github.PullRequestDetails{AheadBy: 0},
			wantSkipAction: false,
			wantErr:        false,
		},
		{
			name:           "dont skip action when linear history is not required and pull request is ahead",
			cfg:            &MergeConfigV1{RequireLinearHistory: false},
			details:        &github.PullRequestDetails{AheadBy: 1},
			wantSkipAction: false,
			wantErr:        false,
		},
	}
	worker := Worker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := worker.shouldSkipBecauseOfHistory(tt.cfg)(context.Background(), &log.Logger, tt.details)
			if (err != nil) != tt.wantErr {
				t.Errorf("shouldSkipBecauseOfHistory() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got.SkipAction != tt.wantSkipAction {
				t.Errorf("shouldSkipBecauseOfHistory() got = %v, wantSkipAction %v", got, tt.wantSkipAction)
			}
		})
	}
}

func Test_shouldSkipBecauseOfTitle(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *IgnoreConfig
		details        *github.PullRequestDetails
		wantSkipAction bool
		wantErr        bool
	}{
		{
			name:           "skip action when no-merge title is present and configured",
			cfg:            &IgnoreConfig{IgnoreWithTitles: common.RegexSlice{common.MustNewRegexItem("no-merge")}},
			details:        &github.PullRequestDetails{Title: "no-merge"},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when no-merge title is present and configured, but it is uppercase",
			cfg:            &IgnoreConfig{IgnoreWithTitles: common.RegexSlice{common.MustNewRegexItem("no-merge")}},
			details:        &github.PullRequestDetails{Title: "NO-MERGE"},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when no-merge title is present and configured using regex",
			cfg:            &IgnoreConfig{IgnoreWithTitles: common.RegexSlice{common.MustNewRegexItem("no-merge-.+")}},
			details:        &github.PullRequestDetails{Title: "no-merge-until-now"},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when no-merge title is present and a slice is configured",
			cfg:            &IgnoreConfig{IgnoreWithTitles: common.RegexSlice{common.MustNewRegexItem("never-merge"), common.MustNewRegexItem("no-merge")}},
			details:        &github.PullRequestDetails{Title: "no-merge"},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "dont skip action when no-merge title is present, but not configured",
			cfg:            &IgnoreConfig{},
			details:        &github.PullRequestDetails{Title: "no-merge"},
			wantSkipAction: false,
			wantErr:        false,
		},
		{
			name:           "dont skip action when no-merge title is present, but never-merge title was configured",
			cfg:            &IgnoreConfig{IgnoreWithTitles: common.RegexSlice{common.MustNewRegexItem("never-merge")}},
			details:        &github.PullRequestDetails{Title: "no-merge"},
			wantSkipAction: false,
			wantErr:        false,
		},
	}
	worker := Worker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := worker.shouldSkipBecauseOfTitle(tt.cfg)(context.Background(), &log.Logger, tt.details)
			if (err != nil) != tt.wantErr {
				t.Errorf("shouldSkipBecauseOfTitle() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got.SkipAction != tt.wantSkipAction {
				t.Errorf("shouldSkipBecauseOfTitle() got = %v, wantSkipAction %v", got, tt.wantSkipAction)
			}
		})
	}
}

func Test_shouldSkipBecauseOfAuthorName(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *IgnoreConfig
		details        *github.PullRequestDetails
		wantSkipAction bool
		wantErr        bool
	}{
		{
			name:           "skip action when no-merge author is present and configured",
			cfg:            &IgnoreConfig{IgnoreFromUsers: common.RegexSlice{common.MustNewRegexItem("no-merge")}},
			details:        &github.PullRequestDetails{Author: "no-merge"},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when no-merge author is present and configured, but it is uppercase",
			cfg:            &IgnoreConfig{IgnoreFromUsers: common.RegexSlice{common.MustNewRegexItem("no-merge")}},
			details:        &github.PullRequestDetails{Author: "NO-MERGE"},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when no-merge author is present and configured using regex",
			cfg:            &IgnoreConfig{IgnoreFromUsers: common.RegexSlice{common.MustNewRegexItem("no-merge-.+")}},
			details:        &github.PullRequestDetails{Author: "no-merge-until-now"},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "skip action when no-merge author is present and a slice is configured",
			cfg:            &IgnoreConfig{IgnoreFromUsers: common.RegexSlice{common.MustNewRegexItem("never-merge"), common.MustNewRegexItem("no-merge")}},
			details:        &github.PullRequestDetails{Author: "no-merge"},
			wantSkipAction: true,
			wantErr:        false,
		},
		{
			name:           "dont skip action when no-merge author is present, but not configured",
			cfg:            &IgnoreConfig{},
			details:        &github.PullRequestDetails{Author: "no-merge"},
			wantSkipAction: false,
			wantErr:        false,
		},
		{
			name:           "dont skip action when no-merge author is present, but never-merge author was configured",
			cfg:            &IgnoreConfig{IgnoreFromUsers: common.RegexSlice{common.MustNewRegexItem("never-merge")}},
			details:        &github.PullRequestDetails{Author: "no-merge"},
			wantSkipAction: false,
			wantErr:        false,
		},
	}
	worker := Worker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := worker.shouldSkipBecauseOfAuthorName(tt.cfg)(context.Background(), &log.Logger, tt.details)
			if (err != nil) != tt.wantErr {
				t.Errorf("shouldSkipBecauseOfAuthorName() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got.SkipAction != tt.wantSkipAction {
				t.Errorf("shouldSkipBecauseOfAuthorName() got = %v, wantSkipAction %v", got, tt.wantSkipAction)
			}
		})
	}
}
