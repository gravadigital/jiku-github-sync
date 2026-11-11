package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
)

// clock is a manual clock shared by the fakes, so "who changed last" is deterministic.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) tick() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

// ---- GitHub ----

type fakeGH struct {
	mu      sync.Mutex
	clk     *clock
	issues  map[string]*github.Issue // node id
	comms   map[string][]*github.Comment
	items   map[string]*github.Item // issue node id -> item
	schema  *github.ProjectSchema
	nextNum map[string]int
	nextID  int64
	// automationStatus is what the "item added" workflow sets ("" = no workflow).
	automationStatus string
	writes           []string
	issueTypes       map[string]bool // nil = every type accepted
}

func newFakeGH(clk *clock) *fakeGH {
	return &fakeGH{clk: clk, issues: map[string]*github.Issue{}, comms: map[string][]*github.Comment{},
		items: map[string]*github.Item{}, nextNum: map[string]int{}, nextID: 1000}
}

func (f *fakeGH) write(s string, a ...any) { f.writes = append(f.writes, fmt.Sprintf(s, a...)) }

func (f *fakeGH) takeWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.writes
	f.writes = nil
	return w
}

func (f *fakeGH) byNum(repo string, n int) *github.Issue {
	for _, is := range f.issues {
		if strings.EqualFold(is.Repo, repo) && is.Number == n {
			return is
		}
	}
	return nil
}

func cp(is *github.Issue) *github.Issue {
	c := *is
	c.Labels = append([]string(nil), is.Labels...)
	return &c
}

// newIssue simulates a person opening an issue.
func (f *fakeGH) newIssue(repo, title, body, author string) *github.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextNum[repo]++
	n := f.nextNum[repo]
	now := f.clk.tick()
	is := &github.Issue{NodeID: fmt.Sprintf("I_%s_%d", strings.ReplaceAll(repo, "/", "_"), n), Repo: repo, Number: n,
		Title: title, Body: body, State: "open", Author: author, HTMLURL: fmt.Sprintf("https://github.com/%s/issues/%d", repo, n),
		CreatedAt: now, UpdatedAt: now}
	f.issues[is.NodeID] = is
	return cp(is)
}

func (f *fakeGH) edit(node string, fn func(*github.Issue)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[node]
	fn(is)
	is.UpdatedAt = f.clk.tick()
}

// setStatus simulates a person (or the automation) moving the card.
func (f *fakeGH) setField(node, field, option, by string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.items[node]
	it.Values[field] = github.FieldValue{Option: option, UpdatedAt: f.clk.tick(), Creator: by}
}

func (f *fakeGH) comment(node, author, body string) *github.Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[node]
	f.nextID++
	now := f.clk.tick()
	c := &github.Comment{ID: f.nextID, IssueNum: is.Number, Body: body, Author: author,
		HTMLURL: fmt.Sprintf("%s#issuecomment-%d", is.HTMLURL, f.nextID), CreatedAt: now, UpdatedAt: now}
	f.comms[node] = append(f.comms[node], c)
	return c
}

func (f *fakeGH) GetIssue(_ context.Context, repo string, n int) (*github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if is := f.byNum(repo, n); is != nil {
		return cp(is), nil
	}
	return nil, &github.APIError{Status: 404, Message: "not found"}
}

func (f *fakeGH) IssueRef(_ context.Context, node string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if is := f.issues[node]; is != nil {
		return is.Repo, is.Number, nil
	}
	return "", 0, &github.APIError{Status: 404}
}

func (f *fakeGH) ListIssues(_ context.Context, repo string, since time.Time, _ string) ([]*github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*github.Issue
	for _, is := range f.issues {
		if strings.EqualFold(is.Repo, repo) && !is.UpdatedAt.Before(since) {
			out = append(out, cp(is))
		}
	}
	return out, nil
}

func (f *fakeGH) CreateIssue(_ context.Context, repo string, in github.IssueInput) (*github.Issue, error) {
	f.mu.Lock()
	if in.Type != nil && f.issueTypes != nil && !f.issueTypes[*in.Type] {
		f.mu.Unlock()
		return nil, &github.APIError{Status: 422, Message: "invalid type"}
	}
	f.mu.Unlock()
	is := f.newIssue(repo, *in.Title, *in.Body, "connector")
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.Type != nil {
		f.issues[is.NodeID].IssueType = *in.Type
	}
	f.write("create issue %s#%d", repo, is.Number)
	return cp(f.issues[is.NodeID]), nil
}

