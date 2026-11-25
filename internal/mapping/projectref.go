package mapping

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ProjectRef is a reference to a GitHub Project v2: either its node id, or its owner (an
// organization or a user) and number, which is what the Project's page shows
// (github.com/orgs/<owner>/projects/<number>).
type ProjectRef struct {
	NodeID string
	Owner  string
	Number int
}

// String returns the reference in its shortest form.
func (r ProjectRef) String() string {
	if r.NodeID != "" {
		return r.NodeID
	}
	return fmt.Sprintf("%s/%d", r.Owner, r.Number)
}

// ParseProjectRef accepts the three ways of naming a Project:
//
//	acme-org/12                                     owner/number
//	https://github.com/orgs/acme-org/projects/12    the URL of its page (users/<login>/… too)
//	PVT_kwDOAbCdEf                                  the node id
func ParseProjectRef(s string) (ProjectRef, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		return parseProjectURL(s)
	}
	if owner, num, ok := strings.Cut(s, "/"); ok {
		n, err := strconv.Atoi(num)
		if owner == "" || err != nil || n <= 0 {
			return ProjectRef{}, fmt.Errorf("%q is not owner/number (e.g. acme-org/12), a Project URL or a node id", s)
		}
		return ProjectRef{Owner: owner, Number: n}, nil
	}
	if s == "" || strings.ContainsAny(s, " \t") {
		return ProjectRef{}, fmt.Errorf("%q is not owner/number (e.g. acme-org/12), a Project URL or a node id", s)
	}
	return ProjectRef{NodeID: s}, nil
}

// parseProjectURL reads https://github.com/{orgs|users}/<owner>/projects/<number>[/views/…].
func parseProjectURL(s string) (ProjectRef, error) {
	bad := fmt.Errorf("%q is not a Project URL (https://github.com/orgs/<owner>/projects/<number>)", s)
	u, err := url.Parse(s)
	if err != nil {
		return ProjectRef{}, bad
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || (parts[0] != "orgs" && parts[0] != "users") || parts[1] == "" || parts[2] != "projects" {
		return ProjectRef{}, bad
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return ProjectRef{}, bad
	}
	return ProjectRef{Owner: parts[1], Number: n}, nil
}

// ProjectFinder finds the node id of a Project by its owner and number.
type ProjectFinder interface {
	ProjectNodeID(ctx context.Context, owner string, number int) (string, error)
}

// ResolveProjects fills GithubProject for the projects that name their Project by owner and
// number, asking GitHub with each project's own credentials (finder). A project whose reference
// and credentials did not change from prev (the mapping in use, nil on start) keeps the node id
// it already had, so a reload does not depend on GitHub for what it did not touch.
func (f *File) ResolveProjects(ctx context.Context, prev *File, finder func(*Project) ProjectFinder) error {
	var errs []error
	for i, p := range f.Projects {
		ref := p.projectRef
		if p.GithubProjectRef == "" || ref.NodeID != "" || p.GithubProject != "" {
			continue
		}
		if prev != nil {
			if old := prev.Project(p.JikuProject); old != nil && old.GithubProject != "" &&
				old.GithubProjectRef == p.GithubProjectRef && old.CredentialName() == p.CredentialName() {
				p.GithubProject = old.GithubProject
				continue
			}
		}
		id, err := finder(p).ProjectNodeID(ctx, ref.Owner, ref.Number)
		if err != nil {
			errs = append(errs, fmt.Errorf("projects[%d].githubProject %s (does it exist, and can the project's credentials see it?): %w", i, ref, err))
			continue
		}
		p.GithubProject = id
	}
	return errors.Join(errs...)
}
