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

type Message interface {
	GetInstallationID() int64
	GetRepository() Repository
}

type BaseMessage struct {
	InstallationID int64      `json:"installation_id"`
	Repository     Repository `json:"repository"`
}

func (m BaseMessage) GetRepository() Repository {
	return m.Repository
}
func (m BaseMessage) GetInstallationID() int64 {
	return m.InstallationID
}

type QueuePullRequestMessage struct {
	BaseMessage
	PullRequest PullRequest `json:"pull_request"`
}

type QueuePushMessage struct {
	BaseMessage
}

type QueueStatusMessage struct {
	BaseMessage
}
