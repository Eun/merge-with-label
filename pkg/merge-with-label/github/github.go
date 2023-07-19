package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	gengraphql "github.com/Eun/go-gen-graphql"
	"github.com/golang-jwt/jwt/v4"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/common"
)

const maxBodyBytes = 1024 * 1024 * 16

var _ zerolog.LogObjectMarshaler = &ResponseError{}

type ResponseError struct {
	Message            string
	ActualStatusCode   int
	ExpectedStatusCode int
	Body               string
	NextError          error
}

func (e *ResponseError) Error() string {
	var sb strings.Builder
	if e.Message != "" {
		sb.WriteString(e.Message)
	}

	if e.NextError != nil {
		if sb.Len() > 0 {
			sb.WriteString(": ")
		}
		sb.WriteString(e.NextError.Error())
	}

	return sb.String()
}

func (e *ResponseError) MarshalZerologObject(ev *zerolog.Event) {
	ev.Str("message", e.Message)
	if e.ActualStatusCode != e.ExpectedStatusCode {
		ev.Int("actual_status_code", e.ActualStatusCode)
		ev.Int("expected_status_code", e.ExpectedStatusCode)
	}
	ev.Err(e.NextError)
}

type GraphQLErrors struct {
	Errors []string
}

func (g GraphQLErrors) Error() string {
	return strings.Join(g.Errors, "\n")
}

func doGraphQLRequest(ctx context.Context, client *http.Client, token, query string, variables any) ([]byte, error) {
	var body bytes.Buffer
	err := json.NewEncoder(&body).Encode(struct {
		Query     string `json:"query"`
		Variables any    `json:"variables"`
	}{
		Query:     query,
		Variables: variables,
	})

	if err != nil {
		return nil, errors.Wrap(err, "unable to create body")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", &body)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create request")
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "unable to execute request")
	}
	body.Reset()
	_, err = io.Copy(&body, io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, errors.Wrap(err, "unable to copy body")
	}
	if err := resp.Body.Close(); err != nil {
		return nil, errors.Wrap(err, "unable to close body")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.WithStack(&ResponseError{
			Message:            "request failed",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
		})
	}
	var response struct {
		Errors []struct {
			Message string `json:"message"`
		}
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body.Bytes(), &response); err != nil {
		return nil, errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
			NextError:          err,
		})
	}

	if size := len(response.Errors); size > 0 {
		errorStrings := make([]string, size)
		for i := 0; i < size; i++ {
			errorStrings[i] = response.Errors[i].Message
		}
		return nil, GraphQLErrors{Errors: errorStrings}
	}

	return response.Data, nil
}

type AccessToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func GetAccessToken(
	ctx context.Context,
	client *http.Client,
	appID int64,
	privateKey []byte,
	repository *common.Repository,
	installationID int64,
) (*AccessToken, error) {
	var body bytes.Buffer
	err := json.NewEncoder(&body).Encode(struct {
		Repository  string `json:"repository"`
		Permissions struct {
			PullRequests string `json:"pull_requests"`
			Contents     string `json:"contents"`
			Workflows    string `json:"workflows"`
		}
	}{
		Repository: repository.FullName,
		Permissions: struct {
			PullRequests string `json:"pull_requests"`
			Contents     string `json:"contents"`
			Workflows    string `json:"workflows"`
		}{
			PullRequests: "write",
			Contents:     "write",
			Workflows:    "write",
		},
	})
	if err != nil {
		return nil, errors.Wrap(err, "unable to create body")
	}

	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID),
		&body,
	)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create request")
	}

	const maxIssueTime = time.Minute * 2
	iss := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	exp := iss.Add(maxIssueTime)
	claims := &jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(iss),
		ExpiresAt: jwt.NewNumericDate(exp),
		Issuer:    strconv.FormatInt(appID, 10),
	}
	bearer := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

	key, err := jwt.ParseRSAPrivateKeyFromPEM(privateKey)
	if err != nil {
		return nil, errors.Wrap(err, "could not parse private key")
	}

	ss, err := bearer.SignedString(key)
	if err != nil {
		return nil, errors.Wrap(err, "could not sign jwt")
	}

	r.Header.Set("Authorization", "Bearer "+ss)
	r.Header.Add("Accept", "application/vnd.github+json")
	r.Header.Add("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(r)
	if err != nil {
		return nil, errors.Wrap(err, "unable to execute request")
	}
	defer resp.Body.Close()

	body.Reset()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, errors.Wrap(err, "unable to copy body")
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, errors.WithStack(&ResponseError{
			Message:            "error when getting access token",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusCreated,
			Body:               body.String(),
		})
	}

	var token AccessToken
	if err := json.Unmarshal(buf, &token); err != nil {
		return nil, errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusCreated,
			Body:               body.String(),
			NextError:          err,
		})
	}

	return &token, nil
}

