package common

const (
	DelayUntilHeader = "DelayUntil"
)

type Repository struct {
	FullName  string `json:"full_name"`
	Name      string `json:"name"`
	NodeID    string `json:"node_id"`
	OwnerName string `json:"owner_name"`
	Private   bool   `json:"private"`
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