func (f *fakeGH) UpdateIssue(_ context.Context, repo string, n int, in github.IssueInput) (*github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.byNum(repo, n)
	if in.Type != nil && f.issueTypes != nil && *in.Type != "" && !f.issueTypes[*in.Type] {
		return nil, &github.APIError{Status: 422, Message: "invalid type"}
	}
	if in.Title != nil {
		is.Title = *in.Title
		f.write("issue title %q", *in.Title)
	}
	if in.Body != nil {
		is.Body = *in.Body
		f.write("issue body")
	}
	if in.State != nil {
		is.State = *in.State
		is.StateReason = ""
		if in.StateReason != nil {
			is.StateReason = *in.StateReason
		}
		if is.State == "open" {
			is.StateReason = "reopened"
		}
		f.write("issue state %s %s", is.State, is.StateReason)
		// The "Item closed" workflow.
		if is.State == "closed" {
			if it := f.items[is.NodeID]; it != nil && f.schema != nil {
				it.Values["Status"] = github.FieldValue{Option: "Done", UpdatedAt: f.clk.tick(), Creator: AutomationLogin}
			}
		}
	}
	if in.Type != nil {
		is.IssueType = *in.Type
		f.write("issue type %q", *in.Type)
	}
	is.UpdatedAt = f.clk.tick()
	return cp(is), nil
}

func (f *fakeGH) ListComments(_ context.Context, repo string, n int) ([]*github.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.byNum(repo, n)
	var out []*github.Comment
	for _, c := range f.comms[is.NodeID] {
		cc := *c
		out = append(out, &cc)
	}
	return out, nil
}

func (f *fakeGH) CreateComment(_ context.Context, repo string, n int, body string) (*github.Comment, error) {
	f.mu.Lock()
	is := f.byNum(repo, n)
	f.mu.Unlock()
	c := f.comment(is.NodeID, "connector", body)
	f.mu.Lock()
	f.write("comment %q", firstLine(body))
	f.mu.Unlock()
	return c, nil
}

func (f *fakeGH) UpdateComment(_ context.Context, _ string, id int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, cs := range f.comms {
		for _, c := range cs {
			if c.ID == id {
				c.Body = body
				c.UpdatedAt = f.clk.tick()
				f.write("edit comment %d", id)
				return nil
			}
		}
	}
	return &github.APIError{Status: 404}
}

func (f *fakeGH) deleteComment(node string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.comms[node]
	for i, c := range cs {
		if c.ID == id {
			f.comms[node] = append(cs[:i], cs[i+1:]...)
			return
		}
	}
}

func (f *fakeGH) ProjectSchema(context.Context, string) (*github.ProjectSchema, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.schema, nil
}

func (f *fakeGH) ItemForIssue(_ context.Context, node, _ string) (*github.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.items[node]
	if it == nil {
		return nil, nil
	}
	c := &github.Item{ID: it.ID, ContentNodeID: it.ContentNodeID, Values: map[string]github.FieldValue{}}
	for k, v := range it.Values {
		c.Values[k] = v
	}
	return c, nil
}

func (f *fakeGH) AddItem(_ context.Context, _ string, node string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it := f.items[node]; it != nil {
		return it.ID, nil
	}
	it := &github.Item{ID: "ITEM_" + node, ContentNodeID: node, Values: map[string]github.FieldValue{}}
	if f.automationStatus != "" {
		it.Values["Status"] = github.FieldValue{Option: f.automationStatus, UpdatedAt: f.clk.tick(), Creator: AutomationLogin}
	}
	f.items[node] = it
	f.write("add item")
	return it.ID, nil
}

func (f *fakeGH) optionName(fieldID, optID string) (string, string) {
	for _, fl := range f.schema.Fields {
		if fl.ID == fieldID {
			for name, id := range fl.Options {
				if id == optID {
					return fl.Name, name
				}
			}
			return fl.Name, ""
		}
	}
	return "", ""
}

func (f *fakeGH) SetSelect(_ context.Context, _, itemID, fieldID, optID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	field, name := f.optionName(fieldID, optID)
	for _, it := range f.items {
		if it.ID == itemID {
			it.Values[field] = github.FieldValue{Option: name, UpdatedAt: f.clk.tick(), Creator: "connector"}
			f.write("set %s=%s", field, name)
			return nil
		}
	}
	return fmt.Errorf("no item %s", itemID)
}

func (f *fakeGH) ClearField(_ context.Context, _, itemID, fieldID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	field, _ := f.optionName(fieldID, "")
	for _, it := range f.items {
		if it.ID == itemID {
			delete(it.Values, field)
			f.write("clear %s", field)
			return nil
		}
	}
	return fmt.Errorf("no item %s", itemID)
}

func schemaOf(fields map[string][]string) *github.ProjectSchema {
	s := &github.ProjectSchema{ID: "P", Fields: map[string]*github.SelectField{}}
	for name, opts := range fields {
		f := &github.SelectField{ID: "F_" + name, Name: name, Options: map[string]string{}}
		for _, o := range opts {
			f.Options[o] = "O_" + o
			f.Order = append(f.Order, o)
		}
		s.Fields[name] = f
	}
	return s
}

// ---- Jiku ----

type fakeJiku struct {
	mu     sync.Mutex
	clk    *clock
	self   string
	reqs   map[int64]*jiku.Requirement
	comms  map[int64][]*jiku.Comment
	nextID int64
	writes []string
}

func newFakeJiku(clk *clock) *fakeJiku {
	return &fakeJiku{clk: clk, self: "connector-sub", reqs: map[int64]*jiku.Requirement{}, comms: map[int64][]*jiku.Comment{}, nextID: 100}
}

func (f *fakeJiku) takeWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.writes
	f.writes = nil
	return w
}

