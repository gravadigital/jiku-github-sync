package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Sub-issues. Linking or unlinking a sub-issue does NOT change either issue's updated_at, so a
// poll by `since` never sees it: the poller sweeps the parents of each repo (IssueParents) and
// the timeline says who changed a parent and when (LastParentChange).

// AddSubIssue makes the issue with numeric id subID a sub-issue of parentRepo#parentNumber. With
// replace, an issue that already has another parent is moved (otherwise GitHub refuses it).
func (c *Client) AddSubIssue(ctx context.Context, parentRepo string, parentNumber int, subID int64, replace bool) error {
	body := map[string]any{"sub_issue_id": subID}
	if replace {
		body["replace_parent"] = true
	}
	_, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/sub_issues", parentRepo, parentNumber), body, nil, false)
	return err
}

// RemoveSubIssue detaches the issue with numeric id subID from parentRepo#parentNumber.
func (c *Client) RemoveSubIssue(ctx context.Context, parentRepo string, parentNumber int, subID int64) error {
	_, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/%d/sub_issue", parentRepo, parentNumber), map[string]any{"sub_issue_id": subID}, nil, false)
	return err
}

// ListSubIssues lists the sub-issues of an issue (they may live in other repos).
func (c *Client) ListSubIssues(ctx context.Context, repo string, number int) ([]*Issue, error) {
	raw, err := getAll[restIssue](ctx, c, fmt.Sprintf("/repos/%s/issues/%d/sub_issues?per_page=100", repo, number))
	if err != nil {
		return nil, err
	}
	out := make([]*Issue, 0, len(raw))
	for i := range raw {
		out = append(out, raw[i].toIssue(""))
	}
	return out, nil
}

// IssueParent is an open issue and its parent ("" without one), as the parents sweep reads them.
type IssueParent struct {
	NodeID string
	Number int
	// Parent is "owner/repo#number".
	Parent string
}

// IssueParents lists the open issues of a repo with their parent, 100 per query.
func (c *Client) IssueParents(ctx context.Context, repo string) ([]IssueParent, error) {
	owner, name, _ := strings.Cut(repo, "/")
	q := `query($o:String!,$n:String!,$after:String){repository(owner:$o,name:$n){issues(first:100,after:$after,states:[OPEN]){
pageInfo{hasNextPage endCursor} nodes{id number parent{number repository{nameWithOwner}}}}}}`
	var out []IssueParent
	vars := map[string]any{"o": owner, "n": name, "after": nil}
	for {
		var resp struct {
			Repository *struct {
				Issues struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID     string `json:"id"`
						Number int    `json:"number"`
						Parent *struct {
							Number     int `json:"number"`
							Repository struct {
								NameWithOwner string `json:"nameWithOwner"`
							} `json:"repository"`
						} `json:"parent"`
					} `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		}
		if err := c.graphql(ctx, q, vars, &resp, false); err != nil {
			return nil, err
		}
		if resp.Repository == nil {
			return nil, &APIError{Status: http.StatusNotFound, Method: "POST", Path: "/graphql", Message: "repository " + repo}
		}
		for _, n := range resp.Repository.Issues.Nodes {
			ip := IssueParent{NodeID: n.ID, Number: n.Number}
			if n.Parent != nil {
				ip.Parent = fmt.Sprintf("%s#%d", n.Parent.Repository.NameWithOwner, n.Parent.Number)
			}
			out = append(out, ip)
		}
		if !resp.Repository.Issues.PageInfo.HasNextPage {
			return out, nil
		}
		vars["after"] = resp.Repository.Issues.PageInfo.EndCursor
	}
}

// ParentChange is the latest change of an issue's parent on its timeline.
type ParentChange struct {
	// Added is false when the parent was removed.
	Added     bool
	Parent    string // "owner/repo#number"
	Actor     string
	CreatedAt time.Time
}

// LastParentChange reads the latest parent change of an issue (nil when there is none). The
// timeline lags a few seconds behind the change.
func (c *Client) LastParentChange(ctx context.Context, issueNodeID string) (*ParentChange, error) {
	q := `query($id:ID!){node(id:$id){... on Issue{timelineItems(last:1,itemTypes:[PARENT_ISSUE_ADDED_EVENT,PARENT_ISSUE_REMOVED_EVENT]){nodes{
__typename ... on ParentIssueAddedEvent{createdAt actor{login} parent{number repository{nameWithOwner}}}
... on ParentIssueRemovedEvent{createdAt actor{login} parent{number repository{nameWithOwner}}}}}}}}`
	var resp struct {
		Node *struct {
			TimelineItems struct {
				Nodes []struct {
					Typename  string    `json:"__typename"`
					CreatedAt time.Time `json:"createdAt"`
					Actor     *struct {
						Login string `json:"login"`
					} `json:"actor"`
					Parent *struct {
						Number     int `json:"number"`
						Repository struct {
							NameWithOwner string `json:"nameWithOwner"`
						} `json:"repository"`
					} `json:"parent"`
				} `json:"nodes"`
			} `json:"timelineItems"`
		} `json:"node"`
	}
	if err := c.graphql(ctx, q, map[string]any{"id": issueNodeID}, &resp, false); err != nil {
		return nil, err
	}
	if resp.Node == nil || len(resp.Node.TimelineItems.Nodes) == 0 {
		return nil, nil
	}
	n := resp.Node.TimelineItems.Nodes[0]
	pc := &ParentChange{Added: n.Typename == "ParentIssueAddedEvent", CreatedAt: n.CreatedAt}
	if n.Actor != nil {
		pc.Actor = n.Actor.Login
	}
	if n.Parent != nil {
		pc.Parent = fmt.Sprintf("%s#%d", n.Parent.Repository.NameWithOwner, n.Parent.Number)
	}
	return pc, nil
}
