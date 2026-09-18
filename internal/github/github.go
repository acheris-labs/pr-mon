// Package github talks to GitHub's GraphQL API: the queries pr-mon needs and
// the mutations it performs.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/readiness"
)

const (
	APIURL     = "https://api.github.com/graphql"
	PRLimit    = 50
	CheckLimit = 20
	// Issues a PR closes on merge. More than a handful is rare, and the list is
	// there to be read in a details pane, not counted.
	IssueLimit = 10
	// Mergeability fields are slow for GitHub to compute and GraphQL requests time
	// out after ~10s, so full PR details are fetched in pages of this size.
	PageSize          = 10
	RateLimitFallback = 60 * time.Second
	requestTimeout    = 30 * time.Second
)

var prFields = fmt.Sprintf(`
fragment PrFields on PullRequest {
  id number state title url createdAt isDraft
  author { login }
  headRefName baseRefName
  headRef { id }
  headRepository { nameWithOwner }
  mergeable mergeStateStatus reviewDecision
  autoMergeRequest { mergeMethod enabledBy { login } }
  closingIssuesReferences(first: %d) {
    nodes { number title url repository { nameWithOwner } }
  }
  commits(last: 1) {
    nodes {
      commit {
        oid
        committedDate
        statusCheckRollup {
          state
          contexts(first: %d) {
            totalCount
            nodes {
              __typename
              ... on CheckRun { name status conclusion startedAt }
              ... on StatusContext { context state createdAt }
            }
          }
        }
      }
    }
  }
}
`, IssueLimit, CheckLimit)

var repoQuery = fmt.Sprintf(`
query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    nameWithOwner
    mergeCommitAllowed
    squashMergeAllowed
    rebaseMergeAllowed
    deleteBranchOnMerge
    autoMergeAllowed
    newest: pullRequests(
      states: OPEN, first: %d, orderBy: {field: CREATED_AT, direction: DESC}
    ) {
      totalCount
      nodes { number }
    }
    firstPage: pullRequests(
      states: OPEN, first: %d, orderBy: {field: CREATED_AT, direction: DESC}
    ) {
      nodes { ...PrFields }
    }
  }
}
`, PRLimit, PageSize) + prFields

func batchQuery(numbers []int) string {
	fields := make([]string, len(numbers))
	for i, number := range numbers {
		fields[i] = fmt.Sprintf("    pr%d: pullRequest(number: %d) { ...PrFields }", number, number)
	}
	return "query($owner: String!, $name: String!) {\n" +
		"  repository(owner: $owner, name: $name) {\n" +
		strings.Join(fields, "\n") + "\n" +
		"  }\n" +
		"}\n" + prFields
}

const (
	mergeMutation = `
mutation($id: ID!, $method: PullRequestMergeMethod!, $head: GitObjectID) {
  mergePullRequest(
    input: {pullRequestId: $id, mergeMethod: $method, expectedHeadOid: $head}
  ) { clientMutationId }
}
`
	updateBranchMutation = `
mutation($id: ID!) {
  updatePullRequestBranch(input: {pullRequestId: $id}) { clientMutationId }
}
`
	enableAutoMergeMutation = `
mutation($id: ID!, $method: PullRequestMergeMethod!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $method}) {
    clientMutationId
  }
}
`
	disableAutoMergeMutation = `
mutation($id: ID!) {
  disablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId }
}
`
	deleteRefMutation = `
mutation($id: ID!) {
  deleteRef(input: {refId: $id}) { clientMutationId }
}
`
)

// Error is anything GitHub or the network refused.
type Error struct {
	Message string
}

func (e *Error) Error() string { return e.Message }

// NotFoundError means the repo or PR isn't there (or isn't visible).
type NotFoundError struct{ Message string }

func (e *NotFoundError) Error() string { return e.Message }

// AuthError means the token was rejected or couldn't be read.
type AuthError struct{ Message string }

func (e *AuthError) Error() string { return e.Message }

// RateLimitError carries when GitHub says the limit resets.
type RateLimitError struct {
	Message string
	ResetAt time.Time
}

func (e *RateLimitError) Error() string { return e.Message }

