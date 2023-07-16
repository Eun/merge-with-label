package common

const (
	DelayUntilHeader = "DelayUntil"
)

type Repository struct {
	NodeID    string `json:"node_id"`
	FullName  string `json:"full_name"`
	Name      string `json:"name"`
	OwnerName string `json:"owner_name"`
}
type PullRequest struct {
	Number int64 `json:"number"`
}

type QueuePullRequestMessage struct {
	InstallationID int64       `json:"installation_id"`
	Repository     Repository  `json:"repository"`
	PullRequest    PullRequest `json:"pull_request"`
}

type QueuePushMessage struct {
	InstallationID int64      `json:"installation_id"`
	Repository     Repository `json:"repository"`
}