func MergePullRequest(
	ctx context.Context,
	client *http.Client,
	token,
	pullRequestID,
	expectedHeadOid,
	mergeStrategy string,
) error {
	_, err := doGraphQLRequest(ctx, client, token, `
mutation MergePulLRequest($pullRequestId: ID!, $expectedHeadOid: GitObjectID!, $mergeMethod: PullRequestMergeMethod!){ 
  mergePullRequest(input: {
    pullRequestId: $pullRequestId,
    expectedHeadOid: $expectedHeadOid,
    mergeMethod: $mergeMethod,
  }) {
    clientMutationId
  }
}
`, map[string]any{
		"pullRequestId":   pullRequestID,
		"expectedHeadOid": expectedHeadOid,
		"mergeMethod":     mergeStrategy,
	})
	if err != nil {
		return errors.Wrap(err, "unable to merge pull request")
	}
	return nil
}

func DeleteRef(ctx context.Context, client *http.Client, token, refNodeID string) error {
	_, err := doGraphQLRequest(ctx, client, token, `
mutation DeleteRef($refId: ID!){ 
  deleteRef(input: {
    refId: $refId,
  }) {
    clientMutationId
  }
}
`, map[string]any{
		"refId": refNodeID,
	})
	if err != nil {
		return errors.Wrap(err, "unable to merge pull request")
	}
	return nil
}

func UpdatePullRequest(
	ctx context.Context,
	client *http.Client,
	token string,
	pullRequestID,
	expectedHeadSha string,
) error {
	_, err := doGraphQLRequest(ctx, client, token, `
mutation UpdatePullRequestBranch($pullRequestId: ID!, $expectedHeadOid: GitObjectID!){ 
  updatePullRequestBranch(input: {
    pullRequestId: $pullRequestId,
    expectedHeadOid: $expectedHeadOid,
  }) {
    clientMutationId
  }
}
`, map[string]any{
		"pullRequestId":   pullRequestID,
		"expectedHeadOid": expectedHeadSha,
	})
	if err != nil {
		return errors.Wrap(err, "unable to update pull request")
	}
	return nil
}

func GetPullRequestsThatAreOpenAndHaveOneOfTheseLabels(
	ctx context.Context,
	client *http.Client,
	token string,
	repository *common.Repository,
	labels []string,
) ([]common.PullRequest, error) {
	var after string
	var pullRequests []common.PullRequest
	for {
		var response struct {
			Search struct {
				Nodes []struct {
					Number int64 `json:"number"`
				} `json:"nodes"`
				PageInfo struct {
					EndCursor   string `json:"endCursor"`
					HasNextPage bool   `json:"hasNextPage"`
				} `json:"pageInfo"`
			} `json:"search"`
		}

		query := `
query GetPullRequests($query: String!, $after: String){
  search(query: $query, type:ISSUE, first: 100, after: $after){
    nodes{
      ... on PullRequest {
        id
        number
        state
      }
    }
    pageInfo{
      endCursor
      hasNextPage
    }
  }
}`

		buf, err := doGraphQLRequest(ctx, client, token, query, struct {
			After string `json:"after,omitempty"`
			Query string `json:"query"`
		}{
			After: after,
			Query: fmt.Sprintf("repo:%s is:pr state:open label:%s", repository.FullName, strings.Join(labels, ",")),
		})
		if err != nil {
			return nil, errors.Wrap(err, "unable to get pull requests")
		}

		if err := json.Unmarshal(buf, &response); err != nil {
			return nil, errors.WithStack(&ResponseError{
				Message:            "unable to decode body",
				ExpectedStatusCode: http.StatusOK,
				Body:               string(buf),
				NextError:          err,
			})
		}

		for i := range response.Search.Nodes {
			pullRequests = append(pullRequests, response.Search.Nodes[i])
		}
		if !response.Search.PageInfo.HasNextPage {
			break
		}
		after = response.Search.PageInfo.EndCursor
	}

	return pullRequests, nil
}