// GetToken reads the token from the GitHub CLI.
func GetToken() (string, error) {
	command := exec.Command("gh", "auth", "token")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		var missing *exec.Error
		if errors.As(err, &missing) {
			return "", &AuthError{Message: "GitHub CLI 'gh' not found. " +
				"Install it with `brew install gh`, then run `gh auth login`"}
		}
		if problem := strings.TrimSpace(stderr.String()); problem != "" {
			return "", &AuthError{Message: problem + ". Run `gh auth login`"}
		}
		return "", &AuthError{Message: "gh returned no token. Run `gh auth login`"}
	}
	token := strings.TrimSpace(stdout.String())
	if token == "" {
		return "", &AuthError{Message: strings.TrimSpace(stderr.String())}
	}
	return token, nil
}

// Client is a GraphQL client; the token provider is called again if GitHub
// rejects the token, because a long-running daemon outlives `gh auth login`.
type Client struct {
	url           string
	http          *http.Client
	tokenProvider func() (string, error)
	mutex         sync.RWMutex
	token         string
}

func NewClient(tokenProvider func() (string, error)) (*Client, error) {
	token, err := tokenProvider()
	if err != nil {
		return nil, err
	}
	return &Client{
		url:           APIURL,
		http:          &http.Client{Timeout: requestTimeout},
		tokenProvider: tokenProvider,
		token:         token,
	}, nil
}

// SetURL points the client at another endpoint (tests).
func (c *Client) SetURL(url string) { c.url = url }

func (c *Client) currentToken() string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.token
}

func (c *Client) refreshToken() error {
	token, err := c.tokenProvider()
	if err != nil {
		return err
	}
	c.mutex.Lock()
	c.token = token
	c.mutex.Unlock()
	return nil
}

func (c *Client) graphql(ctx context.Context, query string, variables map[string]any) (map[string]json.RawMessage, error) {
	data, err := c.post(ctx, query, variables)
	var authProblem *AuthError
	if errors.As(err, &authProblem) {
		if refreshErr := c.refreshToken(); refreshErr != nil {
			return nil, refreshErr
		}
		return c.post(ctx, query, variables)
	}
	return data, err
}

func (c *Client) post(ctx context.Context, query string, variables map[string]any) (map[string]json.RawMessage, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, &Error{Message: err.Error()}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, &Error{Message: err.Error()}
	}
	request.Header.Set("Authorization", "Bearer "+c.currentToken())
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("Network error: %v", err)}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("Network error: %v", err)}
	}
	if problem := httpProblem(response, body); problem != nil {
		return nil, problem
	}
	var envelope struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, &Error{Message: fmt.Sprintf("GitHub sent an unreadable response: %v", err)}
	}
	if len(envelope.Errors) > 0 {
		messages := []string{}
		rateLimited, missing := false, false
		for _, problem := range envelope.Errors {
			message := problem.Message
			if message == "" {
				message = "unknown error"
			}
			messages = append(messages, message)
			switch problem.Type {
			case "RATE_LIMITED":
				rateLimited = true
			case "NOT_FOUND":
				missing = true
			}
		}
		joined := strings.Join(messages, "; ")
		switch {
		case rateLimited:
			return nil, &RateLimitError{Message: joined, ResetAt: rateLimitReset(response)}
		case missing:
			return nil, &NotFoundError{Message: joined}
		}
		return nil, &Error{Message: joined}
	}
	return envelope.Data, nil
}

func httpProblem(response *http.Response, body []byte) error {
	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return &AuthError{Message: "GitHub rejected the token; run `gh auth login`"}
	case (response.StatusCode == http.StatusForbidden ||
		response.StatusCode == http.StatusTooManyRequests) &&
		response.Header.Get("x-ratelimit-remaining") == "0":
		return &RateLimitError{Message: "GitHub rate limit reached", ResetAt: rateLimitReset(response)}
	case response.StatusCode == http.StatusBadGateway || response.StatusCode == http.StatusGatewayTimeout:
		return &Error{Message: fmt.Sprintf(
			"GitHub timed out (%d); will retry next poll", response.StatusCode)}
	case response.StatusCode >= 400:
		detail := string(body)
		if strings.Contains(response.Header.Get("content-type"), "html") {
			detail = ""
		}
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return &Error{Message: strings.TrimSpace(
			fmt.Sprintf("GitHub API error %d %s", response.StatusCode, detail))}
	}
	return nil
}

func rateLimitReset(response *http.Response) time.Time {
	if reset := response.Header.Get("x-ratelimit-reset"); reset != "" {
		if seconds, err := strconv.ParseInt(reset, 10, 64); err == nil {
			return time.Unix(seconds, 0).UTC()
		}
	}
	return time.Now().UTC().Add(RateLimitFallback)
}

