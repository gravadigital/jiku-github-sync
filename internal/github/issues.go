package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Issue is the subset of an issue the connector uses.
type Issue struct {
	NodeID      string
	Repo        string // owner/name
	Number      int
	Title       string
	Body        string
	State       string // open | closed
	StateReason string // completed | not_planned | duplicate | reopened | ""
	Author      string
	ClosedBy    string
	IssueType   string
	Labels      []string
	HTMLURL     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	IsPR        bool
}

// Open reports whether the issue is open.
func (i *Issue) Open() bool { return i.State == "open" }

// Comment is an issue comment.
type Comment struct {
	ID        int64
	IssueNum  int
	Body      string
	Author    string
	HTMLURL   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type restUser struct {
	Login string `json:"login"`
}

type restIssue struct {
	NodeID      string                 `json:"node_id"`
	Number      int                    `json:"number"`
	Title       string                 `json:"title"`
	Body        *string                `json:"body"`
	State       string                 `json:"state"`
	StateReason *string                `json:"state_reason"`
	User        *restUser              `json:"user"`
	ClosedBy    *restUser              `json:"closed_by"`
	HTMLURL     string                 `json:"html_url"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
	PullRequest *struct{}              `json:"pull_request"`
	Type        *struct{ Name string } `json:"type"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	RepositoryURL string `json:"repository_url"`
}

func (r *restIssue) toIssue(repo string) *Issue {
	i := &Issue{
		NodeID: r.NodeID, Repo: repo, Number: r.Number, Title: r.Title, State: r.State,
		HTMLURL: r.HTMLURL, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, IsPR: r.PullRequest != nil,
	}
	if r.Body != nil {
		i.Body = *r.Body
	}
	if r.StateReason != nil {
		i.StateReason = *r.StateReason
	}
	if r.User != nil {
		i.Author = r.User.Login
	}
	if r.ClosedBy != nil {
		i.ClosedBy = r.ClosedBy.Login
	}
	if r.Type != nil {
		i.IssueType = r.Type.Name
	}
	for _, l := range r.Labels {
		i.Labels = append(i.Labels, l.Name)
	}
	if i.Repo == "" {
		i.Repo = repoFromURL(r.RepositoryURL)
	}
	return i
}

func repoFromURL(u string) string {
	const marker = "/repos/"
	if k := strings.Index(u, marker); k >= 0 {
		return u[k+len(marker):]
	}
	return ""
}

type restComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      *restUser `json:"user"`
	HTMLURL   string    `json:"html_url"`
	IssueURL  string    `json:"issue_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (r *restComment) toComment() *Comment {
	c := &Comment{ID: r.ID, Body: r.Body, HTMLURL: r.HTMLURL, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if r.User != nil {
		c.Author = r.User.Login
	}
	if k := strings.LastIndex(r.IssueURL, "/"); k >= 0 {
		c.IssueNum, _ = strconv.Atoi(r.IssueURL[k+1:])
	}
	return c
}

// Viewer returns the login of the token's account.
func (c *Client) Viewer(ctx context.Context) (string, error) {
	var u restUser
	if _, err := c.do(ctx, http.MethodGet, "/user", nil, &u, false); err != nil {
		return "", err
	}
	return u.Login, nil
}

// Repo is the subset of a repository the connector checks.
type Repo struct {
	ID         int64  `json:"id"`
	FullName   string `json:"full_name"`
	Visibility string `json:"visibility"`
	Private    bool   `json:"private"`
	NodeID     string `json:"node_id"`
	HasIssues  bool   `json:"has_issues"`
}

// GetRepo reads a repository.
func (c *Client) GetRepo(ctx context.Context, repo string) (*Repo, error) {
	var r Repo
	if _, err := c.do(ctx, http.MethodGet, "/repos/"+repo, nil, &r, false); err != nil {
		return nil, err
	}
	return &r, nil
}

// OrgIssueTypes lists the names of an organization's issue types (empty for a user account or
// when issue types are not available).
func (c *Client) OrgIssueTypes(ctx context.Context, org string) ([]string, error) {
	var ts []struct {
		Name      string `json:"name"`
		IsEnabled *bool  `json:"is_enabled"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/orgs/"+org+"/issue-types", nil, &ts, false); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		if t.IsEnabled == nil || *t.IsEnabled {
			names = append(names, t.Name)
		}
	}
	return names, nil
}

