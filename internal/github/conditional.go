// Cheap change detection: conditional REST requests. GitHub answers an
// unchanged one with 304 Not Modified, which costs nothing against the rate
// limit, so a quiet repo can be checked often for free. Only the PRs that
// moved are then fetched in full over GraphQL.

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/readiness"
)

// RESTURL is GitHub's REST root; SetRESTURL points it elsewhere (tests).
const RESTURL = "https://api.github.com"

// OpenPRs is what one conditional look at a repo's open PRs found.
type OpenPRs struct {
	// False: GitHub said 304, so nothing in the list changed.
	Changed bool
	ETag    string
	// Open PR numbers, newest first, and when each last changed.
	Numbers []int
	Updated map[int]string
}

func (c *Client) SetRESTURL(url string) { c.restURL = url }

func (c *Client) restRoot() string {
	if c.restURL != "" {
		return c.restURL
	}
	return RESTURL
}

// conditional GETs a REST path, sending etag; 304 comes back as changed=false.
func (c *Client) conditional(ctx context.Context, path, etag string) (
	changed bool, body []byte, newETag string, err error) {
	for attempt := range 2 {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.restRoot()+path, nil)
		if err != nil {
			return false, nil, "", &Error{Message: err.Error()}
		}
		request.Header.Set("Authorization", "Bearer "+c.currentToken())
		request.Header.Set("Accept", "application/vnd.github+json")
		if etag != "" {
			request.Header.Set("If-None-Match", etag)
		}
		response, err := c.http.Do(request)
		if err != nil {
			return false, nil, "", &Error{Message: fmt.Sprintf("Network error: %v", err)}
		}
		data, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			return false, nil, "", &Error{Message: fmt.Sprintf("Network error: %v", readErr)}
		}
		if response.StatusCode == http.StatusNotModified {
			return false, nil, etag, nil
		}
		if problem := httpProblem(response, data); problem != nil {
			// The token may have been replaced since the daemon started.
			var authProblem *AuthError
			if attempt == 0 && asAuth(problem, &authProblem) {
				if refreshErr := c.refreshToken(); refreshErr != nil {
					return false, nil, "", refreshErr
				}
				continue
			}
			return false, nil, "", problem
		}
		return true, data, response.Header.Get("ETag"), nil
	}
	return false, nil, "", &Error{Message: "GitHub kept rejecting the token"}
}

func asAuth(err error, target **AuthError) bool {
	problem, ok := err.(*AuthError)
	if ok {
		*target = problem
	}
	return ok
}

// ListOpenPRs asks which PRs are open and when each last changed: one request,
// free while the answer is the same as last time.
func (c *Client) ListOpenPRs(ctx context.Context, name, etag string) (OpenPRs, error) {
	owner, repo, err := splitRepo(name)
	if err != nil {
		return OpenPRs{}, err
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=open&sort=created&direction=desc&per_page=%d",
		owner, repo, PRLimit)
	changed, body, newETag, err := c.conditional(ctx, path, etag)
	if err != nil || !changed {
		return OpenPRs{Changed: false, ETag: newETag}, err
	}
	var listed []struct {
		Number    int    `json:"number"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		return OpenPRs{}, &Error{Message: fmt.Sprintf("GitHub sent an unreadable PR list: %v", err)}
	}
	found := OpenPRs{Changed: true, ETag: newETag, Updated: map[int]string{}}
	for _, entry := range listed {
		found.Numbers = append(found.Numbers, entry.Number)
		found.Updated[entry.Number] = entry.UpdatedAt
	}
	return found, nil
}

// BaseMoved reports whether the repo's default branch has new commits, which
// changes how every PR merges without touching any of them.
func (c *Client) BaseMoved(ctx context.Context, name, etag string) (bool, string, error) {
	owner, repo, err := splitRepo(name)
	if err != nil {
		return false, "", err
	}
	path := fmt.Sprintf("/repos/%s/%s/commits?per_page=1", owner, repo)
	changed, _, newETag, err := c.conditional(ctx, path, etag)
	return changed, newETag, err
}

// FetchPRs returns full details for named PRs: what the poll spends its rate
// limit on, now only for the ones that moved.
func (c *Client) FetchPRs(ctx context.Context, name string, numbers []int) ([]models.PullRequest, error) {
	owner, repo, err := splitRepo(name)
	if err != nil {
		return nil, err
	}
	nodes, err := c.fetchPages(ctx, map[string]any{"owner": owner, "name": repo}, numbers)
	if err != nil {
		return nil, err
	}
	prs := []models.PullRequest{}
	for _, number := range numbers {
		node, have := nodes[number]
		if !have || node.State != "OPEN" {
			continue
		}
		prs = append(prs, readiness.Assess(node.toPullRequest()))
	}
	return prs, nil
}

func splitRepo(name string) (string, string, error) {
	owner, repo, found := strings.Cut(name, "/")
	if !found || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", &NotFoundError{Message: fmt.Sprintf("Expected owner/name, got %q", name)}
	}
	return owner, repo, nil
}

// RerunFailedJobs asks GitHub Actions to run the failed jobs of these runs
// again. Runs are named by the repo the workflow belongs to, which for a PR is
// the one being merged into.
func (c *Client) RerunFailedJobs(ctx context.Context, name string, runs []int) error {
	owner, repo, err := splitRepo(name)
	if err != nil {
		return err
	}
	for _, run := range runs {
		path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/rerun-failed-jobs", owner, repo, run)
		if err := c.restPost(ctx, path); err != nil {
			return err
		}
	}
	return nil
}

// restPost sends an empty POST, refreshing the token once if GitHub refuses it.
func (c *Client) restPost(ctx context.Context, path string) error {
	for attempt := range 2 {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.restRoot()+path, nil)
		if err != nil {
			return &Error{Message: err.Error()}
		}
		request.Header.Set("Authorization", "Bearer "+c.currentToken())
		request.Header.Set("Accept", "application/vnd.github+json")
		response, err := c.http.Do(request)
		if err != nil {
			return &Error{Message: fmt.Sprintf("Network error: %v", err)}
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode < 300 {
			return nil
		}
		problem := httpProblem(response, body)
		var authProblem *AuthError
		if attempt == 0 && asAuth(problem, &authProblem) {
			if refreshErr := c.refreshToken(); refreshErr != nil {
				return refreshErr
			}
			continue
		}
		if problem == nil {
			problem = &Error{Message: fmt.Sprintf("GitHub API error %d", response.StatusCode)}
		}
		return problem
	}
	return &Error{Message: "GitHub kept rejecting the token"}
}

// Stale is how long a repo goes without a full fetch, however quiet the
// conditional checks say it is.
const Stale = 5 * time.Minute
