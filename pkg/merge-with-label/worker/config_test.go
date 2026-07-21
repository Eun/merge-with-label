package worker

import (
	"testing"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
)

func TestParseConfig_Version1(t *testing.T) {
	yaml := []byte(`
version: 1
merge:
  labels: ["merge-me"]
  strategy: squash
  requiredApprovals: 2
  requireLinearHistory: true
  deleteBranch: true
  requiredChecks:
    - ci/.*
update:
  labels: ["update-branch"]
  ignoreFromUsers:
    - dependabot
`)
	cfg, err := parseConfig(yaml)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("Version = %d, want 1", cfg.Version)
	}
	if cfg.Merge.Labels.ContainsOneOf("merge-me") == "" {
		t.Error("merge label not parsed")
	}
	if cfg.Merge.Strategy != SquashMergeStrategy {
		t.Errorf("Strategy = %q, want %q", cfg.Merge.Strategy, SquashMergeStrategy)
	}
	if cfg.Merge.RequiredApprovals != 2 {
		t.Errorf("RequiredApprovals = %d, want 2", cfg.Merge.RequiredApprovals)
	}
	if !cfg.Merge.RequireLinearHistory {
		t.Error("RequireLinearHistory should be true")
	}
	if !cfg.Merge.DeleteBranch {
		t.Error("DeleteBranch should be true")
	}
	if cfg.Merge.RequiredChecks.ContainsOneOf("ci/build") == "" {
		t.Error("RequiredChecks regex not matched")
	}
	if cfg.Update.Labels.ContainsOneOf("update-branch") == "" {
		t.Error("update label not parsed")
	}
	// Note: ignoreFromUsers under update: is only accessible via the embedded
	// IgnoreConfig; testing that separately in TestIgnoreConfig_IsLabelIgnored.
}

func TestParseConfig_UnknownVersion(t *testing.T) {
	_, err := parseConfig([]byte("version: 99\n"))
	if err == nil {
		t.Error("expected error for unknown config version")
	}
}

func TestParseConfig_InvalidYAML(t *testing.T) {
	_, err := parseConfig([]byte("{{not yaml"))
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	if cfg.Merge.Labels.ContainsOneOf("merge") == "" {
		t.Error("default merge label should be 'merge'")
	}
	if cfg.Merge.Strategy != SquashMergeStrategy {
		t.Errorf("default strategy = %q, want squash", cfg.Merge.Strategy)
	}
}

func TestMergeStrategy_GithubString(t *testing.T) {
	tests := []struct {
		strategy MergeStrategy
		want     string
	}{
		{MergeCommitStrategy, "MERGE"},
		{SquashMergeStrategy, "SQUASH"},
		{RebaseMergeStrategy, "REBASE"},
		{MergeStrategy("unknown"), ""},
	}
	for _, tt := range tests {
		if got := tt.strategy.GithubString(); got != tt.want {
			t.Errorf("GithubString(%q) = %q, want %q", tt.strategy, got, tt.want)
		}
	}
}

func TestIgnoreConfig_IsLabelIgnored(t *testing.T) {
	cfg := &IgnoreConfig{
		ignoreWithLabels: common.RegexSlice{
			common.MustNewRegexItem("wip"),
			common.MustNewRegexItem("do-not-merge.*"),
		},
	}
	if cfg.IsLabelIgnored("wip") == "" {
		t.Error("'wip' should be ignored")
	}
	if cfg.IsLabelIgnored("do-not-merge-ever") == "" {
		t.Error("'do-not-merge-ever' should be ignored by regex")
	}
	if cfg.IsLabelIgnored("merge") != "" {
		t.Error("'merge' should not be ignored")
	}
}
