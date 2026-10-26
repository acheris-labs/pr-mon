// The repo's dependency graph, shown in the details while the repo tabs have
// the focus: its open PRs in the order they can merge.

package tui

import (
	"fmt"
	"strings"

	"github.com/acheris-labs/pr-mon/internal/models"
)

// mergeLayers sorts a repo's PRs into the order they can merge: each layer
// waits only on PRs in the layers before it (or outside the repo's open PRs).
// PRs with no dependency either way are left out, in `unlinked`.
func mergeLayers(repo string, prs []models.PullRequest) (layers [][]models.PullRequest, unlinked []models.PullRequest) {
	open := map[string]models.PullRequest{}
	for _, pr := range prs {
		open[models.PRKey(repo, pr.Number)] = pr
	}
	depth := map[string]int{}
	var depthOf func(key string) int
	depthOf = func(key string) int {
		if known, found := depth[key]; found {
			return known
		}
		depth[key] = 0 // a loop, which the backend refuses, stops here
		deepest := 0
		for _, ref := range open[key].WaitsOn {
			if _, found := open[ref.Key()]; found {
				deepest = max(deepest, depthOf(ref.Key())+1)
			}
		}
		depth[key] = deepest
		return deepest
	}
	for _, pr := range prs {
		if len(pr.WaitsOn) == 0 && len(pr.RequiredBy) == 0 {
			unlinked = append(unlinked, pr)
			continue
		}
		layer := depthOf(models.PRKey(repo, pr.Number))
		for len(layers) <= layer {
			layers = append(layers, nil)
		}
		layers[layer] = append(layers[layer], pr)
	}
	return layers, unlinked
}

// RepoGraphText lists the repo's PRs by merge order, each with what it waits
// on, then the PRs nothing links.
func RepoGraphText(repo string, prs []models.PullRequest) string {
	layers, unlinked := mergeLayers(repo, prs)
	if len(layers) == 0 {
		return dim.Render("No dependencies between these PRs")
	}
	lines := []string{bold.Render("Dependencies, in merge order"), ""}
	for index, layer := range layers {
		step := fmt.Sprintf("%2d  ", index+1)
		for _, pr := range layer {
			lines = append(lines, step+refLine(prRef(repo, pr), repo, 0))
			step = strings.Repeat(" ", len(step))
			if waits := waitsOnText(repo, pr, prs); waits != "" {
				lines = append(lines, dim.Render("      waits on ")+waits)
			}
		}
	}
	if len(unlinked) > 0 {
		lines = append(lines, "", dim.Render("Not linked"))
		for _, pr := range unlinked {
			lines = append(lines, "    "+refLine(prRef(repo, pr), repo, 0))
		}
	}
	return strings.Join(lines, "\n")
}

// waitsOnText names what a PR waits on: just the number for the repo's open
// PRs, which are listed already, and the whole line for anything else.
func waitsOnText(repo string, pr models.PullRequest, prs []models.PullRequest) string {
	listed := map[string]bool{}
	for _, other := range prs {
		listed[models.PRKey(repo, other.Number)] = true
	}
	parts := []string{}
	for _, ref := range pr.WaitsOn {
		if listed[ref.Key()] {
			parts = append(parts, fmt.Sprintf("#%d", ref.Number))
		} else {
			parts = append(parts, refLine(ref, repo, 0))
		}
	}
	return strings.Join(parts, ", ")
}

func prRef(repo string, pr models.PullRequest) models.PRRef {
	return models.PRRef{
		Repo: repo, Number: pr.Number, Title: pr.Title, URL: pr.URL,
		State: models.PROpen, Status: models.Ptr(pr.Status),
	}
}
