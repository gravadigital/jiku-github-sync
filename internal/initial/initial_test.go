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

type fakeGH struct {
	issues []*github.Issue
	comms  map[int][]*github.Comment
}

func (f *fakeGH) ListIssues(_ context.Context, repo string, _ time.Time, st string) ([]*github.Issue, error) {
	var out []*github.Issue
	for _, i := range f.issues {
		if i.Repo == repo && (st == "all" || i.State == st) {
			out = append(out, i)
		}
	}
	return out, nil
}

func (f *fakeGH) ListComments(_ context.Context, _ string, n int) ([]*github.Comment, error) {
	return f.comms[n], nil
}

type fakeJiku struct {
	jiku.API
	reqs  []*jiku.Requirement
	comms map[int64][]*jiku.Comment
}

func (f *fakeJiku) SelfID() string { return "self" }

func (f *fakeJiku) ListRequirements(_ context.Context, p int64, filter map[string]any) ([]*jiku.Requirement, error) {
	var out []*jiku.Requirement
	for _, r := range f.reqs {
		if r.ProjectID == p && (filter["visibilityLevel"] == nil || filter["visibilityLevel"] == r.VisibilityLevel) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeJiku) ListComments(_ context.Context, id int64) ([]*jiku.Comment, error) {
	return f.comms[id], nil
}

func req(id int64, title, st, vis string) *jiku.Requirement {
	return &jiku.Requirement{ID: id, Title: title, State: st, VisibilityLevel: vis, ProjectID: 7}
}

func issue(n int, title, body string) *github.Issue {
	return &github.Issue{NodeID: "I" + title, Repo: "acme/api", Number: n, Title: title, Body: body, State: "open"}
}

func TestPlan(t *testing.T) {
	gh := &fakeGH{
		issues: []*github.Issue{
			issue(1, "Mismo título", ""),
			issue(2, "Creado por el conector", text.IssueBody("x", 12)),
			issue(3, "Con espacios", ""),
			issue(4, "Repetido", ""),
			issue(5, "Repetido", ""),
			issue(6, "Solo en GitHub", ""),
			issue(8, "Mayúsculas distintas", ""),
			{NodeID: "PR", Repo: "acme/api", Number: 7, Title: "PR", State: "open", IsPR: true},
		},
		comms: map[int][]*github.Comment{6: {{ID: 1, Body: "hola"}, {ID: 2, Body: text.Notice("aviso")}}},
	}
	jk := &fakeJiku{
		reqs: []*jiku.Requirement{
			req(11, "Mismo título", "analisis", "public"),
			req(12, "otro nombre", "desarrollo", "public"),
			req(13, "  Con espacios ", "revision", "public"),
			req(14, "Repetido", "analisis", "public"),
			req(15, "Solo en Jiku", "planificacion", "public"),
			req(16, "Interno", "analisis", "internal"),
			req(17, "Cerrado", "resuelto", "public"),
			req(18, "mayúsculas distintas", "analisis", "public"),
		},
		comms: map[int64][]*jiku.Comment{15: {
			{ID: 1, AuthorID: "person", VisibilityLevel: "public"},
			{ID: 2, AuthorID: "person", VisibilityLevel: "internal"},
			{ID: 3, AuthorID: "self", VisibilityLevel: "public"},
		}},
	}
	p := &mapping.Project{JikuProject: 7, Repos: []string{"acme/api"}, DefaultRepo: "acme/api", GithubProject: "P"}
	pl, err := Build(context.Background(), Deps{GitHub: gh, Jiku: jk, Store: state.New(state.NewMem()), WriteInterval: time.Second}, p)
	if err != nil {
		t.Fatal(err)
	}
	links := map[int64]string{}
	for _, l := range pl.Link {
		links[l.Req.ID] = l.Reason
	}
	if links[11] != "title" || links[12] != "marker" || links[13] != "title" || len(links) != 3 {
		t.Fatalf("links: %v (exact title, surrounding spaces ignored; the connector's own issue by marker)", links)
	}
	var exp []int64
	for _, r := range pl.Export {
		exp = append(exp, r.ID)
	}
	if len(exp) != 3 || exp[0] != 14 || exp[1] != 15 || exp[2] != 18 {
		t.Fatalf("export: %v (a different case is not the same title; a repeated title is not paired; internal and closed are left out)", exp)
	}
	var imp []int
	for _, is := range pl.Import {
		imp = append(imp, is.Number)
	}
	if len(imp) != 4 || imp[0] != 4 || imp[1] != 5 || imp[2] != 6 || imp[3] != 8 {
		t.Fatalf("import: %v (pull requests are left out)", imp)
	}
	if pl.CommentsToGitHub != 1 || pl.CommentsToJiku != 1 {
		t.Fatalf("comments: %d to GitHub, %d to Jiku", pl.CommentsToGitHub, pl.CommentsToJiku)
	}
	var out bytes.Buffer
	pl.Render(&out)
	for _, want := range []string{"Se vinculan (ya existen en los dos lados): 3", "título repetido", "mails"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("render lacks %q:\n%s", want, out.String())
		}
	}
}

func TestEstimate(t *testing.T) {
	if got := estimate(10, time.Second); got != 10*time.Second {
		t.Errorf("10 creations at 1s: %s", got)
	}
	if got := estimate(1200, 0); got != 2*time.Hour {
		t.Errorf("1200 creations are bound by 500/hour: %s", got)
	}
	if got := estimate(0, time.Second); got != 0 {
		t.Errorf("nothing to do: %s", got)
	}
}
