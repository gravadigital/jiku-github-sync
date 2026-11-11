// Package initial is the one-time initial sync of a project: everything that already exists and
// is open, both ways, with its comments. It first builds a plan (what is created, what is linked,
// what is skipped, how many writes and how long) that can be printed with --dry-run and reviewed
// before applying it.
package initial

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gravadigital/jiku-github-sync/internal/engine"
	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

// GitHub limits content creation to 80 per minute and 500 per hour.
const (
	createsPerMinute = 80
	createsPerHour   = 500
)

// Pair is an existing requirement and an existing issue that are the same thing.
type Pair struct {
	Req    *jiku.Requirement
	Issue  *github.Issue
	Reason string // marker | manual | title
}

// Plan is what the initial sync would do.
type Plan struct {
	Project *mapping.Project
	Link    []Pair
	Export  []*jiku.Requirement
	Import  []*github.Issue
	Skipped []string

	// Comments to copy: Jiku -> GitHub and GitHub -> Jiku.
	CommentsToGitHub int
	CommentsToJiku   int
	// Creations on each side (issues + comments + Project items on GitHub).
	GitHubCreates int
	JikuCreates   int
	Estimate      time.Duration
}

// ManualLink pins a requirement to an issue ("owner/repo#123").
type ManualLink struct {
	Requirement int64  `yaml:"requirement"`
	Issue       string `yaml:"issue"`
}

// LoadLinks reads a manual links file.
func LoadLinks(path string) ([]ManualLink, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator's file
	if err != nil {
		return nil, err
	}
	var ls []ManualLink
	if err := yaml.Unmarshal(b, &ls); err != nil {
		return nil, fmt.Errorf("links file: %w", err)
	}
	return ls, nil
}

// GitHub is what building the plan needs.
type GitHub interface {
	ListIssues(ctx context.Context, repo string, since time.Time, state string) ([]*github.Issue, error)
	ListComments(ctx context.Context, repo string, number int) ([]*github.Comment, error)
}

// Deps are the plan's sources.
type Deps struct {
	GitHub GitHub
	Jiku   jiku.API
	Store  *state.Store
	// WriteInterval is the gap between GitHub writes (SYNC_WRITE_INTERVAL), for the estimate.
	WriteInterval time.Duration
}

