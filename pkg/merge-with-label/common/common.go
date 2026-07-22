package common

import "fmt"

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

// QueueRepoMessage is enqueued when a repo-level event fires (push, status,
// check_run without a specific PR). The worker fans it out into QueuePRMessages.
type QueueRepoMessage struct {
	BaseMessage
}

// QueuePRMessage is enqueued for a specific pull request. It is the single
// unit of work the pull_request worker processes.
type QueuePRMessage struct {
	BaseMessage
	PullRequest PullRequest `json:"pull_request"`
}

// RepoDedupKey returns the deduplication key for a repo-level event.
// All repo-level events for the same repo collapse to one queue row.
func RepoDedupKey(repoNodeID string) string {
	return "repo:" + repoNodeID
}

// PRDedupKey returns the deduplication key for a PR-level event.
// All events targeting the same PR (pull_request, pull_request_review,
// check_run, push-fanout, status-fanout) collapse to one queue row.
func PRDedupKey(repoNodeID string, prNumber int64) string {
	return fmt.Sprintf("pr:%s:%d", repoNodeID, prNumber)
}