type PullRequestDetails struct {
	AheadBy        int
	ApprovedBy     []string
	Author         string
	BaseRefName    string
	CheckStates    map[string]string
	HasConflicts   bool
	HeadRefID      string
	HeadRefName    string
	ID             string
	IsMergeable    bool
	Labels         []string
	LastCommitSha  string
	LastCommitTime time.Time
	State          string
	Title          string
}

func getPullRequestBaseName(ctx context.Context, client *http.Client, token string, repo *common.Repository, number int64) (string, error) {
	var response struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					BaseRef struct {
						Name string `json:"name"`
					} `json:"baseRef"`
				} `json:"pullRequest" graphql:"pullRequest(number: $number)"`
			} `json:"repository" graphql:"repository(owner: $owner, name: $name)"`
		} `graphql:"query GetPullRequestBaseName($owner: String!, $name: String!, $number: Int!)"`
	}

	query, err := gengraphql.Generate(&response, nil)
	if err != nil {
		return "", errors.Wrap(err, "unable to build query")
	}

	buf, err := doGraphQLRequest(ctx, client, token, query, map[string]any{
		"owner":  repo.OwnerName,
		"name":   repo.Name,
		"number": number,
	})
	if err != nil {
		return "", errors.Wrap(err, "unable to get latest pull request details")
	}

	if err := json.Unmarshal(buf, &response.Data); err != nil {
		return "", errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ExpectedStatusCode: http.StatusOK,
			Body:               string(buf),
			NextError:          err,
		})
	}

	return response.Data.Repository.PullRequest.BaseRef.Name, nil
}