// ListIssues lists the issues of a repo updated at or after since (all of them with a zero since),
// oldest update first. Pull requests are left out.
func (c *Client) ListIssues(ctx context.Context, repo string, since time.Time, state string) ([]*Issue, error) {
	q := url.Values{"state": {state}, "sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	raw, err := getAll[restIssue](ctx, c, "/repos/"+repo+"/issues?"+q.Encode())
	if err != nil {
		return nil, err
	}
	out := make([]*Issue, 0, len(raw))
	for i := range raw {
		is := raw[i].toIssue(repo)
		if !is.IsPR {
			out = append(out, is)
		}
	}
	return out, nil
}

// GetIssue reads one issue. A transferred issue answers 301 to the old path; the HTTP client
// follows it, so the returned Repo is taken from the answer, not from the argument.
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (*Issue, error) {
	var r restIssue
	if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil, &r, false); err != nil {
		return nil, err
	}
	return r.toIssue(""), nil
}

// IssueRef locates an issue by its node id: where it lives now (it may have been transferred).
func (c *Client) IssueRef(ctx context.Context, nodeID string) (repo string, number int, err error) {
	var out struct {
		Node *struct {
			Number     int `json:"number"`
			Repository struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"repository"`
		} `json:"node"`
	}
	q := `query($id:ID!){node(id:$id){... on Issue{number repository{nameWithOwner}}}}`
	if err := c.graphql(ctx, q, map[string]any{"id": nodeID}, &out, false); err != nil {
		return "", 0, err
	}
	if out.Node == nil || out.Node.Number == 0 {
		return "", 0, &APIError{Status: http.StatusNotFound, Method: "POST", Path: "/graphql", Message: "issue " + nodeID}
	}
	return out.Node.Repository.NameWithOwner, out.Node.Number, nil
}

// IssueInput is the body of a create or update. Nil fields are not sent.
type IssueInput struct {
	Title       *string `json:"title,omitempty"`
	Body        *string `json:"body,omitempty"`
	State       *string `json:"state,omitempty"`
	StateReason *string `json:"state_reason,omitempty"`
	// Type is the issue type name; a pointer to "" removes it.
	Type *string `json:"-"`
}

func (in IssueInput) payload() map[string]any {
	m := map[string]any{}
	if in.Title != nil {
		m["title"] = *in.Title
	}
	if in.Body != nil {
		m["body"] = *in.Body
	}
	if in.State != nil {
		m["state"] = *in.State
	}
	if in.StateReason != nil {
		m["state_reason"] = *in.StateReason
	}
	if in.Type != nil {
		if *in.Type == "" {
			m["type"] = nil
		} else {
			m["type"] = *in.Type
		}
	}
	return m
}

// CreateIssue creates an issue.
func (c *Client) CreateIssue(ctx context.Context, repo string, in IssueInput) (*Issue, error) {
	var r restIssue
	if _, err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/issues", in.payload(), &r, true); err != nil {
		return nil, err
	}
	return r.toIssue(repo), nil
}

// UpdateIssue patches an issue.
func (c *Client) UpdateIssue(ctx context.Context, repo string, number int, in IssueInput) (*Issue, error) {
	var r restIssue
	if _, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, number), in.payload(), &r, false); err != nil {
		return nil, err
	}
	return r.toIssue(""), nil
}

// ListComments lists every comment of an issue, oldest first.
func (c *Client) ListComments(ctx context.Context, repo string, number int) ([]*Comment, error) {
	raw, err := getAll[restComment](ctx, c, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, number))
	if err != nil {
		return nil, err
	}
	out := make([]*Comment, 0, len(raw))
	for i := range raw {
		out = append(out, raw[i].toComment())
	}
	return out, nil
}

// ListRepoComments lists the comments of a whole repo updated at or after since.
func (c *Client) ListRepoComments(ctx context.Context, repo string, since time.Time) ([]*Comment, error) {
	q := url.Values{"sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	raw, err := getAll[restComment](ctx, c, "/repos/"+repo+"/issues/comments?"+q.Encode())
	if err != nil {
		return nil, err
	}
	out := make([]*Comment, 0, len(raw))
	for i := range raw {
		out = append(out, raw[i].toComment())
	}
	return out, nil
}

// CreateComment adds a comment to an issue.
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) (*Comment, error) {
	var r restComment
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number), map[string]any{"body": body}, &r, true); err != nil {
		return nil, err
	}
	return r.toComment(), nil
}

// UpdateComment replaces a comment's body.
func (c *Client) UpdateComment(ctx context.Context, repo string, id int64, body string) error {
	_, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, id), map[string]any{"body": body}, nil, false)
	return err
}
