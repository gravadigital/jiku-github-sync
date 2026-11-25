package mapping

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseProjectRef(t *testing.T) {
	for in, want := range map[string]ProjectRef{
		"acme-org/12":   {Owner: "acme-org", Number: 12},
		" acme-org/12 ": {Owner: "acme-org", Number: 12},
		"https://github.com/orgs/acme-org/projects/12":         {Owner: "acme-org", Number: 12},
		"https://github.com/orgs/acme-org/projects/12/views/3": {Owner: "acme-org", Number: 12},
		"https://github.com/users/some-user/projects/4/":       {Owner: "some-user", Number: 4},
		"PVT_kwDOAbCdEf": {NodeID: "PVT_kwDOAbCdEf"},
	} {
		got, err := ParseProjectRef(in)
		if err != nil || got != want {
			t.Errorf("%q -> %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"acme-org/x", "acme-org/0", "/12", "acme-org/12/3", "https://github.com/acme-org/example-repo",
		"https://github.com/orgs/acme-org/projects/x", "two words"} {
		if _, err := ParseProjectRef(in); err == nil {
			t.Errorf("%q must be rejected", in)
		}
	}
}

func TestGithubProjectInMapping(t *testing.T) {
	f, err := Parse([]byte(`
projects:
  - { jikuProject: 1, repos: [a/b], defaultRepo: a/b, githubProject: PVT_x }
  - { jikuProject: 2, repos: [a/c], defaultRepo: a/c, githubProject: acme-org/12 }
  - { jikuProject: 3, repos: [a/d], defaultRepo: a/d }
`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Project(1).GithubProject != "PVT_x" {
		t.Fatal("a node id is used as it is, without asking GitHub")
	}
	if f.Project(2).GithubProject != "" || f.Project(2).GithubProjectRef != "acme-org/12" {
		t.Fatal("owner/number waits for ResolveProjects")
	}

	_, err = Parse([]byte(`
credentials:
  app-b: { app: { installations: { org-b: 7 } } }
projects:
  - { jikuProject: 1, repos: [a/b], defaultRepo: a/b, credentials: app-b, githubProject: "org-b/x" }
  - { jikuProject: 2, repos: [org-b/c], defaultRepo: org-b/c, credentials: app-b, githubProject: other-org/3 }
`))
	for _, want := range []string{"projects[0].githubProject", "the Project must be in org-b, not other-org"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in: %v", want, err)
		}
	}
}

type finderFunc func(owner string, number int) (string, error)

func (f finderFunc) ProjectNodeID(_ context.Context, owner string, number int) (string, error) {
	return f(owner, number)
}

func TestResolveProjects(t *testing.T) {
	const yml = `
projects:
  - { jikuProject: 1, repos: [a/b], defaultRepo: a/b, githubProject: PVT_x }
  - { jikuProject: 2, repos: [a/c], defaultRepo: a/c, githubProject: "https://github.com/orgs/acme-org/projects/12" }
  - { jikuProject: 3, repos: [a/d], defaultRepo: a/d }
`
	calls := 0
	finder := finderFunc(func(owner string, number int) (string, error) {
		calls++
		if owner != "acme-org" || number != 12 {
			t.Errorf("asked for %s/%d", owner, number)
		}
		return "PVT_resolved", nil
	})
	f, _ := Parse([]byte(yml))
	if err := f.ResolveProjects(context.Background(), nil, func(*Project) ProjectFinder { return finder }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || f.Project(2).GithubProject != "PVT_resolved" || f.Project(1).GithubProject != "PVT_x" || f.Project(3).GithubProject != "" {
		t.Fatalf("calls %d, projects %q %q %q", calls, f.Project(1).GithubProject, f.Project(2).GithubProject, f.Project(3).GithubProject)
	}

	// A reload that does not touch the reference keeps the id without asking GitHub again.
	g, _ := Parse([]byte(yml))
	failing := finderFunc(func(string, int) (string, error) { return "", errors.New("github down") })
	if err := g.ResolveProjects(context.Background(), f, func(*Project) ProjectFinder { return failing }); err != nil || g.Project(2).GithubProject != "PVT_resolved" {
		t.Fatalf("unchanged reference must be reused: %v %q", err, g.Project(2).GithubProject)
	}

	// A changed reference is asked again, and a failure names the project.
	h, _ := Parse([]byte(strings.Replace(yml, "projects/12", "projects/13", 1)))
	err := h.ResolveProjects(context.Background(), f, func(*Project) ProjectFinder { return failing })
	if err == nil || !strings.Contains(err.Error(), "projects[1].githubProject acme-org/13") || !strings.Contains(err.Error(), "github down") {
		t.Fatalf("got %v", err)
	}
}