func GetPullRequestDetails(
	ctx context.Context,
	client *http.Client,
	token string,
	repo *common.Repository,
	number int64,
) (*PullRequestDetails, error) {
	baseName, err := getPullRequestBaseName(ctx, client, token, repo, number)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get base name")
	}
	var response struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					Author struct {
						Login string `json:"login"`
					} `json:"author"`
					Commits struct {
						Nodes []struct {
							Commit struct {
								CheckSuites struct {
									Nodes []struct {
										App struct {
											Name string `json:"name"`
										} `json:"app"`
										CheckRuns struct {
											Nodes []struct {
												Conclusion string `json:"conclusion"`
												Name       string `json:"name"`
												Status     string `json:"status"`
											} `json:"nodes"`
										} `json:"checkRuns" graphql:"checkRuns(last:100)"`
										Conclusion string `json:"conclusion"`
									} `json:"nodes"`
								} `json:"checkSuites" graphql:"checkSuites(last:100)"`
								CommittedDate string `json:"committedDate"`
								Oid           string `json:"oid"`
								Status        struct {
									Contexts []struct {
										Context string `json:"context"`
										State   string `json:"state"`
									} `json:"contexts"`
								} `json:"status"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits" graphql:"commits(last:1)"`
					HeadRef struct {
						Compare struct {
							AheadBy int `json:"aheadBy"`
						} `json:"compare" graphql:"compare(headRef: $branch)"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"headRef"`
					ID     string `json:"id"`
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels" graphql:"labels(last: 100)"`
					Mergeable string `json:"mergeable"`
					State     string `json:"state"`
					Title     string `json:"title"`
					Reviews   struct {
						Nodes []struct {
							Author struct {
								Login string `json:"login"`
							} `json:"author"`
						} `json:"nodes"`
					} `json:"reviews" graphql:"reviews(states: APPROVED, last: 100)"`
				} `json:"pullRequest" graphql:"pullRequest(number: $number)"`
			} `json:"repository" graphql:"repository(owner: $owner, name: $name)"`
		} `graphql:"query GetPullRequestDetails($owner: String!, $name: String!, $number: Int!, $branch: String!)"`
	}

	query, err := gengraphql.Generate(&response, nil)
	if err != nil {
		return nil, errors.Wrap(err, "unable to build query")
	}

	buf, err := doGraphQLRequest(ctx, client, token, query, map[string]any{
		"owner":  repo.OwnerName,
		"name":   repo.Name,
		"number": number,
		"branch": baseName,
	})
	if err != nil {
		return nil, errors.Wrap(err, "unable to get latest pull request details")
	}

	if err := json.Unmarshal(buf, &response.Data); err != nil {
		return nil, errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ExpectedStatusCode: http.StatusOK,
			Body:               string(buf),
			NextError:          err,
		})
	}

	details := &PullRequestDetails{
		AheadBy:      response.Data.Repository.PullRequest.HeadRef.Compare.AheadBy,
		ApprovedBy:   make([]string, len(response.Data.Repository.PullRequest.Reviews.Nodes)),
		Author:       response.Data.Repository.PullRequest.Author.Login,
		BaseRefName:  baseName,
		HasConflicts: response.Data.Repository.PullRequest.Mergeable == "CONFLICTING",
		HeadRefID:    response.Data.Repository.PullRequest.HeadRef.ID,
		HeadRefName:  response.Data.Repository.PullRequest.HeadRef.Name,
		ID:           response.Data.Repository.PullRequest.ID,
		IsMergeable:  response.Data.Repository.PullRequest.Mergeable == "MERGEABLE",
		Labels:       make([]string, len(response.Data.Repository.PullRequest.Labels.Nodes)),
		State:        response.Data.Repository.PullRequest.State,
		Title:        response.Data.Repository.PullRequest.Title,
	}

	for i := range response.Data.Repository.PullRequest.Reviews.Nodes {
		details.ApprovedBy[i] = response.Data.Repository.PullRequest.Reviews.Nodes[i].Author.Login
	}

	for i := range response.Data.Repository.PullRequest.Labels.Nodes {
		details.Labels[i] = response.Data.Repository.PullRequest.Labels.Nodes[i].Name
	}

	if len(response.Data.Repository.PullRequest.Commits.Nodes) != 0 {
		commit := &response.Data.Repository.PullRequest.Commits.Nodes[0].Commit
		details.LastCommitSha = commit.Oid
		details.LastCommitTime, err = time.Parse(time.RFC3339, commit.CommittedDate)
		if err != nil {
			return nil, errors.Wrap(err, "unable to parse date")
		}

		details.CheckStates = make(map[string]string)

		for _, c := range commit.Status.Contexts {
			details.CheckStates[c.Context] = c.State
		}

		for _, node := range commit.CheckSuites.Nodes {
			details.CheckStates[node.App.Name] = node.Conclusion
			for _, run := range node.CheckRuns.Nodes {
				if run.Status == "COMPLETED" {
					details.CheckStates[node.App.Name+"/"+run.Name] = run.Conclusion
				} else {
					details.CheckStates[node.App.Name+"/"+run.Name] = "PENDING"
				}
			}
		}
	}

	return details, nil
}

func GetLatestBaseCommitSha(ctx context.Context, client *http.Client, token string, repo *common.Repository) (string, error) {
	var response struct {
		Data struct {
			Repository struct {
				DefaultBranchRef struct {
					Target struct {
						Oid string `json:"oid"`
					} `json:"target"`
				} `json:"defaultBranchRef"`
			} `json:"repository" graphql:"repository(owner: $owner, name: $name)"`
		} `graphql:"query GetLatestBaseCommitSha($owner: String!, $name: String!)"`
	}

	query, err := gengraphql.Generate(&response, nil)
	if err != nil {
		return "", errors.Wrap(err, "unable to build query")
	}

	buf, err := doGraphQLRequest(ctx, client, token, query, map[string]any{
		"owner": repo.OwnerName,
		"name":  repo.Name,
	})
	if err != nil {
		return "", errors.Wrap(err, "unable to get latest commit sha for default branch")
	}

	if err := json.Unmarshal(buf, &response.Data); err != nil {
		return "", errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ExpectedStatusCode: http.StatusOK,
			Body:               string(buf),
			NextError:          err,
		})
	}

	return response.Data.Repository.DefaultBranchRef.Target.Oid, nil
}

