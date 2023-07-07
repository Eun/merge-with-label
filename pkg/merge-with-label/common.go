package merge_with_label

import (
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
)

type Owner struct {
	Login string `json:"login"`
}

type Repository struct {
	NodeId        string `json:"node_id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Name          string `json:"name"`
	Owner         Owner  `json:"owner"`
}

type PullRequest struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Mergeable bool   `json:"mergeable"`
	Merged    bool   `json:"merged"`
	Number    int    `json:"number"`
	State     string `json:"state"`
	Head      struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type Request struct {
	Action       string       `json:"action"`
	PullRequest  *PullRequest `json:"pull_request"`
	Repository   *Repository  `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repositories []struct {
		FullName string `json:"full_name"`
	} `json:"repositories"`
}

type MergeResponse struct {
	Merged bool `json:"merged"`
}

type AccessToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type CheckRunsResponse struct {
	TotalCount int `json:"total_count"`
	CheckRuns  []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

type QueueMessageKind uint8

const (
	Unknown QueueMessageKind = iota
	PullRequestMessage
	PushRequestMessage
)

type QueueMessage struct {
	ID              string           `json:"id"`
	Kind            QueueMessageKind `json:"kind"`
	PushBackCounter int              `json:"push_back_counter"`
	DelayUntil      time.Time        `json:"delay_until"`
}
type QueuePullRequestMessage struct {
	QueueMessage
	InstallationID int64        `json:"installation_id"`
	PullRequest    *PullRequest `json:"pull_request"`
	Repository     *Repository  `json:"repository"`
}

type QueuePushMessage struct {
	QueueMessage
	InstallationID int64       `json:"installation_id"`
	Repository     *Repository `json:"repository"`
}

func decodeMap(input map[string]any, out any, isHeader bool) error {
	config := &mapstructure.DecoderConfig{
		DecodeHook:           mapstructure.StringToTimeHookFunc(time.RFC3339),
		ErrorUnused:          !isHeader,
		ErrorUnset:           true,
		ZeroFields:           true,
		WeaklyTypedInput:     true,
		Squash:               true,
		Result:               out,
		TagName:              "json",
		IgnoreUntaggedFields: true,
	}

	dec, err := mapstructure.NewDecoder(config)
	if err != nil {
		return errors.Wrap(err, "unable to create decoder")
	}

	if err := dec.Decode(input); err != nil {
		return errors.Wrap(err, "unable to decode message")
	}
	return nil
}