func (f *fakeJiku) SelfID() string { return f.self }

func cpReq(r *jiku.Requirement) *jiku.Requirement {
	c := *r
	if r.Type != nil {
		t := *r.Type
		c.Type = &t
	}
	return &c
}

// newReq simulates a person creating a requirement.
func (f *fakeJiku) newReq(project int64, title, desc, typ, visibility string) *jiku.Requirement {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	now := f.clk.tick()
	r := &jiku.Requirement{ID: f.nextID, Title: title, Description: desc, Priority: "sin_prioridad", State: "analisis",
		ProjectID: project, CreatedBy: "person", VisibilityLevel: visibility, CreatedAt: now, UpdatedAt: now}
	if typ != "" {
		r.Type = &typ
	}
	f.reqs[r.ID] = r
	return cpReq(r)
}

// editReq simulates a person editing.
func (f *fakeJiku) editReq(id int64, fn func(*jiku.Requirement)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.reqs[id])
	f.reqs[id].UpdatedAt = f.clk.tick()
}

func (f *fakeJiku) personComment(req int64, body, vis string) *jiku.Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	now := f.clk.tick()
	c := &jiku.Comment{ID: f.nextID, Body: body, AuthorID: "person", VisibilityLevel: vis, CreatedAt: now, UpdatedAt: now}
	c.Author = &struct {
		Name     string `json:"name"`
		Username string `json:"username"`
	}{Name: "Ana Pérez"}
	f.comms[req] = append(f.comms[req], c)
	return c
}

func (f *fakeJiku) GetRequirement(_ context.Context, id int64) (*jiku.Requirement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.reqs[id]
	if r == nil {
		return nil, jiku.ErrNotFound
	}
	return cpReq(r), nil
}

func (f *fakeJiku) ListRequirements(_ context.Context, project int64, filter map[string]any) ([]*jiku.Requirement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*jiku.Requirement
	for _, r := range f.reqs {
		if r.ProjectID != project {
			continue
		}
		if cb, ok := filter["createdBy"]; ok && r.CreatedBy != cb {
			continue
		}
		if v, ok := filter["visibilityLevel"]; ok && r.VisibilityLevel != v {
			continue
		}
		out = append(out, cpReq(r))
	}
	return out, nil
}

func (f *fakeJiku) CreateRequirement(_ context.Context, in jiku.NewRequirement) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	now := f.clk.tick()
	r := &jiku.Requirement{ID: f.nextID, Title: in.Title, Description: in.Description, Type: in.Type, Priority: "sin_prioridad",
		State: "analisis", ProjectID: in.ProjectID, CreatedBy: f.self, VisibilityLevel: in.VisibilityLevel, CreatedAt: now, UpdatedAt: now}
	if in.Priority != "" {
		r.Priority = in.Priority
	}
	f.reqs[r.ID] = r
	f.writes = append(f.writes, fmt.Sprintf("create requirement %d", r.ID))
	return r.ID, nil
}

func (f *fakeJiku) EditRequirement(_ context.Context, id int64, fields map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.reqs[id]
	if s, ok := fields["state"].(string); ok && s == "resuelto" && r.TypeOrEmpty() == "incidencia" && fields["resolutionType"] == nil {
		return fmt.Errorf("resolution_required")
	}
	for k, v := range fields {
		switch k {
		case "title":
			r.Title = v.(string)
		case "description":
			r.Description = v.(string)
		case "state":
			r.State = v.(string)
		case "priority":
			r.Priority = v.(string)
		case "type":
			t := v.(string)
			r.Type = &t
		}
		f.writes = append(f.writes, fmt.Sprintf("req.%s=%v", k, v))
	}
	r.UpdatedAt = f.clk.tick()
	return nil
}

func (f *fakeJiku) ListComments(_ context.Context, req int64) ([]*jiku.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*jiku.Comment
	for _, c := range f.comms[req] {
		cc := *c
		out = append(out, &cc)
	}
	return out, nil
}

func (f *fakeJiku) CreateComment(_ context.Context, req int64, body, vis string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	now := f.clk.tick()
	f.comms[req] = append(f.comms[req], &jiku.Comment{ID: f.nextID, Body: body, AuthorID: f.self, VisibilityLevel: vis, CreatedAt: now, UpdatedAt: now})
	f.writes = append(f.writes, fmt.Sprintf("%s comment %q", vis, firstLine(body)))
	return f.nextID, nil
}

func (f *fakeJiku) EditComment(_ context.Context, req, cid int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comms[req] {
		if c.ID == cid {
			if c.AuthorID != f.self {
				return fmt.Errorf("activity_not_editable")
			}
			c.Body = body
			f.writes = append(f.writes, fmt.Sprintf("edit comment %d", cid))
			return nil
		}
	}
	return fmt.Errorf("comment_not_found")
}

func (f *fakeJiku) personEditComment(req, cid int64, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comms[req] {
		if c.ID == cid {
			c.Body = body
		}
	}
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

type (
	jikuReq     = jiku.Requirement
	jikuComment = jiku.Comment
	jikuNew     = jiku.NewRequirement
)
