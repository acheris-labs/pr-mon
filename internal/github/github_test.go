package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acheris-labs/pr-mon/internal/github"
	"github.com/acheris-labs/pr-mon/internal/models"
)

// rawPR is one pull request as GitHub's GraphQL API returns it.
func rawPR(number int, mutate ...func(map[string]any)) map[string]any {
	pr := map[string]any{
		"id": fmt.Sprintf("PR_%d", number), "number": number, "state": "OPEN",
		"title": "A change", "url": fmt.Sprintf("https://github.com/acme/api/pull/%d", number),
		"createdAt": "2026-09-15T01:28:54Z", "isDraft": false,
		"author":      map[string]any{"login": "alice"},
		"headRefName": fmt.Sprintf("feature-%d", number), "baseRefName": "main",
		"headRef":        map[string]any{"id": "REF_1"},
		"headRepository": map[string]any{"nameWithOwner": "acme/api"},
		"mergeable":      "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": nil,
		"autoMergeRequest": nil,
		"commits": map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{
			"oid": "abc123", "committedDate": "2026-09-15T02:00:00Z",
			"statusCheckRollup": map[string]any{
				"state":    "SUCCESS",
				"contexts": map[string]any{"totalCount": 0, "nodes": []any{}},
			},
		}}}},
	}
	for _, apply := range mutate {
		apply(pr)
	}
	return pr
}

// repoResponse is the repository query's reply: the newest numbers plus the first page.
func repoResponse(prs []map[string]any, total int) map[string]any {
	numbers := []any{}
	for _, pr := range prs {
		numbers = append(numbers, map[string]any{"number": pr["number"]})
	}
	firstPage := []any{}
	for i, pr := range prs {
		if i >= github.PageSize {
			break
		}
		firstPage = append(firstPage, pr)
	}
	if total == 0 {
		total = len(prs)
	}
	return map[string]any{"repository": map[string]any{
		"nameWithOwner": "acme/api", "mergeCommitAllowed": true, "squashMergeAllowed": true,
		"rebaseMergeAllowed": true, "deleteBranchOnMerge": false, "autoMergeAllowed": true,
		"newest":    map[string]any{"totalCount": total, "nodes": numbers},
		"firstPage": map[string]any{"nodes": firstPage},
	}}
}

// recorder is a fake GitHub that records requests and replays canned replies.
type recorder struct {
	server   *httptest.Server
	mutex    sync.Mutex
	requests []map[string]any
}

type reply struct {
	status  int
	headers map[string]string
	body    string
}

func okData(data any) reply {
	body, _ := json.Marshal(map[string]any{"data": data})
	return reply{status: 200, body: string(body)}
}

func graphqlError(errorType, message string) reply {
	body, _ := json.Marshal(map[string]any{"data": nil, "errors": []any{
		map[string]any{"type": errorType, "message": message},
	}})
	return reply{status: 200, body: string(body)}
}

func serve(t *testing.T, handler func(request map[string]any) reply) (*github.Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		body["__authorization"] = r.Header.Get("Authorization")
		rec.mutex.Lock()
		rec.requests = append(rec.requests, body)
		rec.mutex.Unlock()
		response := handler(body)
		for key, value := range response.headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(response.status)
		fmt.Fprint(w, response.body)
	}))
	t.Cleanup(rec.server.Close)
	client, err := github.NewClient(func() (string, error) { return "tok", nil })
	if err != nil {
		t.Fatal(err)
	}
	client.SetURL(rec.server.URL)
	return client, rec
}

func always(response reply) func(map[string]any) reply {
	return func(map[string]any) reply { return response }
}

func (r *recorder) count() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return len(r.requests)
}

func (r *recorder) last() map[string]any {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.requests[len(r.requests)-1]
}

func query(request map[string]any) string {
	text, _ := request["query"].(string)
	return text
}

