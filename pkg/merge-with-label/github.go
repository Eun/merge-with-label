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

	"github.com/Eun/merge-with-label/pkg/merge-with-label/internal"
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

func GetAccessToken(ctx context.Context, client *http.Client, appID int64, privateKey []byte, req *internal.Request) (*internal.AccessToken, error) {
	var body bytes.Buffer
	err := json.NewEncoder(&body).Encode(struct {
		Repository  string `json:"repository"`
		Permissions struct {
			PullRequests string `json:"pull_requests"`
			Contents     string `json:"contents"`
			Workflows    string `json:"workflows"`
		}
	}{
		Repository: req.Repository.FullName,
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

	r, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", req.Installation.ID), &body)
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

	var token internal.AccessToken
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

func GetLatestCommitShaForPullRequest(ctx context.Context, client *http.Client, accessToken *internal.AccessToken, repo *internal.Repository, number int) (string, error) {
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
			Owner:  repo.Owner.Name,
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

	req.Header.Set("Authorization", "Bearer "+accessToken.Token)

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

func MergePullRequest(ctx context.Context, client *http.Client, accessToken *internal.AccessToken, req *internal.Request, tryCounter int) error {
	var body bytes.Buffer

	if err := json.NewEncoder(&body).Encode(struct {
		CommitTitle   string `json:"commit_title"`
		CommitMessage string `json:"commit_message"`
		MergeMethod   string `json:"merge_method"`
	}{
		CommitTitle:   req.PullRequest.Title,
		CommitMessage: "",
		MergeMethod:   "squash",
	}); err != nil {
		return errors.Wrap(err, "unable to create body")
	}

	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d/merge", req.Repository.FullName, req.PullRequest.Number),
		&body,
	)
	if err != nil {
		return errors.Wrap(err, "unable to create request")
	}

	r.Header.Add("Accept", "application/vnd.github+json")
	r.Header.Add("X-GitHub-Api-Version", "2022-11-28")
	r.Header.Add("Authorization", "Bearer "+accessToken.Token)

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
			if tryCounter < 3 {
				return MergePullRequest(ctx, client, accessToken, req, tryCounter+1)
			}
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

	var mergeResponse internal.MergeResponse
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

func UpdatePullRequest(ctx context.Context, client *http.Client, accessToken *internal.AccessToken, repo *internal.Repository, number, tryCounter int) error {
	sha, err := GetLatestCommitShaForPullRequest(ctx, client, accessToken, repo, number)
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
	r.Header.Add("Authorization", "Bearer "+accessToken.Token)

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
		if tryCounter < 3 {
			return UpdatePullRequest(ctx, client, accessToken, repo, number, tryCounter+1)
		}
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

func GetPullRequestsThatNeedToBeUpdated(ctx context.Context, client *http.Client, accessToken *internal.AccessToken, req *internal.Request) ([]int, error) {
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
				Query:  fmt.Sprintf("repo:%s is:pr state:open label:auto-update", req.Repository.FullName),
				Branch: req.Repository.MasterBranch,
			},
		})

		if err != nil {
			return nil, errors.Wrap(err, "unable to create body")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", &body)
		if err != nil {
			return nil, errors.Wrap(err, "unable to create request")
		}

		req.Header.Set("Authorization", "Bearer "+accessToken.Token)

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

func AreChecksGreen(ctx context.Context, client *http.Client, accessToken *internal.AccessToken, req *internal.Request) (bool, error) {
	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/commits/%s/check-runs", req.Repository.FullName, req.PullRequest.Head.SHA),
		http.NoBody,
	)
	if err != nil {
		return false, errors.Wrap(err, "unable to create request")
	}

	r.Header.Add("Accept", "application/vnd.github+json")
	r.Header.Add("X-GitHub-Api-Version", "2022-11-28")
	r.Header.Add("Authorization", "Bearer "+accessToken.Token)

	resp, err := client.Do(r)
	if err != nil {
		return false, errors.Wrap(err, "unable to execute request")
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return false, errors.Wrap(err, "unable to copy body")
	}

	if resp.StatusCode != http.StatusOK {
		return false, errors.WithStack(&ResponseError{
			Message:            "error when checking runs",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               string(buf),
		})
	}

	var checkRunsResponse internal.CheckRunsResponse
	if err := json.Unmarshal(buf, &checkRunsResponse); err != nil {
		return false, errors.WithStack(&ResponseError{
			Message:            "unable to decode body",
			ActualStatusCode:   resp.StatusCode,
			ExpectedStatusCode: http.StatusOK,
			Body:               string(buf),
			NextError:          err,
		})
	}

	for _, run := range checkRunsResponse.CheckRuns {
		if run.Status != "completed" {
			return false, nil
		}
		switch run.Conclusion {
		case "neutral", "success", "skipped":
		default:
			return false, nil
		}
	}

	return true, nil
}
