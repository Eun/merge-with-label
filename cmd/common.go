package cmd

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
)

type Setting string

const (
	AllowedRepositoriesSetting          Setting = "AllowedRepositories"
	AllowOnlyPublicRepositories         Setting = "AllowOnlyPublicRepositories"
	BotNameSetting                      Setting = "BotName"
	PostgresDSNSetting                  Setting = "PostgresDSN"
	RateLimitIntervalSetting            Setting = "RateLimitInterval"
	MessageRetryAttemptsSetting         Setting = "MessageRetryAttempts"
	MessageRetryWaitSetting             Setting = "MessageRetryWait"
	DurationBeforeMergeAfterCheckSetting   Setting = "DurationBeforeMergeAfterCheck"
	DurationToWaitAfterUpdateBranchSetting Setting = "DurationToWaitAfterUpdateBranch"
	MessageChannelSizePerSubjectSetting Setting = "MessageChannelSizePerSubject"
)

var defaultSettings = map[Setting]any{
	AllowedRepositoriesSetting:          common.RegexSlice{common.MustNewRegexItem(".*")},
	AllowOnlyPublicRepositories:         false,
	BotNameSetting:                      "merge-with-label",
	PostgresDSNSetting:                  "postgres://postgres:postgres@localhost:5432/merge_with_label?sslmode=disable",
	RateLimitIntervalSetting:            time.Second * 30, //nolint:mnd // allow to set defaults
	MessageRetryAttemptsSetting:         5,                //nolint:mnd // allow to set defaults
	MessageRetryWaitSetting:             time.Second * 15, //nolint:mnd // allow to set defaults
	DurationBeforeMergeAfterCheckSetting:   time.Second * 10, //nolint:mnd // allow to set defaults
	DurationToWaitAfterUpdateBranchSetting: time.Second * 30, //nolint:mnd // allow to set defaults
	MessageChannelSizePerSubjectSetting:    64,               //nolint:mnd // allow to set defaults
}

func GetSetting[T any](name Setting) (t T) {
	if s := os.Getenv(string(name)); s != "" {
		return convertValue(s, reflect.TypeOf(t)).Interface().(T)
	}
	return defaultSettings[name].(T)
}

func convertValue(value string, targetType reflect.Type) reflect.Value {
	if targetType == reflect.TypeOf(common.RegexSlice{}) {
		s := strings.Split(value, ",")
		items := make(common.RegexSlice, 0, len(s))
		for _, item := range s {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			items = append(items, common.MustNewRegexItem(item))
		}
		return reflect.ValueOf(items)
	}
	if targetType == reflect.TypeOf(time.Duration(0)) {
		t, err := time.ParseDuration(value)
		if err != nil {
			panic(fmt.Sprintf("unable to parse duration `%s'", value))
		}
		return reflect.ValueOf(t)
	}
	switch targetType.Kind() {
	case reflect.Bool:
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return reflect.ValueOf(boolValue).Convert(targetType)
		}
	case reflect.String:
		return reflect.ValueOf(value)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if intValue, err := strconv.ParseInt(value, 10, 64); err == nil {
			return reflect.ValueOf(intValue).Convert(targetType)
		}
	default:
		panic(fmt.Sprintf("unsupported type: %s", targetType))
	}
	return reflect.Zero(targetType)
}