func TestFetchRepo(t *testing.T) {
	client, rec := serve(t, always(okData(repoResponse([]map[string]any{rawPR(5)}, 70))))
	repo, err := client.FetchRepo(context.Background(), "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Name != "acme/api" || repo.PRTotal != 70 || len(repo.PRs) != 1 || repo.PRs[0].Number != 5 {
		t.Fatalf("repo = %+v", repo)
	}
	if repo.PRs[0].Status != models.StatusReady {
		t.Errorf("status = %q, want the backend rules applied", repo.PRs[0].Status)
	}
	if repo.PRs[0].Author != "alice" || *repo.PRs[0].HeadSha != "abc123" {
		t.Errorf("pr = %+v", repo.PRs[0])
	}
	if got := repo.MergeMethods; len(got) != 3 || got[0] != models.MergeSquash {
		t.Errorf("merge methods = %v, want squash first", got)
	}
	if rec.count() != 1 {
		t.Errorf("requests = %d, want one", rec.count())
	}
	request := rec.last()
	if request["__authorization"] != "Bearer tok" {
		t.Errorf("authorization = %v", request["__authorization"])
	}
	if got := request["variables"]; !sameMap(got, map[string]any{"owner": "acme", "name": "api"}) {
		t.Errorf("variables = %v", got)
	}
	for _, want := range []string{"CREATED_AT", "first: 50", "first: 10"} {
		if !strings.Contains(query(request), want) {
			t.Errorf("query is missing %q", want)
		}
	}
}

func TestFetchRepoBatchesRemainingPRs(t *testing.T) {
	prs := []map[string]any{}
	byNumber := map[int]map[string]any{}
	for number := 125; number > 100; number-- { // 25 PRs, newest first
		pr := rawPR(number)
		if number == 101 {
			pr["state"] = "MERGED"
		}
		prs = append(prs, pr)
		byNumber[number] = pr
	}
	batchNumbers := regexp.MustCompile(`pullRequest\(number: (\d+)\)`)
	client, rec := serve(t, func(request map[string]any) reply {
		text := query(request)
		if strings.Contains(text, "newest:") {
			return okData(repoResponse(prs, 0))
		}
		found := map[string]any{}
		for _, match := range batchNumbers.FindAllStringSubmatch(text, -1) {
			number, _ := strconv.Atoi(match[1])
			found["pr"+match[1]] = byNumber[number]
		}
		return okData(map[string]any{"repository": found})
	})
	repo, err := client.FetchRepo(context.Background(), "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	numbers := []int{}
	for _, pr := range repo.PRs {
		numbers = append(numbers, pr.Number)
	}
	if len(numbers) != 24 || numbers[0] != 125 || numbers[23] != 102 {
		t.Errorf("numbers = %v, want 125..102 with the merged one dropped", numbers)
	}
	if rec.count() != 3 {
		t.Errorf("requests = %d, want the repo query plus two batches", rec.count())
	}
	sizes := []int{}
	rec.mutex.Lock()
	for _, request := range rec.requests[1:] {
		sizes = append(sizes, len(batchNumbers.FindAllString(query(request), -1)))
	}
	rec.mutex.Unlock()
	total := 0
	for _, size := range sizes {
		if size > github.PageSize {
			t.Errorf("batch of %d, want at most %d", size, github.PageSize)
		}
		total += size
	}
	if total != 15 {
		t.Errorf("batched %d PRs, want the 15 the first page missed", total)
	}
}

func TestBatchFailureFailsTheFetch(t *testing.T) {
	prs := []map[string]any{}
	for number := 15; number > 0; number-- {
		prs = append(prs, rawPR(number))
	}
	client, _ := serve(t, func(request map[string]any) reply {
		if strings.Contains(query(request), "newest:") {
			return okData(repoResponse(prs, 0))
		}
		return reply{status: 502, headers: map[string]string{"content-type": "text/html"},
			body: "<html>"}
	})
	_, err := client.FetchRepo(context.Background(), "acme/api")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want a timeout", err)
	}
}

func TestFetchMissingRepo(t *testing.T) {
	client, _ := serve(t, always(okData(map[string]any{"repository": nil})))
	var missing *github.NotFoundError
	if _, err := client.FetchRepo(context.Background(), "acme/api"); !errors.As(err, &missing) {
		t.Errorf("error = %v, want not found", err)
	}
	client, _ = serve(t, always(graphqlError("NOT_FOUND", "Could not resolve to a Repository")))
	if _, err := client.FetchRepo(context.Background(), "acme/api"); !errors.As(err, &missing) {
		t.Errorf("error = %v, want not found", err)
	}
}

