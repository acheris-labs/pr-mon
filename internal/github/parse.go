// GitHub's GraphQL shapes, and how they become pr-mon's models.

package github

import (
	"github.com/acheris-labs/pr-mon/internal/models"
)

type repoPayload struct {
	NameWithOwner       string `json:"nameWithOwner"`
	MergeCommitAllowed  bool   `json:"mergeCommitAllowed"`
	SquashMergeAllowed  bool   `json:"squashMergeAllowed"`
	RebaseMergeAllowed  bool   `json:"rebaseMergeAllowed"`
	DeleteBranchOnMerge bool   `json:"deleteBranchOnMerge"`
	AutoMergeAllowed    bool   `json:"autoMergeAllowed"`
	Newest              struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Number int `json:"number"`
		} `json:"nodes"`
	} `json:"newest"`
	FirstPage struct {
		Nodes []prNode `json:"nodes"`
	} `json:"firstPage"`
}

// mergeMethods keeps squash, merge, rebase order: the preference order pr-mon offers.
func (r repoPayload) mergeMethods() []models.MergeMethod {
	methods := []models.MergeMethod{}
	if r.SquashMergeAllowed {
		methods = append(methods, models.MergeSquash)
	}
	if r.MergeCommitAllowed {
		methods = append(methods, models.MergeCommit)
	}
	if r.RebaseMergeAllowed {
		methods = append(methods, models.MergeRebase)
	}
	return methods
}

type login struct {
	Login string `json:"login"`
}

type prNode struct {
	ID             string  `json:"id"`
	Number         int     `json:"number"`
	State          string  `json:"state"`
	Title          string  `json:"title"`
	URL            string  `json:"url"`
	CreatedAt      string  `json:"createdAt"`
	IsDraft        bool    `json:"isDraft"`
	Author         *login  `json:"author"`
	HeadRefName    string  `json:"headRefName"`
	BaseRefName    string  `json:"baseRefName"`
	Mergeable      string  `json:"mergeable"`
	MergeState     string  `json:"mergeStateStatus"`
	ReviewDecision *string `json:"reviewDecision"`
	HeadRef        *struct {
		ID string `json:"id"`
	} `json:"headRef"`
	HeadRepository *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"headRepository"`
	AutoMergeRequest *struct {
		MergeMethod string `json:"mergeMethod"`
		EnabledBy   *login `json:"enabledBy"`
	} `json:"autoMergeRequest"`
	// Nodes can hold nulls: an issue in a repository the token cannot read.
	ClosingIssuesReferences *struct {
		Nodes []*struct {
			Number     int    `json:"number"`
			Title      string `json:"title"`
			URL        string `json:"url"`
			Repository *struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"repository"`
		} `json:"nodes"`
	} `json:"closingIssuesReferences"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				OID               string `json:"oid"`
				CommittedDate     string `json:"committedDate"`
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						TotalCount int         `json:"totalCount"`
						Nodes      []checkNode `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

type checkNode struct {
	TypeName   string  `json:"__typename"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Conclusion string  `json:"conclusion"`
	StartedAt  *string `json:"startedAt"`
	Context    string  `json:"context"`
	State      string  `json:"state"`
	CreatedAt  *string `json:"createdAt"`
}

// toCheck flattens the two kinds of check GitHub reports into one shape.
func (c checkNode) toCheck() models.Check {
	if c.TypeName == "StatusContext" {
		return models.Check{Name: c.Context, State: c.State, StartedAt: c.CreatedAt}
	}
	if c.Status != "COMPLETED" {
		return models.Check{Name: c.Name, State: "PENDING", StartedAt: c.StartedAt}
	}
	state := c.Conclusion
	if state == "" {
		state = "PENDING"
	}
	return models.Check{Name: c.Name, State: state, StartedAt: c.StartedAt}
}

func (p prNode) toPullRequest() models.PullRequest {
	pr := models.PullRequest{
		ID:             p.ID,
		Number:         p.Number,
		Title:          p.Title,
		URL:            p.URL,
		Author:         "ghost",
		CreatedAt:      p.CreatedAt,
		IsDraft:        p.IsDraft,
		HeadRef:        p.HeadRefName,
		BaseRef:        p.BaseRefName,
		Mergeable:      p.Mergeable,
		MergeState:     p.MergeState,
		ReviewDecision: p.ReviewDecision,
		Checks:         []models.Check{},
		ClosingIssues:  []models.LinkedIssue{},
		Reasons:        []models.Reason{},
		Actions:        []models.ActionOption{},
		WaitsOn:        []models.PRRef{},
		RequiredBy:     []models.PRRef{},
	}
	if p.Author != nil && p.Author.Login != "" {
		pr.Author = p.Author.Login
	}
	if p.HeadRef != nil {
		pr.HeadRefID = models.Ptr(p.HeadRef.ID)
	}
	if p.HeadRepository != nil {
		pr.HeadRepo = models.Ptr(p.HeadRepository.NameWithOwner)
	}
	if auto := p.AutoMergeRequest; auto != nil {
		enabledBy := "ghost"
		if auto.EnabledBy != nil && auto.EnabledBy.Login != "" {
			enabledBy = auto.EnabledBy.Login
		}
		pr.AutoMerge = &models.AutoMerge{
			Method: models.MergeMethod(auto.MergeMethod), EnabledBy: enabledBy,
		}
	}
	if linked := p.ClosingIssuesReferences; linked != nil {
		for _, issue := range linked.Nodes {
			if issue == nil {
				continue // an issue this token cannot read
			}
			entry := models.LinkedIssue{Number: issue.Number, Title: issue.Title, URL: issue.URL}
			if issue.Repository != nil {
				entry.Repo = issue.Repository.NameWithOwner
			}
			pr.ClosingIssues = append(pr.ClosingIssues, entry)
		}
	}
	if len(p.Commits.Nodes) > 0 {
		commit := p.Commits.Nodes[0].Commit
		if commit.OID != "" {
			pr.HeadSha = models.Ptr(commit.OID)
		}
		if commit.CommittedDate != "" {
			pr.LastCommitAt = models.Ptr(commit.CommittedDate)
		}
		if rollup := commit.StatusCheckRollup; rollup != nil {
			pr.CheckState = models.Ptr(rollup.State)
			pr.ChecksTotal = rollup.Contexts.TotalCount
			for _, node := range rollup.Contexts.Nodes {
				if node.TypeName == "" && node.Name == "" && node.Context == "" {
					continue // an empty node: a check type we didn't ask about
				}
				pr.Checks = append(pr.Checks, node.toCheck())
			}
		}
	}
	return pr
}
