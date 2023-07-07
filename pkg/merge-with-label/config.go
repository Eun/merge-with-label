package merge_with_label

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
)

type MergeStrategy string

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
	Merge struct {
		Label                string        `yaml:"label"`
		Strategy             MergeStrategy `yaml:"strategy"`
		RequiredApprovals    int           `yaml:"requiredApprovals"`
		RequiredChecks       []string      `yaml:"requiredChecks"`
		RequireLinearHistory bool          `yaml:"requireLinearHistory"`
		IgnoreConfig
	} `yaml:"merge"`
	Update struct {
		Label string `yaml:"label"`
		IgnoreConfig
	} `yaml:"update"`
}

func defaultConfig() (*ConfigV1, error) {
	var cfg ConfigV1
	err := yaml.Unmarshal([]byte(`
version: 1
merge:
  label: "merge"
  strategy: "squash"
  requiredApprovals: 1
  requiredChecks:
    - .*
update:
  label: "update-branch"
`), &cfg)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

type IgnoreConfig struct {
	IgnoreFromUsers  IgnoreSlice `yaml:"ignoreFromUsers"`
	IgnoreWithTitles IgnoreSlice `yaml:"ignoreWithTitles"`
}

func (c *IgnoreConfig) IsUserIgnored(userName string) (string, error) {
	return c.IgnoreFromUsers.IsIgnored(userName)
}

func (c *IgnoreConfig) IsTitleIgnored(userName string) (string, error) {
	return c.IgnoreFromUsers.IsIgnored(userName)
}

type IgnoreSlice struct {
	slices  []string
	regexes []*regexp.Regexp
}

func (is *IgnoreSlice) createRegexes() error {
	if len(is.slices) == 0 {
		return nil
	}
	var err error
	is.regexes = make([]*regexp.Regexp, len(is.slices))
	for i := 0; i < len(is.slices); i++ {
		is.regexes[i], err = regexp.Compile(is.slices[i])
		if err != nil {
			return errors.Wrapf(err, "`%s' is not a valid regex", is.slices[i])
		}
	}
	return nil
}

func (is *IgnoreSlice) String() string {
	return strings.Join(is.slices, ", ")
}

func (is *IgnoreSlice) MarshalJSON() ([]byte, error) {
	return json.Marshal(is.slices)
}

func (is *IgnoreSlice) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &is.slices); err != nil {
		return err
	}
	return is.createRegexes()
}

func (is *IgnoreSlice) MarshalYAML() (interface{}, error) {
	return is.slices, nil
}

func (is *IgnoreSlice) UnmarshalYAML(unmarshal func(interface{}) error) error {
	if err := unmarshal(&is.slices); err != nil {
		return err
	}
	return is.createRegexes()
}

func (c *IgnoreSlice) IsIgnored(s string) (string, error) {
	for i, re := range c.regexes {
		if re.MatchString(s) {
			return c.slices[i], nil
		}
	}
	return "", nil
}

type cachedConfig struct {
	*ConfigV1
	SHA string
}