func GetConfig(
	ctx context.Context,
	client *http.Client,
	token string,
	repository *common.Repository,
	sha string,
) ([]byte, error) {
	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/.github/merge-with-label.yml", repository.FullName, sha),
		http.NoBody,
	)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create request")
	}

	r.Header.Add("Accept", "application/vnd.github.raw")
	r.Header.Add("X-GitHub-Api-Version", "2022-11-28")
	r.Header.Add("Authorization", "Bearer "+token)

	resp, err := client.Do(r)
	if err != nil {
		return nil, errors.Wrap(err, "unable to execute request")
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, errors.Wrap(err, "unable to copy body")
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, errors.WithStack(&ResponseError{
			Message:            "error when getting config",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               string(buf),
		})
	}
	return buf, nil
}

func CreateCheckRun(
	ctx context.Context,
	client *http.Client,
	token string,
	repo *common.Repository,
	sha,
	status,
	name,
	title,
	summary string,
) (string, error) {
	buf, err := doGraphQLRequest(ctx, client, token, `
mutation CreateCheckRun(
  $repositoryId: ID!,
  $sha: GitObjectID!,
  $status: RequestableCheckStatusState!,
  $name: String!,
  $title: String!,
  $summary: String!
){
  createCheckRun(input: {
    repositoryId: $repositoryId,
    headSha: $sha,
    status: $status,
    name: $name,
    conclusion: NEUTRAL,
    output: {
      title: $title
      summary: $summary
    }
  }) {
    clientMutationId
  }
}
`, map[string]any{
		"repositoryId": repo.NodeID,
		"sha":          sha,
		"status":       status,
		"name":         name,
		"title":        title,
		"summary":      summary,
	})
	if err != nil {
		return "", errors.Wrap(err, "unable to create check run")
	}

	var response struct {
		ClientMutationID string `json:"clientMutationId"`
	}
	if err := json.Unmarshal(buf, &response); err != nil {
		return "", errors.WithStack(&ResponseError{
			Message:   "unable to decode body",
			Body:      string(buf),
			NextError: err,
		})
	}

	return response.ClientMutationID, nil
}

func UpdateCheckRun(
	ctx context.Context,
	client *http.Client,
	token string,
	repo *common.Repository,
	checkRunID,
	status,
	name,
	title,
	summary string,
) (string, error) {
	buf, err := doGraphQLRequest(ctx, client, token, `
mutation UpdateCheckRun(
  $checkRunId: ID!,
  $repositoryId: ID!,
  $status: RequestableCheckStatusState!,
  $name: String!,
  $title: String!,
  $summary: String!
){
  updateCheckRun(input: {
    checkRunId: $checkRunId,
    repositoryId: $repositoryId,
    status: $status,
    name: $name,
    conclusion: NEUTRAL,
    output: {
      title: $title
      summary: $summary
    }
  }) {
    clientMutationId
  }
}
`, map[string]any{
		"checkRunId":   checkRunID,
		"repositoryId": repo.NodeID,
		"status":       status,
		"name":         name,
		"title":        title,
		"summary":      summary,
	})
	if err != nil {
		return "", errors.Wrap(err, "unable to update check run")
	}

	var response struct {
		ClientMutationID string `json:"clientMutationId"`
	}
	if err := json.Unmarshal(buf, &response); err != nil {
		return "", errors.WithStack(&ResponseError{
			Message:   "unable to decode body",
			Body:      string(buf),
			NextError: err,
		})
	}

	return response.ClientMutationID, nil
}
