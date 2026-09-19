// Package dependencies holds the PR dependency graph: which PRs must merge
// before which. Edges are stored as keys ("owner/repo#12") so a dependency can
// name a PR in any repo, monitored or not.
package dependencies

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/acheris-labs/pr-mon/internal/models"
)

// Graph maps a waiting PR's key to the keys of the PRs it waits on. It is the
// map saved in state.json, and is changed in place.
type Graph map[string][]string

var shortForm = regexp.MustCompile(`^([\w.-]+/[\w.-]+)#(\d+)$`)

// ParseKey splits a stored key back into repo and number.
func ParseKey(key string) (string, int, bool) {
	match := shortForm.FindStringSubmatch(key)
	if match == nil {
		return "", 0, false
	}
	number, err := strconv.Atoi(match[2])
	if err != nil || number <= 0 {
		return "", 0, false
	}
	return match[1], number, true
}

// ParseRef reads a PR the user named: "owner/repo#12", or a pull request URL.
func ParseRef(text string) (string, int, error) {
	text = strings.TrimSpace(text)
	if repo, number, ok := ParseKey(text); ok {
		return repo, number, nil
	}
	if !strings.Contains(text, "://") {
		text = "https://" + text
	}
	if parsed, err := url.Parse(text); err == nil && strings.EqualFold(parsed.Host, "github.com") {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) >= 4 && parts[2] == "pull" {
			if repo, number, ok := ParseKey(parts[0] + "/" + parts[1] + "#" + parts[3]); ok {
				return repo, number, nil
			}
		}
	}
	return "", 0, fmt.Errorf("expected owner/repo#number or a pull request URL, got %q",
		strings.TrimPrefix(text, "https://"))
}

// Add records that `from` waits on `to`, refusing duplicates and loops.
func (g Graph) Add(from, to string) error {
	if strings.EqualFold(from, to) {
		return fmt.Errorf("%s can't wait on itself", from)
	}
	if slices.Contains(g[from], to) {
		return fmt.Errorf("%s already waits on %s", from, to)
	}
	if g.reaches(to, from) {
		return fmt.Errorf("%s already waits on %s, so it can't also be the other way round",
			to, from)
	}
	g[from] = append(g[from], to)
	return nil
}

// reaches reports whether `start` waits on `target`, directly or through others.
func (g Graph) reaches(start, target string) bool {
	seen := map[string]bool{}
	pending := []string{start}
	for len(pending) > 0 {
		key := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if key == target {
			return true
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		pending = append(pending, g[key]...)
	}
	return false
}

// Remove drops one edge; false when `from` didn't wait on `to`.
func (g Graph) Remove(from, to string) bool {
	index := slices.Index(g[from], to)
	if index < 0 {
		return false
	}
	g[from] = slices.Delete(g[from], index, index+1)
	if len(g[from]) == 0 {
		delete(g, from)
	}
	return true
}

// Forget drops everything `key` waits on: it merged or closed.
func (g Graph) Forget(key string) { delete(g, key) }

// WaitsOn is what `key` waits on, in the order they were added.
func (g Graph) WaitsOn(key string) []string { return append([]string{}, g[key]...) }

// RequiredBy is every PR waiting on `key`, sorted.
func (g Graph) RequiredBy(key string) []string {
	waiting := []string{}
	for from, targets := range g {
		if slices.Contains(targets, key) {
			waiting = append(waiting, from)
		}
	}
	slices.Sort(waiting)
	return waiting
}

// Targets is every PR something waits on, sorted.
func (g Graph) Targets() []string {
	targets := []string{}
	for _, keys := range g {
		for _, key := range keys {
			if !slices.Contains(targets, key) {
				targets = append(targets, key)
			}
		}
	}
	slices.Sort(targets)
	return targets
}

// Tree is `key`'s dependencies as a tree in one direction: what it waits on
// (upstream) or what waits on it (downstream). A PR reachable two ways is shown
// in full once and marked Repeated after that.
func (g Graph) Tree(key string, upstream bool, ref func(string) models.PRRef) []models.DependencyNode {
	next := g.RequiredBy
	if upstream {
		next = g.WaitsOn
	}
	seen := map[string]bool{key: true}
	var grow func(string) []models.DependencyNode
	grow = func(from string) []models.DependencyNode {
		nodes := []models.DependencyNode{}
		for _, child := range next(from) {
			node := models.DependencyNode{PR: ref(child), Children: []models.DependencyNode{}}
			if seen[child] {
				node.Repeated = true
			} else {
				seen[child] = true
				node.Children = grow(child)
			}
			nodes = append(nodes, node)
		}
		return nodes
	}
	return grow(key)
}
