// Package models holds the data pr-mon exchanges with clients. Field order and
// JSON names match docs/protocol.md exactly; the fixtures test that.
package models

import "strings"

// Status is how mergeable a PR is.
type Status string

const (
	StatusDraft    Status = "DRAFT"
	StatusChecking Status = "CHECKING"
	StatusConflict Status = "CONFLICT"
	StatusFailing  Status = "FAILING"
	StatusPending  Status = "PENDING"
	StatusBehind   Status = "BEHIND"
	StatusBlocked  Status = "BLOCKED"
	StatusReady    Status = "READY"
)

// IsAlert reports whether a status needs the user's attention.
func (s Status) IsAlert() bool {
	return s == StatusConflict || s == StatusFailing || s == StatusBlocked
}

type MergeMethod string

const (
	MergeSquash MergeMethod = "SQUASH"
	MergeCommit MergeMethod = "MERGE"
	MergeRebase MergeMethod = "REBASE"
)

func (m MergeMethod) Lower() string { return strings.ToLower(string(m)) }

type Check struct {
	Name      string  `json:"name"`
	State     string  `json:"state"`
	StartedAt *string `json:"started_at"`
}

// LinkedIssue is an issue this PR closes when it merges: what GitHub's own
// Development panel lists, from "Closes #12" in the body or a manual link.
// Repo is the issue's own repository, which is not always the PR's.
type LinkedIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Repo   string `json:"repo"`
}

type AutoMerge struct {
	Method    MergeMethod `json:"method"`
	EnabledBy string      `json:"enabled_by"`
}

type Reason struct {
	Text  string `json:"text"`
	Level string `json:"level"` // "error" | "warning" | "info"
}

// ArmedMerge is a PR pr-mon will merge itself once it is strictly ready.
type ArmedMerge struct {
	Method       MergeMethod `json:"method"`
	DeleteBranch bool        `json:"delete_branch"`
	ArmedAt      string      `json:"armed_at"` // ISO-8601 UTC
}

// ActionOption is one entry of a PR's action menu, as the backend decides it.
type ActionOption struct {
	Key                string  `json:"key"`  // "merge" | "auto_merge" | "update"
	Kind               string  `json:"kind"` // the Action kind to send
	Label              string  `json:"label"`
	Available          bool    `json:"available"`
	Reason             *string `json:"reason"` // why it is unavailable
	Note               *string `json:"note"`
	NeedsMethod        bool    `json:"needs_method"`
	OffersDeleteBranch bool    `json:"offers_delete_branch"`
}

type PullRequest struct {
	ID             string        `json:"id"`
	Number         int           `json:"number"`
	Title          string        `json:"title"`
	URL            string        `json:"url"`
	Author         string        `json:"author"`
	CreatedAt      string        `json:"created_at"`
	IsDraft        bool          `json:"is_draft"`
	HeadRef        string        `json:"head_ref"`
	BaseRef        string        `json:"base_ref"`
	HeadRefID      *string       `json:"head_ref_id"`
	HeadRepo       *string       `json:"head_repo"`
	Mergeable      string        `json:"mergeable"`
	MergeState     string        `json:"merge_state"`
	ReviewDecision *string       `json:"review_decision"`
	CheckState     *string       `json:"check_state"`
	Checks         []Check       `json:"checks"`
	ChecksTotal    int           `json:"checks_total"`
	AutoMerge      *AutoMerge    `json:"auto_merge"`
	ClosingIssues  []LinkedIssue `json:"closing_issues"`
	LastCommitAt   *string       `json:"last_commit_at"`
	HeadSha        *string       `json:"head_sha"`
	// Filled in by the backend (readiness, actions); clients only display them.
	Status             Status         `json:"status"`
	Reasons            []Reason       `json:"reasons"`
	StrictlyReady      bool           `json:"strictly_ready"`
	LastCheckStartedAt *string        `json:"last_check_started_at"`
	Actions            []ActionOption `json:"actions"`
}

type Repo struct {
	Name                string        `json:"name"`
	MergeMethods        []MergeMethod `json:"merge_methods"`
	DeleteBranchOnMerge bool          `json:"delete_branch_on_merge"`
	PRs                 []PullRequest `json:"prs"`
	PRTotal             int           `json:"pr_total"`
	AutoMergeAllowed    bool          `json:"auto_merge_allowed"`
}

// Action is a user-requested change to a PR.
type Action struct {
	// "merge" | "update" | "auto_merge_on" | "auto_merge_off" | "arm_merge" | "disarm_merge"
	Kind         string       `json:"kind"`
	Method       *MergeMethod `json:"method"`
	DeleteBranch bool         `json:"delete_branch"`
}

// OwnerKey is the case-insensitive owner of an owner/name repo, used to group repos.
func OwnerKey(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	return strings.ToLower(owner)
}

// Owner is the owner as spelled in the repo name.
func Owner(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	return owner
}

// ShortName is the part of a repo name after the owner.
func ShortName(repo string) string {
	_, name, found := strings.Cut(repo, "/")
	if !found {
		return repo
	}
	return name
}

// Ptr is a helper for the many optional wire fields.
func Ptr[T any](value T) *T { return &value }