func TestInvalidNameMakesNoRequest(t *testing.T) {
	client, rec := serve(t, always(okData(map[string]any{})))
	for _, name := range []string{"acme", "acme/api/extra", "/api", "acme/"} {
		var missing *github.NotFoundError
		if _, err := client.FetchRepo(context.Background(), name); !errors.As(err, &missing) {
			t.Errorf("%q: error = %v, want not found", name, err)
		}
	}
	if rec.count() != 0 {
		t.Errorf("requests = %d, want none", rec.count())
	}
}

func TestUnauthorized(t *testing.T) {
	client, rec := serve(t, always(reply{status: 401, body: `{"message":"Bad credentials"}`}))
	var authProblem *github.AuthError
	if _, err := client.FetchRepo(context.Background(), "acme/api"); !errors.As(err, &authProblem) {
		t.Errorf("error = %v, want an auth error", err)
	}
	if rec.count() != 2 {
		t.Errorf("requests = %d, want the token re-read and one retry", rec.count())
	}
}

func TestRateLimits(t *testing.T) {
	reset := time.Now().UTC().Add(15 * time.Minute).Truncate(time.Second)
	headers := map[string]string{
		"x-ratelimit-remaining": "0",
		"x-ratelimit-reset":     strconv.FormatInt(reset.Unix(), 10),
	}
	client, _ := serve(t, always(reply{status: 403, headers: headers, body: "{}"}))
	var limited *github.RateLimitError
	_, err := client.FetchRepo(context.Background(), "acme/api")
	if !errors.As(err, &limited) {
		t.Fatalf("error = %v, want a rate limit", err)
	}
	if !limited.ResetAt.Equal(reset) {
		t.Errorf("reset = %v, want %v", limited.ResetAt, reset)
	}

	client, _ = serve(t, always(graphqlError("RATE_LIMITED", "API rate limit exceeded")))
	_, err = client.FetchRepo(context.Background(), "acme/api")
	if !errors.As(err, &limited) {
		t.Fatalf("error = %v, want a rate limit", err)
	}
	if limited.ResetAt.Before(time.Now().UTC()) {
		t.Errorf("reset = %v, want a fallback in the future", limited.ResetAt)
	}
}

func TestHTTPErrors(t *testing.T) {
	client, _ := serve(t, always(reply{status: 500, body: "server exploded"}))
	_, err := client.FetchRepo(context.Background(), "acme/api")
	if err == nil || !strings.Contains(err.Error(), "500") ||
		!strings.Contains(err.Error(), "server exploded") {
		t.Errorf("error = %v", err)
	}
	client, _ = serve(t, always(reply{status: 500,
		headers: map[string]string{"content-type": "text/html"},
		body:    "<html>a very long page</html>"}))
	_, err = client.FetchRepo(context.Background(), "acme/api")
	if err == nil || strings.Contains(err.Error(), "html") {
		t.Errorf("error = %v, want the HTML body hidden", err)
	}
}

func TestNetworkError(t *testing.T) {
	client, rec := serve(t, always(okData(map[string]any{})))
	rec.server.Close() // nothing listening any more
	_, err := client.FetchRepo(context.Background(), "acme/api")
	if err == nil || !strings.Contains(err.Error(), "Network error") {
		t.Errorf("error = %v", err)
	}
}

