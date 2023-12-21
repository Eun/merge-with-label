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
	AllowedRepositoriesSetting             Setting = "AllowedRepositories"
	AllowOnlyPublicRepositories            Setting = "AllowOnlyPublicRepositories"
	BotNameSetting                         Setting = "BotName"
	StreamNameSetting                      Setting = "StreamName"
	PushSubjectSetting                     Setting = "PushSubject"
	StatusSubjectSetting                   Setting = "StatusSubject"
	PullRequestSubjectSetting              Setting = "PullRequestSubject"
	MessageRetryAttemptsSetting            Setting = "MessageRetryAttempts"
	MessageRetryWaitSetting                Setting = "MessageRetryWait"
	RateLimitBucketNameSetting             Setting = "RateLimitBucketName"
	RateLimitBucketTTLSetting              Setting = "RateLimitBucketTTL"
	RateLimitIntervalSetting               Setting = "RateLimitInterval"
	AccessTokensBucketNameSetting          Setting = "AccessTokensBucketName"
	AccessTokensBucketTTLSetting           Setting = "AccessTokensBucketTTL"
	ConfigsBucketNameSetting               Setting = "ConfigsBucketName"
	ConfigsBucketTTLSetting                Setting = "ConfigsBucketTTL"
	CheckRunsBucketNameSetting             Setting = "CheckRunsBucketName"
	CheckRunsBucketTTLSetting              Setting = "CheckRunsBucketTTL"
	DurationBeforeMergeAfterCheckSetting   Setting = "DurationBeforeMergeAfterCheck"
	DurationToWaitAfterUpdateBranchSetting Setting = "DurationToWaitAfterUpdateBranch"
	MaxMessageAgeSetting                   Setting = "MaxMessageAge"
	MessageChannelSizePerSubjectSetting    Setting = "MessageChannelSizePerSubject"
)

var defaultSettings = map[Setting]any{
	AllowedRepositoriesSetting:             common.RegexSlice{common.MustNewRegexItem(".*")},
	AllowOnlyPublicRepositories:            false,
	BotNameSetting:                         "merge-with-label",
	StreamNameSetting:                      "mwl_bot_events",
	PushSubjectSetting:                     "push",
	StatusSubjectSetting:                   "status",
	PullRequestSubjectSetting:              "pull_request",
	MessageRetryAttemptsSetting:            5,                //nolint: gomnd // allow to set defaults
	MessageRetryWaitSetting:                time.Second * 15, //nolint: gomnd // allow to set defaults
	RateLimitBucketNameSetting:             "mwl_rate_limit",
	RateLimitBucketTTLSetting:              time.Hour * 24,   //nolint: gomnd // allow to set defaults
	RateLimitIntervalSetting:               time.Second * 30, //nolint: gomnd // allow to set defaults
	AccessTokensBucketNameSetting:          "mwl_access_tokens",
	AccessTokensBucketTTLSetting:           time.Hour * 24, //nolint: gomnd // allow to set defaults
	ConfigsBucketNameSetting:               "mwl_configs",
	ConfigsBucketTTLSetting:                time.Hour * 24, //nolint: gomnd // allow to set defaults
	CheckRunsBucketNameSetting:             "mwl_check_runs",
	CheckRunsBucketTTLSetting:              time.Minute * 10, //nolint: gomnd // allow to set defaults
	DurationBeforeMergeAfterCheckSetting:   time.Second * 10, //nolint: gomnd // allow to set defaults
	DurationToWaitAfterUpdateBranchSetting: time.Second * 30, //nolint: gomnd // allow to set defaults
	MaxMessageAgeSetting:                   time.Minute * 10, //nolint: gomnd // allow to set defaults
	MessageChannelSizePerSubjectSetting:    64,               //nolint: gomnd // allow to set defaults
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
