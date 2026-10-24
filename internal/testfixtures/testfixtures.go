// Package testfixtures builds PRs and repos for tests (by default a clean,
// mergeable pull request in a repo that allows every merge method) and holds
// fakes of what the service talks to.
package testfixtures

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/twoway"
)

const (
	CreatedAt    = "2026-09-15T01:28:54Z"
	CommittedAt  = "2026-09-15T02:00:00Z"
	HeadSha      = "abc1234def5678abc1234def5678abc1234def56"
	CheckStarted = "2026-09-15T01:30:00Z"
)

// PR is a mergeable pull request; `mutate` applies the differences a test cares about.
func PR(number int, mutate ...func(*models.PullRequest)) models.PullRequest {
	pr := models.PullRequest{
		ID:            fmt.Sprintf("PR_%d", number),
		Number:        number,
		Title:         "A change",
		URL:           fmt.Sprintf("https://github.com/acme/api/pull/%d", number),
		Author:        "alice",
		CreatedAt:     CreatedAt,
		HeadRef:       fmt.Sprintf("feature-%d", number),
		BaseRef:       "main",
		HeadRefID:     models.Ptr("REF_1"),
		HeadRepo:      models.Ptr("acme/api"),
		Mergeable:     "MERGEABLE",
		MergeState:    "CLEAN",
		CheckState:    models.Ptr("SUCCESS"),
		Checks:        []models.Check{},
		ClosingIssues: []models.LinkedIssue{},
		LastCommitAt:  models.Ptr(CommittedAt),
		HeadSha:       models.Ptr(HeadSha),
		Reasons:       []models.Reason{},
		Actions:       []models.ActionOption{},
		WaitsOn:       []models.PRRef{},
		RequiredBy:    []models.PRRef{},
	}
	for _, apply := range mutate {
		apply(&pr)
	}
	return pr
}

// Repo allows every merge method and GitHub's own auto-merge.
func Repo(name string, prs []models.PullRequest, mutate ...func(*models.Repo)) models.Repo {
	if prs == nil {
		prs = []models.PullRequest{}
	}
	repo := models.Repo{
		Name:             name,
		MergeMethods:     []models.MergeMethod{models.MergeSquash, models.MergeCommit, models.MergeRebase},
		PRs:              prs,
		PRTotal:          len(prs),
		AutoMergeAllowed: true,
	}
	for _, apply := range mutate {
		apply(&repo)
	}
	return repo
}

// CheckRun is a completed check with the given conclusion.
func CheckRun(name, conclusion string) models.Check {
	return models.Check{Name: name, State: conclusion, StartedAt: models.Ptr(CheckStarted)}
}

// Common PR states.

func Pending(pr *models.PullRequest) {
	pr.MergeState = "BLOCKED"
	pr.CheckState = models.Ptr("PENDING")
}

func Failing(pr *models.PullRequest) {
	pr.MergeState = "BLOCKED"
	pr.CheckState = models.Ptr("FAILURE")
}

func Conflict(pr *models.PullRequest) {
	pr.Mergeable = "CONFLICTING"
	pr.MergeState = "DIRTY"
}

func Behind(pr *models.PullRequest) { pr.MergeState = "BEHIND" }

func Draft(pr *models.PullRequest) {
	pr.IsDraft = true
	pr.MergeState = "DRAFT"
}

// TwoWay is a fake two-way channel that records what it was asked to do. Posts
// get IDs "1", "2", … in order; a post failing with PostErr isn't recorded,
// while every edit and delete is, including those failing with EditErr or
// DeleteErr. While unavailable (SetUnavailable) posts, edits and deletes fail
// with twoway.ErrUnavailable and aren't recorded. Send on Taps to deliver a
// tap, or on ReceiveErrs to make a receive fail. Closed says Close was called.
// DeleteHook, when set, runs after a delete without the lock held, and returns
// what Delete returns instead of err. Hold the lock to read or set the fields
// while it is in use.
type TwoWay struct {
	sync.Mutex
	Posts   []twoway.Message
	Edits   map[string]twoway.Message
	Deletes []string
	Acked   []twoway.Tap
	Closed  bool

	PostErr, EditErr, DeleteErr error
	DeleteHook                  func(ctx context.Context, id string, err error) error
	Taps                        chan twoway.Tap
	ReceiveErrs                 chan error
	unavailable                 bool
}

var _ twoway.Channel = (*TwoWay)(nil)

func (f *TwoWay) Name() string { return "Fake" }

// SetUnavailable makes the fake act as a channel whose handler is down, or up again.
func (f *TwoWay) SetUnavailable(unavailable bool) {
	f.Lock()
	defer f.Unlock()
	f.unavailable = unavailable
}

func (f *TwoWay) Post(ctx context.Context, message twoway.Message) (string, error) {
	f.Lock()
	defer f.Unlock()
	if f.unavailable {
		return "", twoway.ErrUnavailable
	}
	if f.PostErr != nil {
		return "", f.PostErr
	}
	f.Posts = append(f.Posts, message)
	return strconv.Itoa(len(f.Posts)), nil
}

func (f *TwoWay) Edit(ctx context.Context, id string, message twoway.Message) error {
	f.Lock()
	defer f.Unlock()
	if f.unavailable {
		return twoway.ErrUnavailable
	}
	if f.Edits == nil {
		f.Edits = map[string]twoway.Message{}
	}
	f.Edits[id] = message
	return f.EditErr
}

func (f *TwoWay) Delete(ctx context.Context, id string) error {
	f.Lock()
	err := twoway.ErrUnavailable
	if !f.unavailable {
		f.Deletes = append(f.Deletes, id)
		err = f.DeleteErr
	}
	hook := f.DeleteHook
	f.Unlock()
	if hook != nil {
		return hook(ctx, id, err)
	}
	return err
}

func (f *TwoWay) Close() error {
	f.Lock()
	defer f.Unlock()
	f.Closed = true
	return nil
}

// Receive waits for one tap on Taps or an error on ReceiveErrs, or for the
// context to end.
func (f *TwoWay) Receive(ctx context.Context) ([]twoway.Tap, error) {
	select {
	case tap := <-f.Taps:
		return []twoway.Tap{tap}, nil
	case err := <-f.ReceiveErrs:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *TwoWay) Ack(ctx context.Context, tap twoway.Tap) error {
	f.Lock()
	defer f.Unlock()
	f.Acked = append(f.Acked, tap)
	return nil
}
