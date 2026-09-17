package readiness_test

import (
	"testing"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

func assess(mutate ...func(*models.PullRequest)) models.PullRequest {
	return readiness.Assess(testfixtures.PR(1, mutate...))
}

func reasonTexts(pr models.PullRequest) []string {
	texts := []string{}
	for _, reason := range pr.Reasons {
		texts = append(texts, reason.Text)
	}
	return texts
}

func TestStatus(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*models.PullRequest)
		want   models.Status
	}{
		{"clean is ready", func(*models.PullRequest) {}, models.StatusReady},
		{"has hooks is ready", func(p *models.PullRequest) { p.MergeState = "HAS_HOOKS" }, models.StatusReady},
		{"draft", testfixtures.Draft, models.StatusDraft},
		{"unknown mergeable is checking", func(p *models.PullRequest) { p.Mergeable = "UNKNOWN" }, models.StatusChecking},
		{"unknown merge state is checking", func(p *models.PullRequest) { p.MergeState = "UNKNOWN" }, models.StatusChecking},
		{"conflicting", func(p *models.PullRequest) { p.Mergeable = "CONFLICTING" }, models.StatusConflict},
		{"dirty is conflict", func(p *models.PullRequest) { p.MergeState = "DIRTY" }, models.StatusConflict},
		{"conflict beats failing", func(p *models.PullRequest) {
			p.Mergeable = "CONFLICTING"
			p.CheckState = models.Ptr("FAILURE")
		}, models.StatusConflict},
		{"failing", testfixtures.Failing, models.StatusFailing},
		{"error rollup is failing", func(p *models.PullRequest) {
			p.MergeState = "BLOCKED"
			p.CheckState = models.Ptr("ERROR")
		}, models.StatusFailing},
		{"unstable is ready", func(p *models.PullRequest) {
			p.MergeState = "UNSTABLE"
			p.CheckState = models.Ptr("FAILURE")
		}, models.StatusReady},
		{"pending", testfixtures.Pending, models.StatusPending},
		{"behind", testfixtures.Behind, models.StatusBehind},
		{"blocked", func(p *models.PullRequest) { p.MergeState = "BLOCKED" }, models.StatusBlocked},
		{"unrecognized merge state is blocked", func(p *models.PullRequest) {
			p.MergeState = "SOMETHING_NEW"
		}, models.StatusBlocked},
		{"no rollup", func(p *models.PullRequest) { p.CheckState = nil }, models.StatusReady},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := assess(test.mutate).Status; got != test.want {
				t.Errorf("status = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReasons(t *testing.T) {
	t.Run("ready has no reasons", func(t *testing.T) {
		if got := reasonTexts(assess()); len(got) != 0 {
			t.Errorf("reasons = %v, want none", got)
		}
	})

	t.Run("draft", func(t *testing.T) {
		if got := reasonTexts(assess(testfixtures.Draft)); len(got) != 1 || got[0] != "Draft" {
			t.Errorf("reasons = %v", got)
		}
	})

	t.Run("checking", func(t *testing.T) {
		got := reasonTexts(assess(func(p *models.PullRequest) { p.Mergeable = "UNKNOWN" }))
		if len(got) != 1 || got[0] != "GitHub is still computing mergeability" {
			t.Errorf("reasons = %v", got)
		}
	})

	t.Run("conflict is an error", func(t *testing.T) {
		pr := assess(testfixtures.Conflict)
		if pr.Reasons[0] != (models.Reason{Text: "Merge conflicts", Level: "error"}) {
			t.Errorf("reasons = %v", pr.Reasons)
		}
	})

	t.Run("failed checks listed by name", func(t *testing.T) {
		pr := assess(func(p *models.PullRequest) {
			testfixtures.Failing(p)
			p.Checks = []models.Check{
				testfixtures.CheckRun("build", "SUCCESS"),
				testfixtures.CheckRun("lint", "FAILURE"),
				testfixtures.CheckRun("deploy", "ERROR"),
			}
			p.ChecksTotal = 3
		})
		want := []string{"Check failed: lint", "Check failed: deploy"}
		if got := reasonTexts(pr); !equal(got, want) {
			t.Errorf("reasons = %v, want %v", got, want)
		}
		for _, reason := range pr.Reasons {
			if reason.Level != "error" {
				t.Errorf("level = %q, want error", reason.Level)
			}
		}
	})

	t.Run("unstable failed checks are warnings", func(t *testing.T) {
		pr := assess(func(p *models.PullRequest) {
			p.MergeState = "UNSTABLE"
			p.CheckState = models.Ptr("FAILURE")
			p.Checks = []models.Check{testfixtures.CheckRun("flaky", "FAILURE")}
			p.ChecksTotal = 1
		})
		if pr.Reasons[0].Level != "warning" {
			t.Errorf("level = %q, want warning", pr.Reasons[0].Level)
		}
	})

	t.Run("truncated checks", func(t *testing.T) {
		pr := assess(func(p *models.PullRequest) {
			p.Checks = []models.Check{testfixtures.CheckRun("build", "SUCCESS")}
			p.ChecksTotal = 30
		})
		if got := reasonTexts(pr); len(got) != 1 || got[0] != "+29 more checks not shown" {
			t.Errorf("reasons = %v", got)
		}
	})

	t.Run("pending checks counted", func(t *testing.T) {
		pr := assess(func(p *models.PullRequest) {
			testfixtures.Pending(p)
			p.Checks = []models.Check{
				{Name: "ci", State: "PENDING"},
				{Name: "deploy", State: "IN_PROGRESS"},
				testfixtures.CheckRun("done", "SUCCESS"),
			}
			p.ChecksTotal = 3
		})
		if got := reasonTexts(pr); len(got) != 1 || got[0] != "2 checks pending" {
			t.Errorf("reasons = %v", got)
		}
	})

	t.Run("review decisions", func(t *testing.T) {
		cases := map[string]string{
			"CHANGES_REQUESTED": "Changes requested",
			"REVIEW_REQUIRED":   "Review required",
		}
		for decision, want := range cases {
			pr := assess(func(p *models.PullRequest) { p.ReviewDecision = models.Ptr(decision) })
			if got := reasonTexts(pr); len(got) != 1 || got[0] != want {
				t.Errorf("%s reasons = %v, want %q", decision, got, want)
			}
		}
		approved := assess(func(p *models.PullRequest) { p.ReviewDecision = models.Ptr("APPROVED") })
		if got := reasonTexts(approved); len(got) != 0 {
			t.Errorf("approved reasons = %v, want none", got)
		}
	})

	t.Run("behind", func(t *testing.T) {
		if got := reasonTexts(assess(testfixtures.Behind)); len(got) != 1 || got[0] != "Behind base branch" {
			t.Errorf("reasons = %v", got)
		}
	})

	t.Run("blocked without another reason", func(t *testing.T) {
		pr := assess(func(p *models.PullRequest) { p.MergeState = "BLOCKED" })
		if got := reasonTexts(pr); len(got) != 1 || got[0] != "Blocked by branch protection" {
			t.Errorf("reasons = %v", got)
		}
	})

	t.Run("unknown merge state is reported", func(t *testing.T) {
		pr := assess(func(p *models.PullRequest) { p.MergeState = "SOMETHING_NEW" })
		if got := reasonTexts(pr); len(got) != 1 || got[0] != "Merge state: SOMETHING_NEW" {
			t.Errorf("reasons = %v", got)
		}
	})
}

func TestStrictlyReady(t *testing.T) {
	for _, state := range []string{"CLEAN", "HAS_HOOKS"} {
		pr := assess(func(p *models.PullRequest) { p.MergeState = state })
		if !pr.StrictlyReady {
			t.Errorf("%s should be strictly ready", state)
		}
	}
	cases := map[string]func(*models.PullRequest){
		"unstable": func(p *models.PullRequest) { p.MergeState = "UNSTABLE" },
		"failed optional check": func(p *models.PullRequest) {
			p.Checks = []models.Check{testfixtures.CheckRun("flaky", "FAILURE")}
			p.ChecksTotal = 1
		},
		"draft":    testfixtures.Draft,
		"pending":  testfixtures.Pending,
		"checking": func(p *models.PullRequest) { p.Mergeable = "UNKNOWN" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if assess(mutate).StrictlyReady {
				t.Error("should not be strictly ready")
			}
		})
	}
}

func TestLastCheckStartedAt(t *testing.T) {
	pr := assess(func(p *models.PullRequest) {
		p.Checks = []models.Check{
			{Name: "old", State: "SUCCESS", StartedAt: models.Ptr("2026-09-15T01:00:00Z")},
			{Name: "new", State: "SUCCESS", StartedAt: models.Ptr("2026-09-15T03:00:00Z")},
			{Name: "none", State: "SUCCESS"},
		}
		p.ChecksTotal = 3
	})
	if pr.LastCheckStartedAt == nil || *pr.LastCheckStartedAt != "2026-09-15T03:00:00Z" {
		t.Errorf("last check = %v", pr.LastCheckStartedAt)
	}
	if got := assess().LastCheckStartedAt; got != nil {
		t.Errorf("no checks should give nil, got %v", *got)
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
