package merge_with_label

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

	"github.com/golang-jwt/jwt/v4"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/sanity-io/litter"
	"gopkg.in/yaml.v3"
)

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
		sb.WriteString(e.Error())
	}

	return sb.String()
}

func (e *ResponseError) MarshalZerologObject(ev *zerolog.Event) {
	ev = ev.Str("message", e.Message)
	if e.ActualStatusCode != e.ExpectedStatusCode {
		ev = ev.Int("actual_status_code", e.ActualStatusCode)
		ev = ev.Int("expected_status_code", e.ExpectedStatusCode)
	}
	ev = ev.Err(e.NextError)
}

func GetAccessToken(ctx context.Context, client *http.Client, appID int64, privateKey []byte, repo *Repository, installationID int64) (*AccessToken, error) {
	var body bytes.Buffer
	err := json.NewEncoder(&body).Encode(struct {
		Repository  string `json:"repository"`
		Permissions struct {
			PullRequests string `json:"pull_requests"`
			Contents     string `json:"contents"`
			Workflows    string `json:"workflows"`
		}
	}{
		Repository: repo.FullName,
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

	r, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID), &body)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create request")
	}

	iss := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	exp := iss.Add(2 * time.Minute)
	claims := &jwt.StandardClaims{
		IssuedAt:  iss.Unix(),
		ExpiresAt: exp.Unix(),
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

func GetLatestCommitShaForPullRequest(ctx context.Context, client *http.Client, token string, repo *Repository, number int) (string, error) {
	var body bytes.Buffer
	type variables struct {
		Owner  string `json:"owner"`
		Name   string `json:"name"`
		Number int    `json:"number"`
	}
	err := json.NewEncoder(&body).Encode(struct {
		Query     string    `json:"query"`
		Variables variables `json:"variables"`
	}{
		Query: `
query GetLatestCommit($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      commits(last: 1) {
        nodes {
          commit {
            oid
          }
        }
      }
    }
  }
}
`,
		Variables: variables{
			Owner:  repo.Owner.Login,
			Name:   repo.Name,
			Number: number,
		},
	})

	if err != nil {
		return "", errors.Wrap(err, "unable to create body")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", &body)
	if err != nil {
		return "", errors.Wrap(err, "unable to create request")
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "unable to execute request")
	}
	body.Reset()
	_, err = io.Copy(&body, io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", errors.Wrap(err, "unable to copy body")
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.WithStack(&ResponseError{
			Message:            "error when getting latest commit",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
		})
	}
	var response struct {
		Errors []struct {
			Message string `json:"message"`
		}
		Data struct {
			Repository struct {
				PullRequest struct {
					Commits struct {
						Nodes []struct {
							Commit struct {
								Oid string `json:"oid"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body.Bytes(), &response); err != nil {
		return "", errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
			NextError:          err,
		})
	}

	if len(response.Errors) > 0 {
		return "", errors.Errorf("error during query: %s", litter.Sdump(response.Errors))
	}

	if len(response.Data.Repository.PullRequest.Commits.Nodes) == 0 {
		return "", nil
	}
	return response.Data.Repository.PullRequest.Commits.Nodes[0].Commit.Oid, nil

}

func MergePullRequest(ctx context.Context, client *http.Client, token string, repository *Repository, pullRequest *PullRequest) error {
	var body bytes.Buffer

	if err := json.NewEncoder(&body).Encode(struct {
		CommitTitle   string `json:"commit_title"`
		CommitMessage string `json:"commit_message"`
		MergeMethod   string `json:"merge_method"`
	}{
		CommitTitle:   pullRequest.Title,
		CommitMessage: "",
		MergeMethod:   "squash",
	}); err != nil {
		return errors.Wrap(err, "unable to create body")
	}

	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d/merge", repository.FullName, pullRequest.Number),
		&body,
	)
	if err != nil {
		return errors.Wrap(err, "unable to create request")
	}

	r.Header.Add("Accept", "application/vnd.github+json")
	r.Header.Add("X-GitHub-Api-Version", "2022-11-28")
	r.Header.Add("Authorization", "Bearer "+token)

	resp, err := client.Do(r)
	if err != nil {
		return errors.Wrap(err, "unable to execute request")
	}
	defer resp.Body.Close()

	body.Reset()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return errors.Wrap(err, "unable to copy body")
	}

	if resp.StatusCode == http.StatusMethodNotAllowed {
		var mergeResponse struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(buf, &mergeResponse); err != nil {
			return errors.WithStack(&ResponseError{
				Message:            "unable to decode body",
				ActualStatusCode:   resp.StatusCode,
				ExpectedStatusCode: http.StatusOK,
				Body:               body.String(),
				NextError:          err,
			})
		}
		if mergeResponse.Message == "Base branch was modified. Review and try the merge again." ||
			mergeResponse.Message == "Pull Request is not mergeable" {
			return errors.New(mergeResponse.Message)
		}
	}

	if resp.StatusCode != http.StatusOK {
		return errors.WithStack(&ResponseError{
			Message:            "error when performing merge",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               string(buf),
		})
	}

	var mergeResponse MergeResponse
	if err := json.Unmarshal(buf, &mergeResponse); err != nil {
		return errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
			NextError:          err,
		})
	}

	if !mergeResponse.Merged {
		return errors.New("pr was not merged")
	}
	return nil
}

func UpdatePullRequest(ctx context.Context, client *http.Client, token string, repo *Repository, number int) error {
	sha, err := GetLatestCommitShaForPullRequest(ctx, client, token, repo, number)
	if err != nil {
		return errors.Wrap(err, "unable to get latest commit sha")
	}
	if sha == "" {
		return nil
	}

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(struct {
		ExpectedHeadSha string `json:"expected_head_sha"`
	}{
		ExpectedHeadSha: sha,
	}); err != nil {
		return errors.Wrap(err, "unable to create body")
	}

	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d/update-branch", repo.FullName, number),
		&body,
	)
	if err != nil {
		return errors.WithStack(err)
	}

	r.Header.Add("Accept", "application/vnd.github+json")
	r.Header.Add("X-GitHub-Api-Version", "2022-11-28")
	r.Header.Add("Authorization", "Bearer "+token)

	resp, err := client.Do(r)
	if err != nil {
		return errors.Wrap(err, "unable to execute request")
	}
	defer resp.Body.Close()

	body.Reset()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return errors.Wrap(err, "unable to copy body")
	}

	if resp.StatusCode == http.StatusUnprocessableEntity {
		return errors.New("update failed")
	}

	if resp.StatusCode != http.StatusAccepted {
		return errors.WithStack(&ResponseError{
			Message:            "error when performing update",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusAccepted,
			Body:               string(buf),
		})
	}
	return nil
}

func GetPullRequestsThatNeedToBeUpdated(ctx context.Context, client *http.Client, token string, repo *Repository, updateLabel string) ([]int, error) {
	var after string
	var body bytes.Buffer
	var pullRequests []int
	type variables struct {
		After  string `json:"after,omitempty"`
		Query  string `json:"query"`
		Branch string `json:"branch"`
	}
	for {
		err := json.NewEncoder(&body).Encode(struct {
			Query     string    `json:"query"`
			Variables variables `json:"variables"`
		}{
			Query: `
query GetPullRequests($query: String!, $branch: String!, $after: String) {
  search(query: $query, type:ISSUE, first: 100, after: $after) {
    pageInfo {
      hasNextPage
      endCursor
    }
    nodes {
      ... on PullRequest {
        number
        headRef {
          compare(headRef: $branch) {
            aheadBy
          }
        }
      }
    }
  }
}
`,
			Variables: variables{
				After:  after,
				Query:  fmt.Sprintf("repo:%s is:pr state:open label:%s", repo.FullName, updateLabel),
				Branch: repo.DefaultBranch,
			},
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
		if resp.StatusCode != http.StatusOK {
			return nil, errors.WithStack(&ResponseError{
				Message:            "error when getting pull requests",
				ActualStatusCode:   resp.StatusCode,
				ExpectedStatusCode: http.StatusOK,
				Body:               body.String(),
			})
		}
		var response struct {
			Errors []struct {
				Message string `json:"message"`
			}
			Data struct {
				Search struct {
					PageInfo struct {
						EndCursor   string `json:"endCursor"`
						HasNextPage bool   `json:"hasNextPage"`
					} `json:"pageInfo"`
					Nodes []struct {
						URL     string `json:"url"`
						Number  int    `json:"number"`
						HeadRef struct {
							Compare struct {
								AheadBy int `json:"aheadBy"`
							} `json:"compare"`
						} `json:"headRef"`
					} `json:"nodes"`
				} `json:"search"`
			} `json:"data"`
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

		if len(response.Errors) > 0 {
			return nil, errors.Errorf("error during query: %s", litter.Sdump(response.Errors))
		}

		for _, node := range response.Data.Search.Nodes {
			if node.HeadRef.Compare.AheadBy == 0 {
				continue
			}
			pullRequests = append(pullRequests, node.Number)
		}
		body.Reset()
		if !response.Data.Search.PageInfo.HasNextPage {
			break
		}
		after = response.Data.Search.PageInfo.EndCursor
	}

	return pullRequests, nil
}

type PullRequestDetails struct {
	Title          string
	AheadBy        int
	State          string
	Author         string
	ApprovedCount  int
	LastCommitSha  string
	LastCommitTime time.Time
	CheckStates    map[string]string
	Labels         []string
}

func GetPullRequestDetails(ctx context.Context, client *http.Client, token string, repo *Repository, number int) (*PullRequestDetails, error) {
	var body bytes.Buffer
	type variables struct {
		Owner  string `json:"owner"`
		Name   string `json:"name"`
		Number int    `json:"number"`
		Branch string `json:"branch"`
	}
	err := json.NewEncoder(&body).Encode(struct {
		Query     string    `json:"query"`
		Variables variables `json:"variables"`
	}{
		Query: `
query GetPullRequestDetails($owner: String!, $name: String!, $number: Int!, $branch: String!){ 
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      title
      state
      headRef {
        compare(headRef: $branch) {
            aheadBy
        }
      }
      author {
        login
      }
      reviews(states: APPROVED, last: 100) {
        totalCount
      }
      labels(last: 100) {
        nodes {
          name
        }
      }
      commits(last: 1) {
        nodes {
          commit {
            oid
            committedDate
            status {
              contexts {
                state
                context
              }
            }
            checkSuites(last: 100) {
              nodes {
                app {
                  name
                }
                conclusion
                checkRuns(last:100) {
                  nodes {
                    name
                    conclusion
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}
`,
		Variables: variables{
			Owner:  repo.Owner.Login,
			Name:   repo.Name,
			Number: number,
			Branch: repo.DefaultBranch,
		},
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
	if resp.StatusCode != http.StatusOK {
		return nil, errors.WithStack(&ResponseError{
			Message:            "error when getting pull request details",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
		})
	}
	var response struct {
		Errors []struct {
			Message string `json:"message"`
		}
		Data struct {
			Repository struct {
				PullRequest struct {
					Title   string `json:"title"`
					State   string `json:"state"`
					HeadRef struct {
						Compare struct {
							AheadBy int `json:"aheadBy"`
						} `json:"compare"`
					} `json:"headRef"`
					Author struct {
						Login string `json:"login"`
					} `json:"author"`
					Reviews struct {
						TotalCount int `json:"totalCount"`
					}
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
					Commits struct {
						Nodes []struct {
							Commit struct {
								Oid           string    `json:"oid"`
								CommittedDate time.Time `json:"committedDate"`
								Status        struct {
									Contexts []struct {
										Context string `json:"context"`
										State   string `json:"state"`
									} `json:"contexts"`
								} `json:"status"`
								CheckSuites struct {
									Nodes []struct {
										App struct {
											Name string
										} `json:"app"`
										Conclusion string `json:"conclusion"`
										CheckRuns  struct {
											Nodes []struct {
												Name       string `json:"name"`
												Conclusion string `json:"conclusion"`
											} `json:"nodes"`
										} `json:"checkRuns"`
									} `json:"nodes"`
								} `json:"checkSuites"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
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

	if len(response.Errors) > 0 {
		return nil, errors.Errorf("error during query: %s", litter.Sdump(response.Errors))
	}

	details := &PullRequestDetails{
		Title:         response.Data.Repository.PullRequest.Title,
		AheadBy:       response.Data.Repository.PullRequest.HeadRef.Compare.AheadBy,
		State:         response.Data.Repository.PullRequest.State,
		Author:        response.Data.Repository.PullRequest.Author.Login,
		ApprovedCount: response.Data.Repository.PullRequest.Reviews.TotalCount,
		Labels:        make([]string, len(response.Data.Repository.PullRequest.Labels.Nodes)),
	}

	for i := range response.Data.Repository.PullRequest.Labels.Nodes {
		details.Labels[i] = response.Data.Repository.PullRequest.Labels.Nodes[i].Name
	}

	if len(response.Data.Repository.PullRequest.Commits.Nodes) != 0 {
		commit := &response.Data.Repository.PullRequest.Commits.Nodes[0].Commit
		details.LastCommitSha = commit.Oid
		details.LastCommitTime = commit.CommittedDate

		details.CheckStates = make(map[string]string)

		for _, c := range commit.Status.Contexts {
			details.CheckStates[c.Context] = c.State
		}

		for _, node := range commit.CheckSuites.Nodes {
			details.CheckStates[node.App.Name] = node.Conclusion
			for _, run := range node.CheckRuns.Nodes {
				details.CheckStates[node.App.Name+"/"+run.Name] = run.Conclusion
			}
		}
	}

	return details, nil
}

func GetLatestCommitSha(ctx context.Context, client *http.Client, token string, repo *Repository) (string, error) {
	var body bytes.Buffer
	type variables struct {
		Owner string `json:"owner"`
		Name  string `json:"name"`
	}
	err := json.NewEncoder(&body).Encode(struct {
		Query     string    `json:"query"`
		Variables variables `json:"variables"`
	}{
		Query: `
query GetLatestCommitSha($owner: String!, $name: String!){ 
  repository(owner: $owner, name: $name) {
    defaultBranchRef {
      target {
        oid
      }
    }
  }
}
`,
		Variables: variables{
			Owner: repo.Owner.Login,
			Name:  repo.Name,
		},
	})

	if err != nil {
		return "", errors.Wrap(err, "unable to create body")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", &body)
	if err != nil {
		return "", errors.Wrap(err, "unable to create request")
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "unable to execute request")
	}
	body.Reset()
	_, err = io.Copy(&body, io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", errors.Wrap(err, "unable to copy body")
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.WithStack(&ResponseError{
			Message:            "error when getting latest commit sha",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
		})
	}
	var response struct {
		Errors []struct {
			Message string `json:"message"`
		}
		Data struct {
			Repository struct {
				DefaultBranchRef struct {
					Target struct {
						Oid string `json:"oid"`
					} `json:"target"`
				} `json:"defaultBranchRef"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body.Bytes(), &response); err != nil {
		return "", errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
			NextError:          err,
		})
	}

	if len(response.Errors) > 0 {
		return "", errors.Errorf("error during query: %s", litter.Sdump(response.Errors))
	}

	return response.Data.Repository.DefaultBranchRef.Target.Oid, nil
}

func GetConfig(ctx context.Context, client *http.Client, token string, repo *Repository, sha string) (*ConfigV1, error) {
	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/.github/merge-with-label.yml", repo.FullName, sha),
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

	var hdr ConfigHeader
	if err := yaml.Unmarshal(buf, &hdr); err != nil {
		return nil, errors.Wrap(err, "unable to decode config header")
	}

	switch hdr.Version {
	case 1:
		var cfg ConfigV1
		if err := yaml.Unmarshal(buf, &cfg); err != nil {
			return nil, errors.Wrap(err, "unable to decode config")
		}
		return &cfg, nil
	default:
		return nil, errors.Errorf("unknown version `%d'", hdr.Version)
	}

}

func CreateCheckRun(ctx context.Context, client *http.Client, token string, repo *Repository, sha, status, name, title, summary string) (string, error) {
	var body bytes.Buffer
	type variables struct {
		RepositoryID string `json:"repositoryId"`
		Sha          string `json:"sha"`
		Status       string `json:"status"`
		Name         string `json:"name"`
		Title        string `json:"title"`
		Summary      string `json:"summary"`
	}
	err := json.NewEncoder(&body).Encode(struct {
		Query     string    `json:"query"`
		Variables variables `json:"variables"`
	}{
		Query: `
mutation CreateCheckRun($repositoryId: ID!, $sha: GitObjectID!, $status: RequestableCheckStatusState!, $name: String!, $title: String!, $summary: String!){ 
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
`,
		Variables: variables{
			RepositoryID: repo.NodeId,
			Sha:          sha,
			Status:       status,
			Name:         name,
			Title:        title,
			Summary:      summary,
		},
	})

	if err != nil {
		return "", errors.Wrap(err, "unable to create body")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", &body)
	if err != nil {
		return "", errors.Wrap(err, "unable to create request")
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "unable to execute request")
	}
	body.Reset()
	_, err = io.Copy(&body, io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", errors.Wrap(err, "unable to copy body")
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.WithStack(&ResponseError{
			Message:            "error when creating check run",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
		})
	}
	var response struct {
		Errors []struct {
			Message string `json:"message"`
		}
		Data struct {
			ClientMutationId string `json:"clientMutationId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body.Bytes(), &response); err != nil {
		return "", errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
			NextError:          err,
		})
	}

	if len(response.Errors) > 0 {
		return "", errors.Errorf("error during query: %s", litter.Sdump(response.Errors))
	}

	return response.Data.ClientMutationId, nil
}

func UpdateCheckRun(ctx context.Context, client *http.Client, token string, repo *Repository, checkRunId, sha, status, name, title, summary string) (string, error) {
	var body bytes.Buffer
	type variables struct {
		CheckRunID   string `json:"checkRunId"`
		RepositoryID string `json:"repositoryId"`
		Sha          string `json:"sha"`
		Status       string `json:"status"`
		Name         string `json:"name"`
		Title        string `json:"title"`
		Summary      string `json:"summary"`
	}
	err := json.NewEncoder(&body).Encode(struct {
		Query     string    `json:"query"`
		Variables variables `json:"variables"`
	}{
		Query: `
mutation CreateCheckRun($checkRunId: ID!, $repositoryId: ID!, $sha: GitObjectID!, $status: RequestableCheckStatusState!, $name: String!, $title: String!, $summary: String!){ 
  createCheckRun(input: {
    checkRunId: $checkRunId,
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
`,
		Variables: variables{
			CheckRunID:   checkRunId,
			RepositoryID: repo.NodeId,
			Name:         name,
			Sha:          sha,
			Status:       status,
			Title:        title,
			Summary:      summary,
		},
	})

	if err != nil {
		return "", errors.Wrap(err, "unable to create body")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", &body)
	if err != nil {
		return "", errors.Wrap(err, "unable to create request")
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "unable to execute request")
	}
	body.Reset()
	_, err = io.Copy(&body, io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", errors.Wrap(err, "unable to copy body")
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.WithStack(&ResponseError{
			Message:            "error when updating check run",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
		})
	}
	var response struct {
		Errors []struct {
			Message string `json:"message"`
		}
		Data struct {
			ClientMutationId string `json:"clientMutationId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body.Bytes(), &response); err != nil {
		return "", errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
			NextError:          err,
		})
	}

	if len(response.Errors) > 0 {
		return "", errors.Errorf("error during query: %s", litter.Sdump(response.Errors))
	}

	return response.Data.ClientMutationId, nil
}
