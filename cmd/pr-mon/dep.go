// `pr-mon dep`: PR dependencies from the command line, through the running
// backend. A bare number ("912" or "#912") is a PR in the repo of the git
// checkout you're in.

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/acheris-labs/pr-mon/internal/client"
	"github.com/acheris-labs/pr-mon/internal/daemon"
	"github.com/acheris-labs/pr-mon/internal/dependencies"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/tui"
)

const depUsage = `usage:
  pr-mon dep add <pr> on <pr>...   <pr> waits on each PR after "on"
  pr-mon dep rm <pr> on <pr>...    <pr> stops waiting on each PR after "on"
  pr-mon dep <pr>                  what <pr> waits on, and what waits on it
  pr-mon dep graph [owner/repo]    the repo's PRs in merge order

A PR is owner/repo#12, a pull request URL, or 12 / #12 for the repo of the
git checkout you're in.
`

var (
	bareNumber = regexp.MustCompile(`^#?(\d+)$`)
	// git@github.com:owner/repo.git, https://github.com/owner/repo,
	// ssh://git@github.com/owner/repo.git
	githubRemote = regexp.MustCompile(`github\.com[:/]([^/]+/[^/]+?)(?:\.git)?/?$`)
)

func depCommand(paths daemon.Paths, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	remote := client.New(paths.Socket())
	connect := func() error {
		err := remote.Connect("")
		if errors.Is(err, client.ErrUnavailable) {
			return errors.New("the backend isn't running; start it with `pr-mon start`")
		}
		return err
	}
	defer remote.Close()

	switch args[0] {
	case "add", "rm":
		if len(args) < 4 || args[2] != "on" {
			return errUsage
		}
		repo, number, err := resolvePR(args[1])
		if err != nil {
			return err
		}
		targets := []string{}
		for _, text := range args[3:] {
			targetRepo, targetNumber, err := resolvePR(text)
			if err != nil {
				return err
			}
			targets = append(targets, models.PRKey(targetRepo, targetNumber))
		}
		if err := connect(); err != nil {
			return err
		}
		for _, target := range targets {
			if args[0] == "add" {
				err = remote.AddDependency(repo, number, target)
			} else {
				err = remote.RemoveDependency(repo, number, target)
			}
			if err != nil {
				return err
			}
			verb := "now waits on"
			if args[0] == "rm" {
				verb = "no longer waits on"
			}
			fmt.Printf("%s#%d %s %s\n", repo, number, verb, shortKey(target, repo))
		}
		return nil

	case "graph":
		if len(args) > 2 {
			return errUsage
		}
		var repo string
		var err error
		if len(args) == 2 {
			repo = args[1]
		} else if repo, err = checkoutRepo(); err != nil {
			return err
		}
		if err := connect(); err != nil {
			return err
		}
		loaded, found := remote.Repo(repo)
		if !found {
			return fmt.Errorf("%s isn't a repo pr-mon monitors", repo)
		}
		fmt.Println(tui.RepoGraphText(repo, loaded.PRs))
		return nil

	default:
		if len(args) > 1 {
			return errUsage
		}
		repo, number, err := resolvePR(args[0])
		if err != nil {
			return err
		}
		if err := connect(); err != nil {
			return err
		}
		graph, err := remote.DependencyGraph(repo, number)
		if err != nil {
			return err
		}
		fmt.Println(tui.PRGraphText(graph))
		return nil
	}
}

// errUsage asks main to print depUsage.
var errUsage = errors.New("usage")

// resolvePR reads a PR as the user wrote it.
func resolvePR(text string) (string, int, error) {
	if match := bareNumber.FindStringSubmatch(text); match != nil {
		repo, err := checkoutRepo()
		if err != nil {
			return "", 0, fmt.Errorf("%s: %w", text, err)
		}
		number, _ := strconv.Atoi(match[1])
		return repo, number, nil
	}
	return dependencies.ParseRef(text)
}

// checkoutRepo is owner/repo of the git checkout's origin remote.
func checkoutRepo() (string, error) {
	output, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", errors.New("not in a git checkout with an origin remote; name the PR as owner/repo#12")
	}
	match := githubRemote.FindStringSubmatch(strings.TrimSpace(string(output)))
	if match == nil {
		return "", fmt.Errorf("origin isn't on GitHub: %s", strings.TrimSpace(string(output)))
	}
	return match[1], nil
}

// shortKey is "#12" for a PR in repo, else owner/repo#12.
func shortKey(key, repo string) string {
	if number, found := strings.CutPrefix(key, repo+"#"); found {
		return "#" + number
	}
	return key
}
