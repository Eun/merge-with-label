package common

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/pkg/errors"
)

type RegexItem struct {
	Text  string
	Regex *regexp.Regexp
}

func (sl *RegexItem) createRegex() (err error) {
	sl.Regex, err = regexp.Compile(sl.Text)
	if err != nil {
		return errors.Wrapf(err, "`%s' is not a valid regex", sl.Text)
	}
	return nil
}

func (sl *RegexItem) MarshalJSON() ([]byte, error) {
	return json.Marshal(sl.Text)
}

func (sl *RegexItem) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &sl.Text); err != nil {
		return err
	}
	return sl.createRegex()
}

func (sl *RegexItem) MarshalYAML() (interface{}, error) {
	return sl.Text, nil
}

func (sl *RegexItem) UnmarshalYAML(unmarshal func(interface{}) error) error {
	if err := unmarshal(&sl.Text); err != nil {
		return err
	}
	return sl.createRegex()
}

func (sl *RegexItem) Equal(s string) bool {
	if strings.EqualFold(s, sl.Text) {
		return true
	}
	if sl.Regex.MatchString(s) {
		return true
	}
	return false
}

type RegexSlice []RegexItem

func (sl RegexSlice) String() string {
	return strings.Join(sl.Strings(), ", ")
}
func (sl RegexSlice) Strings() []string {
	s := make([]string, len(sl))
	for i := 0; i < len(sl); i++ {
		s[i] = sl[i].Text
	}
	return s
}

func (sl RegexSlice) ContainsOneOf(items ...string) string {
	for _, item := range items {
		for _, re := range sl {
			if re.Equal(item) {
				return re.Text
			}
		}
	}
	return ""
}

func MustNewRegexItem(text string) (i RegexItem) {
	i.Text = text
	if err := i.createRegex(); err != nil {
		panic(err)
	}
	return i
}
