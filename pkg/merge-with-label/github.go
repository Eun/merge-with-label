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
		return errors.WithStack(err)
	}
	defer resp.Body.Close()

	body.Reset()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return errors.WithStack(err)
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

func GetPullRequestsThatNeedToBeUpdated(ctx context.Context, client *http.Client, token string, repo *Repository) ([]int, error) {
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
				Query:  fmt.Sprintf("repo:%s is:pr state:open label:auto-update", repo.FullName),
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
			return nil, fmt.Errorf("error during query: %s", litter.Sdump(response.Errors))
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

func GetPullRequestDetails(ctx context.Context, client *http.Client, token string, repo *Repository, number int) (string, int, error) {
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
      state
      headRef {
        compare(headRef: $branch) {
            aheadBy
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
		return "", 0, errors.Wrap(err, "unable to create body")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", &body)
	if err != nil {
		return "", 0, errors.Wrap(err, "unable to create request")
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, errors.Wrap(err, "unable to execute request")
	}
	body.Reset()
	_, err = io.Copy(&body, io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", 0, errors.Wrap(err, "unable to copy body")
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, errors.WithStack(&ResponseError{
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
					State   string `json:"state"`
					HeadRef struct {
						Compare struct {
							AheadBy int `json:"aheadBy"`
						} `json:"compare"`
					} `json:"headRef"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body.Bytes(), &response); err != nil {
		return "", 0, errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
			NextError:          err,
		})
	}

	if len(response.Errors) > 0 {
		return "", 0, fmt.Errorf("error during query: %s", litter.Sdump(response.Errors))
	}

	return response.Data.Repository.PullRequest.State, response.Data.Repository.PullRequest.HeadRef.Compare.AheadBy, nil
}

func GetCheckSuiteStatusForPullRequest(ctx context.Context, client *http.Client, token string, repo *Repository, number int) (string, time.Time, error) {
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
query GetCheckSuiteStatusCheckRollup($owner: String!, $name: String!, $number: Int!){ 
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      commits(last: 1) {
        nodes {     
          commit {
            committedDate
            statusCheckRollup {
              state
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
		},
	})

	if err != nil {
		return "", time.Time{}, errors.Wrap(err, "unable to create body")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", &body)
	if err != nil {
		return "", time.Time{}, errors.Wrap(err, "unable to create request")
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, errors.Wrap(err, "unable to execute request")
	}
	body.Reset()
	_, err = io.Copy(&body, io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", time.Time{}, errors.Wrap(err, "unable to copy body")
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, errors.WithStack(&ResponseError{
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
			Repository struct {
				PullRequest struct {
					Commits struct {
						Nodes []struct {
							Commit struct {
								CommittedDate     time.Time `json:"committedDate"`
								StatusCheckRollup struct {
									State string `json:"state"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body.Bytes(), &response); err != nil {
		return "", time.Time{}, errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               body.String(),
			NextError:          err,
		})
	}

	if len(response.Errors) > 0 {
		return "", time.Time{}, errors.Errorf("error during query: %s", litter.Sdump(response.Errors))
	}

	if len(response.Data.Repository.PullRequest.Commits.Nodes) == 0 {
		return "", time.Time{}, nil
	}
	return response.Data.Repository.PullRequest.Commits.Nodes[0].Commit.StatusCheckRollup.State,
		response.Data.Repository.PullRequest.Commits.Nodes[0].Commit.CommittedDate,
		nil
}
