package initial

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

type subsGH struct {
	fakeGH
	subs map[int][]*github.Issue // parent number -> sub-issues
}

func (f *subsGH) ListSubIssues(_ context.Context, _ string, n int) ([]*github.Issue, error) {
	return f.subs[n], nil
}

type tasksJiku struct {
	fakeJiku
	tasks []*jiku.Task
}

func (f *tasksJiku) ListTasks(_ context.Context, p int64, filter map[string]any) ([]*jiku.Task, error) {
	var out []*jiku.Task
	for _, t := range f.tasks {
		if t.ProjectID == p && (filter["visibilityLevel"] == nil || filter["visibilityLevel"] == t.VisibilityLevel) {
			out = append(out, t)
		}
	}
	return out, nil
}

func task(id, req int64, title, st, vis string) *jiku.Task {
	t := &jiku.Task{ID: id, Title: title, State: st, VisibilityLevel: vis, ProjectID: 7}
	if req != 0 {
		t.RequirementID = &req
	}
	return t
}

func sub(n int, title, body, st string) *github.Issue {
	return &github.Issue{NodeID: "S" + title, Repo: "acme/api", Number: n, Title: title, Body: body, State: st, ParentRepo: "acme/api", ParentNumber: 1}
}

func TestTaskPlan(t *testing.T) {
	ctx := context.Background()
	st := state.New(state.NewMem())
	for _, l := range []*state.Link{
		{ReqID: 11, JikuProject: 7, IssueNodeID: "I1", Repo: "acme/api", Number: 1},
		{ReqID: 12, JikuProject: 7, IssueNodeID: "I2", Repo: "acme/api", Number: 2, Unlinked: true},
		{ReqID: 13, JikuProject: 7, IssueNodeID: "Sya es requisito", Repo: "acme/api", Number: 9},
	} {
		if err := st.SaveLink(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	gh := &subsGH{subs: map[int][]*github.Issue{1: {
		sub(5, "Mismo título", "", "open"),
		sub(6, "Solo en GitHub", "", "open"),
		sub(7, "Cerrado", "", "closed"),
		sub(8, "Creada por el conector", text.SubIssueBody("x", 23), "open"),
		sub(9, "ya es requisito", "", "open"),
	}}}
	jk := &tasksJiku{tasks: []*jiku.Task{
		task(21, 11, "Mismo título", "activo", "public"),
		task(22, 11, "Solo en Jiku", "backlog", "public"),
		task(23, 11, "Otro nombre", "en_revision", "public"),
		task(24, 11, "Interna", "backlog", "internal"),
		task(25, 11, "Finalizada", "finalizado", "public"),
		task(26, 0, "Sin requisito", "backlog", "public"),
		task(27, 12, "De un requisito desvinculado", "backlog", "public"),
	}}
	p := &mapping.Project{JikuProject: 7, Repos: []string{"acme/api"}, DefaultRepo: "acme/api"}
	pl, err := BuildTasks(ctx, Deps{GitHub: gh, Jiku: jk, Store: st, WriteInterval: time.Second}, gh, p)
	if err != nil {
		t.Fatal(err)
	}
	links := map[int64]string{}
	for _, x := range pl.Link {
		links[x.Task.ID] = x.Reason + ":" + x.Issue.Title
	}
	if len(links) != 2 || links[21] != "title:Mismo título" || links[23] != "marker:Creada por el conector" {
		t.Fatalf("links: %v", links)
	}
	if len(pl.Export) != 1 || pl.Export[0].Task.ID != 22 || pl.Export[0].Req.ReqID != 11 {
		t.Fatalf("export: %+v", pl.Export)
	}
	if len(pl.Import) != 1 || pl.Import[0].Issue.Number != 6 {
		t.Fatalf("import: %+v", pl.Import)
	}
	if len(pl.Skipped) != 1 || !strings.Contains(pl.Skipped[0], "ya está vinculado como requisito") {
		t.Fatalf("skipped: %v", pl.Skipped)
	}
	// One sub-issue: the issue and putting it under its parent.
	if pl.GitHubCreates != 2 || pl.JikuCreates != 1 {
		t.Fatalf("writes: %d %d", pl.GitHubCreates, pl.JikuCreates)
	}
	var b bytes.Buffer
	pl.Render(&b)
	if !strings.Contains(b.String(), "tarea 22 [backlog] bajo acme/api#1") {
		t.Fatalf("render:\n%s", b.String())
	}
}