func normTitle(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func issueRef(is *github.Issue) string { return fmt.Sprintf("%s#%d", is.Repo, is.Number) }

// Build makes the plan of a project.
func Build(ctx context.Context, d Deps, p *mapping.Project, manual []ManualLink) (*Plan, error) {
	pl := &Plan{Project: p}
	all, err := d.Jiku.ListRequirements(ctx, p.JikuProject, map[string]any{"visibilityLevel": "public"})
	if err != nil {
		return nil, err
	}
	reqs := map[int64]*jiku.Requirement{}
	for _, r := range all {
		if r.State == "resuelto" || r.State == "cancelado" {
			continue
		}
		if _, err := d.Store.Link(ctx, r.ID); err == nil {
			continue
		} else if !errors.Is(err, state.ErrNotFound) {
			return nil, err
		}
		reqs[r.ID] = r
	}
	issues := map[string]*github.Issue{}
	for _, repo := range p.Repos {
		list, err := d.GitHub.ListIssues(ctx, repo, time.Time{}, "open")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repo, err)
		}
		for _, is := range list {
			if is.IsPR {
				continue
			}
			if _, err := d.Store.LinkByIssue(ctx, is.NodeID); err == nil {
				continue
			} else if !errors.Is(err, state.ErrNotFound) {
				return nil, err
			}
			issues[strings.ToLower(issueRef(is))] = is
		}
	}

	pair := func(r *jiku.Requirement, is *github.Issue, why string) {
		pl.Link = append(pl.Link, Pair{Req: r, Issue: is, Reason: why})
		delete(reqs, r.ID)
		delete(issues, strings.ToLower(issueRef(is)))
	}
	// 1. Issues the connector created earlier.
	for _, is := range sortedIssues(issues) {
		if id, ok := text.RequirementID(is.Body); ok {
			if r := reqs[id]; r != nil {
				pair(r, is, "marker")
			} else {
				delete(issues, strings.ToLower(issueRef(is)))
				pl.Skipped = append(pl.Skipped, fmt.Sprintf("%s: creado por el conector para el requisito %d, que no está entre los abiertos y públicos", issueRef(is), id))
			}
		}
	}
	// 2. Manual links.
	for _, m := range manual {
		r := reqs[m.Requirement]
		is := issues[strings.ToLower(strings.TrimSpace(m.Issue))]
		if r == nil || is == nil {
			pl.Skipped = append(pl.Skipped, fmt.Sprintf("vínculo manual %d ↔ %s: alguno de los dos no está pendiente (ya vinculado, cerrado, interno o inexistente)", m.Requirement, m.Issue))
			continue
		}
		pair(r, is, "manual")
	}
	// 3. Same title, when it is unique on both sides.
	byTitleR := map[string][]*jiku.Requirement{}
	for _, r := range reqs {
		byTitleR[normTitle(r.Title)] = append(byTitleR[normTitle(r.Title)], r)
	}
	byTitleI := map[string][]*github.Issue{}
	for _, is := range issues {
		byTitleI[normTitle(is.Title)] = append(byTitleI[normTitle(is.Title)], is)
	}
	for t, rs := range byTitleR {
		is := byTitleI[t]
		if len(rs) == 1 && len(is) == 1 {
			pair(rs[0], is[0], "title")
		} else if len(is) > 0 {
			pl.Skipped = append(pl.Skipped, fmt.Sprintf("título repetido %q (%d requisitos, %d issues): se crean por separado; usá un vínculo manual si son lo mismo", rs[0].Title, len(rs), len(is)))
		}
	}
	for _, r := range reqs {
		pl.Export = append(pl.Export, r)
	}
	pl.Import = sortedIssues(issues)
	sort.Slice(pl.Export, func(a, b int) bool { return pl.Export[a].ID < pl.Export[b].ID })
	sort.Slice(pl.Link, func(a, b int) bool { return pl.Link[a].Req.ID < pl.Link[b].Req.ID })

	// Comments to copy, for the estimate.
	self := d.Jiku.SelfID()
	countJ := func(id int64) error {
		cs, err := d.Jiku.ListComments(ctx, id)
		for _, c := range cs {
			if c.AuthorID != self && c.VisibilityLevel == "public" {
				pl.CommentsToGitHub++
			}
		}
		return err
	}
	countG := func(is *github.Issue) error {
		cs, err := d.GitHub.ListComments(ctx, is.Repo, is.Number)
		for _, c := range cs {
			if !text.HasMarker(c.Body) {
				pl.CommentsToJiku++
			}
		}
		return err
	}
	for _, r := range pl.Export {
		if err := countJ(r.ID); err != nil {
			return nil, err
		}
	}
	for _, is := range pl.Import {
		if err := countG(is); err != nil {
			return nil, err
		}
	}
	for _, pr := range pl.Link {
		if err := countJ(pr.Req.ID); err != nil {
			return nil, err
		}
		if err := countG(pr.Issue); err != nil {
			return nil, err
		}
	}
	items := 0
	if p.GithubProject != "" {
		items = len(pl.Export) + len(pl.Import) + len(pl.Link)
	}
	pl.GitHubCreates = len(pl.Export) + pl.CommentsToGitHub + items
	pl.JikuCreates = len(pl.Import) + pl.CommentsToJiku
	pl.Estimate = estimate(pl.GitHubCreates, d.WriteInterval)
	return pl, nil
}

func estimate(creates int, interval time.Duration) time.Duration {
	if creates == 0 {
		return 0
	}
	byInterval := time.Duration(creates) * interval
	byMinute := time.Duration(math.Ceil(float64(creates)/createsPerMinute)-1) * time.Minute
	byHour := time.Duration(math.Ceil(float64(creates)/createsPerHour)-1) * time.Hour
	return max(byInterval, byMinute, byHour)
}

func sortedIssues(m map[string]*github.Issue) []*github.Issue {
	out := make([]*github.Issue, 0, len(m))
	for _, is := range m {
		out = append(out, is)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Repo != out[b].Repo {
			return out[a].Repo < out[b].Repo
		}
		return out[a].Number < out[b].Number
	})
	return out
}

