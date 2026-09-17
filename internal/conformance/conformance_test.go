// Package conformance checks the Go rules against protocol-fixtures/, which the
// Python backend generated: same status, reasons, readiness and action menus.
package conformance_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/actions"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/readiness"
)

type snapshotResponse struct {
	Result struct {
		Repos map[string]models.Repo                  `json:"repos"`
		Armed map[string]map[string]models.ArmedMerge `json:"armed"`
	} `json:"result"`
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "protocol-fixtures", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

func TestRulesMatchTheFixtures(t *testing.T) {
	var snapshot snapshotResponse
	if err := json.Unmarshal(fixture(t, "snapshot-response.json"), &snapshot); err != nil {
		t.Fatalf("decoding snapshot: %v", err)
	}
	if len(snapshot.Result.Repos) == 0 {
		t.Fatal("no repos in the fixture")
	}
	for name, repo := range snapshot.Result.Repos {
		armed := map[int]models.ArmedMerge{}
		for number, merge := range snapshot.Result.Armed[name] {
			var parsed int
			if _, err := fmt.Sscanf(number, "%d", &parsed); err != nil {
				t.Fatalf("armed key %q: %v", number, err)
			}
			armed[parsed] = merge
		}
		// Recompute from the raw GitHub-derived fields only.
		bare := repo
		bare.PRs = nil
		for _, pr := range repo.PRs {
			stripped := pr
			stripped.Status = ""
			stripped.Reasons = nil
			stripped.StrictlyReady = false
			stripped.LastCheckStartedAt = nil
			stripped.Actions = nil
			bare.PRs = append(bare.PRs, readiness.Assess(stripped))
		}
		computed := actions.WithActions(bare, armed)
		for i, pr := range computed.PRs {
			want := repo.PRs[i]
			t.Run(name+"#"+pr.ID, func(t *testing.T) {
				if pr.Status != want.Status {
					t.Errorf("status = %q, want %q", pr.Status, want.Status)
				}
				if !reflect.DeepEqual(pr.Reasons, want.Reasons) {
					t.Errorf("reasons = %+v, want %+v", pr.Reasons, want.Reasons)
				}
				if pr.StrictlyReady != want.StrictlyReady {
					t.Errorf("strictly ready = %v, want %v", pr.StrictlyReady, want.StrictlyReady)
				}
				if !reflect.DeepEqual(pr.LastCheckStartedAt, want.LastCheckStartedAt) {
					t.Errorf("last check = %v, want %v", pr.LastCheckStartedAt, want.LastCheckStartedAt)
				}
				if !reflect.DeepEqual(pr.Actions, want.Actions) {
					t.Errorf("actions = %+v, want %+v", pr.Actions, want.Actions)
				}
			})
		}
	}
}