// FetchRepo returns the repo with its newest open PRs and their details.
func (c *Client) FetchRepo(ctx context.Context, name string) (models.Repo, error) {
	owner, repo, found := strings.Cut(name, "/")
	if !found || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return models.Repo{}, &NotFoundError{
			Message: fmt.Sprintf("Expected owner/name, got %q", name)}
	}
	variables := map[string]any{"owner": owner, "name": repo}
	data, err := c.graphql(ctx, repoQuery, variables)
	if err != nil {
		return models.Repo{}, err
	}
	var payload repoPayload
	if raw, ok := data["repository"]; !ok || string(raw) == "null" {
		return models.Repo{}, &NotFoundError{Message: fmt.Sprintf("Repository %s not found", name)}
	} else if err := json.Unmarshal(raw, &payload); err != nil {
		return models.Repo{}, &Error{Message: fmt.Sprintf("GitHub sent an unreadable repository: %v", err)}
	}

	byNumber := map[int]prNode{}
	for _, node := range payload.FirstPage.Nodes {
		byNumber[node.Number] = node
	}
	numbers := []int{}
	remaining := []int{}
	for _, node := range payload.Newest.Nodes {
		numbers = append(numbers, node.Number)
		if _, have := byNumber[node.Number]; !have {
			remaining = append(remaining, node.Number)
		}
	}
	pages, err := c.fetchPages(ctx, variables, remaining)
	if err != nil {
		return models.Repo{}, err
	}
	for number, node := range pages {
		byNumber[number] = node
	}

	prs := []models.PullRequest{}
	for _, number := range numbers {
		node, have := byNumber[number]
		if !have || node.State != "OPEN" {
			continue
		}
		prs = append(prs, readiness.Assess(node.toPullRequest()))
	}
	return models.Repo{
		Name:                payload.NameWithOwner,
		MergeMethods:        payload.mergeMethods(),
		DeleteBranchOnMerge: payload.DeleteBranchOnMerge,
		PRs:                 prs,
		PRTotal:             payload.Newest.TotalCount,
		AutoMergeAllowed:    payload.AutoMergeAllowed,
	}, nil
}

// fetchPages loads the PRs the first page didn't cover, PageSize at a time.
func (c *Client) fetchPages(ctx context.Context, variables map[string]any, numbers []int) (map[int]prNode, error) {
	batches := [][]int{}
	for start := 0; start < len(numbers); start += PageSize {
		end := min(start+PageSize, len(numbers))
		batches = append(batches, numbers[start:end])
	}
	found := map[int]prNode{}
	var mutex sync.Mutex
	var group sync.WaitGroup
	errs := make([]error, len(batches))
	for i, batch := range batches {
		group.Add(1)
		go func(i int, batch []int) {
			defer group.Done()
			data, err := c.graphql(ctx, batchQuery(batch), variables)
			if err != nil {
				errs[i] = err
				return
			}
			var repository map[string]*prNode
			if raw, ok := data["repository"]; ok {
				if err := json.Unmarshal(raw, &repository); err != nil {
					errs[i] = &Error{Message: fmt.Sprintf("GitHub sent unreadable pull requests: %v", err)}
					return
				}
			}
			mutex.Lock()
			defer mutex.Unlock()
			for _, node := range repository {
				if node != nil {
					found[node.Number] = *node
				}
			}
		}(i, batch)
	}
	group.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return found, nil
}

// Merge merges the PR; with expectedHeadOid, GitHub refuses if the head has moved.
func (c *Client) Merge(ctx context.Context, prID string, method models.MergeMethod, expectedHeadOid string) error {
	variables := map[string]any{"id": prID, "method": string(method)}
	if expectedHeadOid != "" {
		variables["head"] = expectedHeadOid
	}
	_, err := c.graphql(ctx, mergeMutation, variables)
	return err
}

func (c *Client) UpdateBranch(ctx context.Context, prID string) error {
	_, err := c.graphql(ctx, updateBranchMutation, map[string]any{"id": prID})
	return err
}

func (c *Client) EnableAutoMerge(ctx context.Context, prID string, method models.MergeMethod) error {
	_, err := c.graphql(ctx, enableAutoMergeMutation,
		map[string]any{"id": prID, "method": string(method)})
	return err
}

func (c *Client) DisableAutoMerge(ctx context.Context, prID string) error {
	_, err := c.graphql(ctx, disableAutoMergeMutation, map[string]any{"id": prID})
	return err
}

func (c *Client) DeleteBranch(ctx context.Context, refID string) error {
	_, err := c.graphql(ctx, deleteRefMutation, map[string]any{"id": refID})
	return err
}
