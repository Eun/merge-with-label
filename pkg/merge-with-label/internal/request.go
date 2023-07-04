package internal

import "time"

type Repository struct {
	FullName     string `json:"full_name"`
	MasterBranch string `json:"master_branch"`
	Name         string `json:"name"`
	Owner        struct {
		Name string `json:"name"`
	} `json:"owner"`
}

type Request struct {
	Action      string `json:"action"`
	PullRequest *struct {
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
	} `json:"pull_request"`
	Repository   *Repository `json:"repository"`
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