// Render prints the plan for a person to review.
func (pl *Plan) Render(w io.Writer) {
	p := pl.Project
	fmt.Fprintf(w, "Sincronización inicial del proyecto %d (repos: %s)\n\n", p.JikuProject, strings.Join(p.Repos, ", "))
	fmt.Fprintf(w, "Se vinculan (ya existen en los dos lados): %d\n", len(pl.Link))
	for _, x := range pl.Link {
		fmt.Fprintf(w, "  - requisito %d ↔ %s (%s) %q\n", x.Req.ID, issueRef(x.Issue), x.Reason, x.Req.Title)
	}
	fmt.Fprintf(w, "\nRequisitos que crean su issue en %s: %d\n", p.DefaultRepo, len(pl.Export))
	for _, r := range pl.Export {
		fmt.Fprintf(w, "  - requisito %d [%s] %q\n", r.ID, r.State, r.Title)
	}
	fmt.Fprintf(w, "\nIssues que crean su requisito: %d\n", len(pl.Import))
	for _, is := range pl.Import {
		fmt.Fprintf(w, "  - %s %q\n", issueRef(is), is.Title)
	}
	if len(pl.Skipped) > 0 {
		fmt.Fprintf(w, "\nSe saltean: %d\n", len(pl.Skipped))
		for _, s := range pl.Skipped {
			fmt.Fprintf(w, "  - %s\n", s)
		}
	}
	fmt.Fprintf(w, "\nComentarios a copiar: %d de Jiku a GitHub, %d de GitHub a Jiku\n", pl.CommentsToGitHub, pl.CommentsToJiku)
	fmt.Fprintf(w, "Escrituras: %d creaciones en GitHub, %d en Jiku\n", pl.GitHubCreates, pl.JikuCreates)
	fmt.Fprintf(w, "Tiempo estimado: %s (GitHub: %d creaciones/min, %d/hora)\n", pl.Estimate.Round(time.Second), createsPerMinute, createsPerHour)
	if pl.CommentsToJiku > 0 {
		fmt.Fprintf(w, "\nAtención: cada comentario copiado a Jiku manda mails a los suscriptores del requisito (%d comentarios).\n", pl.CommentsToJiku)
	}
}

// Apply runs the plan. It continues past individual failures and reports them all; running it
// again is safe (what is already linked is not in a new plan).
func Apply(ctx context.Context, e *engine.Engine, st *state.Store, pl *Plan, progress io.Writer) error {
	p := pl.Project
	if _, err := e.EnsureStarted(ctx, p.JikuProject); err != nil {
		return err
	}
	opts := engine.SyncOptions{WithDates: true}
	var errs []error
	total := len(pl.Link) + len(pl.Export) + len(pl.Import)
	done := 0
	step := func(what string, err error) {
		done++
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
			fmt.Fprintf(progress, "[%d/%d] ERROR %s: %v\n", done, total, what, err)
			return
		}
		fmt.Fprintf(progress, "[%d/%d] ok %s\n", done, total, what)
	}
	for _, x := range pl.Link {
		// No snapshot: on every field the most recent side wins.
		l := &state.Link{ReqID: x.Req.ID, JikuProject: p.JikuProject, IssueNodeID: x.Issue.NodeID, Repo: x.Issue.Repo, Number: x.Issue.Number}
		err := st.SaveLink(ctx, l)
		if err == nil {
			err = e.SyncLink(ctx, l, opts)
		}
		step(fmt.Sprintf("vincular %d ↔ %s", x.Req.ID, issueRef(x.Issue)), err)
	}
	for _, r := range pl.Export {
		_, err := e.ExportRequirement(ctx, p, r, opts)
		step("exportar requisito "+strconv.FormatInt(r.ID, 10), err)
	}
	for _, is := range pl.Import {
		_, err := e.ImportIssue(ctx, p, is, opts)
		step("importar "+issueRef(is), err)
	}
	if err := st.UpdateProject(ctx, p.JikuProject, func(ps *state.ProjectState) { ps.InitialSyncAt = time.Now().UTC() }); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
