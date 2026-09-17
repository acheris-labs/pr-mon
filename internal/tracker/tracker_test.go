package tracker_test

import (
	"reflect"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/state"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
	"github.com/acheris-labs/pr-mon/internal/tracker"
)

// repo builds acme/api with one PR per (number, state) pair.
func repo(prs ...func(*models.PullRequest)) models.Repo {
	list := []models.PullRequest{}
	for i, mutate := range prs {
		list = append(list, readiness.Assess(testfixtures.PR(i+1, mutate)))
	}
	return testfixtures.Repo("acme/api", list)
}

func numbered(number int, mutate func(*models.PullRequest)) models.Repo {
	return testfixtures.Repo("acme/api",
		[]models.PullRequest{readiness.Assess(testfixtures.PR(number, mutate))})
}

func ready(*models.PullRequest) {}

func records() map[string]map[string]state.Record {
	return map[string]map[string]state.Record{}
}

func kinds(events []tracker.Event) []tracker.EventKind {
	list := []tracker.EventKind{}
	for _, event := range events {
		list = append(list, event.Kind)
	}
	return list
}

func TestFirstPollIsAllNew(t *testing.T) {
	track := tracker.New(records())
	events := track.Update(repo(ready, testfixtures.Pending))
	if got := kinds(events); !reflect.DeepEqual(got, []tracker.EventKind{tracker.EventNew, tracker.EventNew}) {
		t.Errorf("events = %v", got)
	}
	if got := track.Unseen("acme/api"); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("unseen = %v", got)
	}
}

func TestUnchangedStatusGivesNoEvents(t *testing.T) {
	track := tracker.New(records())
	track.Update(repo(ready))
	if events := track.Update(repo(ready)); len(events) != 0 {
		t.Errorf("events = %v, want none", events)
	}
}

func TestBecomingReadyAndBlocked(t *testing.T) {
	track := tracker.New(records())
	track.Update(repo(testfixtures.Pending))
	if got := kinds(track.Update(repo(ready))); !reflect.DeepEqual(got, []tracker.EventKind{tracker.EventReady}) {
		t.Errorf("ready events = %v", got)
	}
	if got := kinds(track.Update(repo(testfixtures.Failing))); !reflect.DeepEqual(got, []tracker.EventKind{tracker.EventBlocked}) {
		t.Errorf("blocked events = %v", got)
	}
	// Moving between two alert statuses doesn't fire again.
	if got := track.Update(repo(testfixtures.Conflict)); len(got) != 0 {
		t.Errorf("events = %v, want none", got)
	}
}

func TestCheckingKeepsTheLastRealStatus(t *testing.T) {
	track := tracker.New(records())
	track.Update(repo(ready))
	track.MarkSeen("acme/api", 1)
	checking := func(pr *models.PullRequest) { pr.Mergeable = "UNKNOWN" }
	if events := track.Update(repo(checking)); len(events) != 0 {
		t.Errorf("events = %v, want none while checking", events)
	}
	if track.IsUnseen("acme/api", 1) {
		t.Error("a checking blip should not mark a PR unseen again")
	}
	if events := track.Update(repo(ready)); len(events) != 0 {
		t.Errorf("events = %v, want none when the old status returns", events)
	}
}

func TestChangesDoesNotRecord(t *testing.T) {
	track := tracker.New(records())
	track.Update(repo(testfixtures.Pending))
	changes := track.Changes(repo(ready))
	want := []tracker.Change{{Number: 1, Old: models.StatusPending, New: models.StatusReady}}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("changes = %+v, want %+v", changes, want)
	}
	// Still reported, because nothing was recorded.
	if again := track.Changes(repo(ready)); !reflect.DeepEqual(again, want) {
		t.Errorf("changes = %+v, want the same", again)
	}
	first := track.Changes(testfixtures.Repo("other/repo",
		[]models.PullRequest{readiness.Assess(testfixtures.PR(9, ready))}))
	if len(first) != 1 || first[0].Old != "" {
		t.Errorf("a PR with no history should have no old status: %+v", first)
	}
	if checking := track.Changes(numbered(1, func(pr *models.PullRequest) {
		pr.Mergeable = "UNKNOWN"
	})); len(checking) != 0 {
		t.Errorf("changes = %+v, want none while checking", checking)
	}
}

func TestSeenFlags(t *testing.T) {
	track := tracker.New(records())
	track.Update(repo(testfixtures.Pending))
	if !track.MarkSeen("acme/api", 1) {
		t.Error("marking an unseen PR should report a change")
	}
	if track.MarkSeen("acme/api", 1) {
		t.Error("marking it twice should not")
	}
	if track.MarkSeen("acme/api", 99) || track.MarkSeen("gone/repo", 1) {
		t.Error("unknown PRs and repos should report no change")
	}
	if len(track.Unseen("acme/api")) != 0 {
		t.Errorf("unseen = %v, want none", track.Unseen("acme/api"))
	}
	// A status change makes it unseen again.
	track.Update(repo(ready))
	if !track.IsUnseen("acme/api", 1) {
		t.Error("a new status should make the PR unseen")
	}
}

func TestForget(t *testing.T) {
	saved := records()
	track := tracker.New(saved)
	track.Update(repo(ready))
	track.Forget("acme/api")
	if len(track.Unseen("acme/api")) != 0 || len(saved) != 0 {
		t.Errorf("records = %v, want the repo dropped", saved)
	}
}

func TestRecordsAreShared(t *testing.T) {
	saved := records()
	track := tracker.New(saved)
	track.Update(repo(ready))
	if got := saved["acme/api"]["1"].Status; got != string(models.StatusReady) {
		t.Errorf("saved status = %q, want the caller's map updated in place", got)
	}
}
