// Package checkconfig compares the mapping with the real GitHub side: that every repo is
// readable, that every field and option the mapping names exists in the Project, which options
// of the Project have no mapping, and which issue types exist. It is what to run when a project is
// mapped and after any change to its board.
package checkconfig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
)

// Level of a finding.
type Level string

// Levels.
const (
	OK    Level = "OK"
	Warn  Level = "AVISO"
	Error Level = "ERROR"
)

// Finding is one line of the report.
type Finding struct {
	Project int64
	Level   Level
	Msg     string
}

// GitHub is what the check reads.
type GitHub interface {
	GetRepo(ctx context.Context, repo string) (*github.Repo, error)
	OrgIssueTypes(ctx context.Context, org string) ([]string, error)
	ProjectSchema(ctx context.Context, projectID string) (*github.ProjectSchema, error)
}

// Run checks every project of the mapping (or only one, when only != 0).
func Run(ctx context.Context, gh GitHub, f *mapping.File, only int64) []Finding {
	var out []Finding
	for _, p := range f.Projects {
		if only != 0 && p.JikuProject != only {
			continue
		}
		out = append(out, project(ctx, gh, p)...)
	}
	return out
}

func project(ctx context.Context, gh GitHub, p *mapping.Project) []Finding {
	var out []Finding
	add := func(l Level, format string, a ...any) {
		out = append(out, Finding{Project: p.JikuProject, Level: l, Msg: fmt.Sprintf(format, a...)})
	}
	orgs := map[string]bool{}
	for _, r := range p.Repos {
		repo, err := gh.GetRepo(ctx, r)
		switch {
		case errors.Is(err, github.ErrNotFound):
			add(Error, "repo %s: no existe o el token no tiene acceso", r)
		case err != nil:
			add(Error, "repo %s: %v", r, err)
		case !repo.HasIssues:
			add(Error, "repo %s: tiene los issues desactivados", r)
		case repo.Visibility == "public":
			add(Warn, "repo %s es PÚBLICO: cualquier usuario de GitHub crea requisitos públicos en Jiku", r)
		default:
			add(OK, "repo %s (%s)", r, repo.Visibility)
		}
		orgs[mapping.Owner(r)] = true
	}

	fm := p.Fields
	if fm != nil && fm.Type != nil {
		for org := range orgs {
			types, err := gh.OrgIssueTypes(ctx, org)
			if err != nil {
				add(Warn, "issue types de %s: no se pudieron leer (%v); el tipo se sincroniza solo por labels", org, err)
				continue
			}
			have := map[string]bool{}
			for _, t := range types {
				have[t] = true
			}
			for _, name := range sortedKeys(fm.Type.IssueType) {
				if !have[name] {
					add(Warn, "issue type %q (type.issueType) no existe en %s", name, org)
				}
			}
			for _, jt := range sortedKeys(fm.Type.ToGithub) {
				if v := fm.Type.ToGithub[jt]; v != nil && !have[*v] {
					add(Error, "type.toGithub[%s] = %q no existe en %s: los issues se crearán sin tipo", jt, *v, org)
				}
			}
			var unmapped []string
			for _, t := range types {
				if _, ok := fm.Type.IssueType[t]; !ok {
					unmapped = append(unmapped, t)
				}
			}
			if len(unmapped) > 0 {
				add(Warn, "issue types de %s sin mapeo (entran como 'otro' salvo que un label los mapee): %v", org, unmapped)
			}
		}
	}

	if p.GithubProject == "" {
		add(Warn, "sin githubProject: solo se sincroniza abierto/cerrado")
		return out
	}
	s, err := gh.ProjectSchema(ctx, p.GithubProject)
	if err != nil {
		add(Error, "Project %s: %v", p.GithubProject, err)
		return out
	}
	add(OK, "Project %q", s.Title)
	if fm == nil {
		add(Warn, "sin fieldMapping: el Project solo recibe los issues, sin estado ni prioridad")
		return out
	}
	if st := fm.Status; st != nil {
		checkSelect(add, s, "status", st.Field, keysOf(st.ToJiku), valuesOf(st.ToGithub))
	}
	if pr := fm.Priority; pr != nil {
		checkSelect(add, s, "priority", pr.Field, keysOfStr(pr.ToJiku), valuesOf(pr.Map))
	}
	return out
}

func checkSelect(add func(Level, string, ...any), s *github.ProjectSchema, part, field string, fromGH, toGH []string) {
	f := s.Fields[field]
	if f == nil {
		add(Error, "%s: el Project no tiene un campo single-select %q", part, field)
		return
	}
	mapped := map[string]bool{}
	for _, o := range fromGH {
		mapped[o] = true
		if _, ok := f.Options[o]; !ok {
			add(Error, "%s: la opción %q no existe en el campo %q", part, o, field)
		}
	}
	for _, o := range toGH {
		mapped[o] = true
		if _, ok := f.Options[o]; !ok {
			add(Error, "%s: el mapeo hacia GitHub usa %q, que no existe en el campo %q", part, o, field)
		}
	}
	var unmapped []string
	for _, o := range f.Order {
		if !mapped[o] {
			unmapped = append(unmapped, o)
		}
	}
	if len(unmapped) > 0 {
		add(Warn, "%s: opciones del Project sin mapeo (mover una tarjeta ahí no cambia Jiku): %v", part, unmapped)
	} else {
		add(OK, "%s: las %d opciones de %q están mapeadas", part, len(f.Order), field)
	}
}

// Render prints the findings; it returns how many errors there were.
func Render(w io.Writer, fs []Finding) int {
	errs := 0
	var last int64 = -1
	for _, f := range fs {
		if f.Project != last {
			fmt.Fprintf(w, "\nProyecto %d\n", f.Project)
			last = f.Project
		}
		fmt.Fprintf(w, "  %-5s %s\n", f.Level, f.Msg)
		if f.Level == Error {
			errs++
		}
	}
	return errs
}

func keysOf(m map[string]mapping.StatusTarget) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysOfStr(m map[string]string) []string { return sortedKeys(m) }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func valuesOf(m map[string]*string) []string {
	var out []string
	for _, k := range sortedKeys(m) {
		if v := m[k]; v != nil && *v != "" {
			out = append(out, *v)
		}
	}
	return out
}