func TestMutations(t *testing.T) {
	client, rec := serve(t, always(okData(map[string]any{"mergePullRequest": map[string]any{}})))
	ctx := context.Background()

	if err := client.Merge(ctx, "PR_1", models.MergeSquash, ""); err != nil {
		t.Fatal(err)
	}
	request := rec.last()
	if !strings.Contains(query(request), "mergePullRequest") {
		t.Errorf("query = %s", query(request))
	}
	variables := request["variables"].(map[string]any)
	if variables["id"] != "PR_1" || variables["method"] != "SQUASH" {
		t.Errorf("variables = %v", variables)
	}
	if _, sent := variables["head"]; sent {
		t.Error("no expected head should be sent when none was given")
	}

	if err := client.Merge(ctx, "PR_1", models.MergeRebase, "deadbeef"); err != nil {
		t.Fatal(err)
	}
	variables = rec.last()["variables"].(map[string]any)
	if variables["head"] != "deadbeef" || variables["method"] != "REBASE" {
		t.Errorf("variables = %v", variables)
	}

	if err := client.UpdateBranch(ctx, "PR_2"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query(rec.last()), "updatePullRequestBranch") {
		t.Errorf("query = %s", query(rec.last()))
	}

	if err := client.EnableAutoMerge(ctx, "PR_3", models.MergeCommit); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query(rec.last()), "enablePullRequestAutoMerge") {
		t.Errorf("query = %s", query(rec.last()))
	}
	if rec.last()["variables"].(map[string]any)["method"] != "MERGE" {
		t.Errorf("variables = %v", rec.last()["variables"])
	}

	if err := client.DisableAutoMerge(ctx, "PR_4"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query(rec.last()), "disablePullRequestAutoMerge") {
		t.Errorf("query = %s", query(rec.last()))
	}

	if err := client.DeleteBranch(ctx, "REF_1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query(rec.last()), "deleteRef") {
		t.Errorf("query = %s", query(rec.last()))
	}
	if rec.last()["variables"].(map[string]any)["id"] != "REF_1" {
		t.Errorf("variables = %v", rec.last()["variables"])
	}
}

func TestMergeErrorSurfacesTheMessage(t *testing.T) {
	client, _ := serve(t, always(graphqlError("", "Head branch was modified. Review and try again.")))
	err := client.Merge(context.Background(), "PR_1", models.MergeSquash, "")
	if err == nil || !strings.Contains(err.Error(), "was modified") {
		t.Errorf("error = %v", err)
	}
}

func TestQueriesRequestAutoMergeFields(t *testing.T) {
	client, rec := serve(t, always(okData(repoResponse([]map[string]any{rawPR(1)}, 0))))
	if _, err := client.FetchRepo(context.Background(), "acme/api"); err != nil {
		t.Fatal(err)
	}
	text := query(rec.last())
	for _, want := range []string{"autoMergeAllowed", "autoMergeRequest", "mergeMethod"} {
		if !strings.Contains(text, want) {
			t.Errorf("query is missing %q", want)
		}
	}
}

func TestTokenIsRereadOnce(t *testing.T) {
	tokens := []string{"first", "second", "third"}
	var index int
	var mutex sync.Mutex
	rec := &recorder{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		rec.requests = append(rec.requests, map[string]any{"auth": r.Header.Get("Authorization")})
		mutex.Unlock()
		w.WriteHeader(401)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
	}))
	defer rec.server.Close()
	client, err := github.NewClient(func() (string, error) {
		mutex.Lock()
		defer mutex.Unlock()
		token := tokens[index]
		index++
		return token, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	client.SetURL(rec.server.URL)
	var authProblem *github.AuthError
	if _, err := client.FetchRepo(context.Background(), "acme/api"); !errors.As(err, &authProblem) {
		t.Fatalf("error = %v, want an auth error", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(rec.requests) != 2 {
		t.Fatalf("requests = %d, want two", len(rec.requests))
	}
	if rec.requests[0]["auth"] != "Bearer first" || rec.requests[1]["auth"] != "Bearer second" {
		t.Errorf("auth headers = %v, want the token re-read once", rec.requests)
	}
}

func TestTokenProviderFailurePropagates(t *testing.T) {
	_, err := github.NewClient(func() (string, error) {
		return "", &github.AuthError{Message: "GitHub CLI 'gh' not found"}
	})
	var authProblem *github.AuthError
	if !errors.As(err, &authProblem) {
		t.Errorf("error = %v, want an auth error", err)
	}
}

func sameMap(got any, want map[string]any) bool {
	asMap, ok := got.(map[string]any)
	if !ok || len(asMap) != len(want) {
		return false
	}
	for key, value := range want {
		if asMap[key] != value {
			return false
		}
	}
	return true
}
