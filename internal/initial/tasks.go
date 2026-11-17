package initial

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/engine"
	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

// The tasks' initial sync: once tasks are turned on for a project, what already exists under each
// synced requirement — its open public tasks and the open sub-issues of its issue — is linked
// (same title, unique under that requirement) or created on the other side. It runs after the
// requirements' initial sync, since it needs their pairs.

// TaskItem is one line of the tasks' plan: a task, a sub-issue or both, under a synced
// requirement.
type TaskItem struct {
	Req   *state.Link
	Task  *jiku.Task
	Issue *github.Issue
	// Reason is "title" or "marker" for a pair.
	Reason string
}

// TaskPlan is what the tasks' initial sync would do.
type TaskPlan struct {
	Project *mapping.Project
	Link    []TaskItem
	Export  []TaskItem
	Import  []TaskItem
	Skipped []string

	CommentsToGitHub int
	CommentsToJiku   int
	GitHubCreates    int
	JikuCreates      int
	Estimate         time.Duration
}

// SubIssueLister is what the tasks' plan needs from GitHub on top of GitHub.
type SubIssueLister interface {
	ListSubIssues(ctx context.Context, repo string, number int) ([]*github.Issue, error)
}

func openTask(t *jiku.Task) bool { return t.State != "finalizado" && t.State != "cancelado" }

// BuildTasks makes the tasks' plan of a project.
func BuildTasks(ctx context.Context, d Deps, subs SubIssueLister, p *mapping.Project) (*TaskPlan, error) {
	pl := &TaskPlan{Project: p}
	ps, err := d.Store.Project(ctx, p.JikuProject)
	if err != nil {
		return nil, err
	}
	all, err := d.Jiku.ListTasks(ctx, p.JikuProject, map[string]any{"visibilityLevel": "public"})
	if err != nil {
		return nil, err
	}
	byReq := map[int64][]*jiku.Task{}
	for _, t := range all {
		if t.Req() == 0 || !openTask(t) {
			continue
		}
		if _, err := d.Store.TaskLink(ctx, t.ID); err == nil {
			continue
		} else if !errors.Is(err, state.ErrNotFound) {
			return nil, err
		}
		byReq[t.Req()] = append(byReq[t.Req()], t)
	}
	reqIDs := append([]int64(nil), ps.Links...)
	sort.Slice(reqIDs, func(a, b int) bool { return reqIDs[a] < reqIDs[b] })
	for _, id := range reqIDs {
		rl, err := d.Store.Link(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("link %d: %w", id, err)
		}
		if rl.Unlinked {
			continue
		}
		if err := planUnder(ctx, d, subs, pl, rl, byReq[rl.ReqID]); err != nil {
			return nil, fmt.Errorf("requirement %d (%s): %w", rl.ReqID, rl.IssueKey(), err)
		}
	}
	for _, x := range pl.Export {
		if err := countTaskComments(ctx, d, pl, x.Task, nil); err != nil {
			return nil, err
		}
	}
	for _, x := range pl.Import {
		if err := countTaskComments(ctx, d, pl, nil, x.Issue); err != nil {
			return nil, err
		}
	}
	for _, x := range pl.Link {
		if err := countTaskComments(ctx, d, pl, x.Task, x.Issue); err != nil {
			return nil, err
		}
	}
	items := 0
	if p.GithubProject != "" && p.TaskStatus() != nil {
		items = len(pl.Export) + len(pl.Import) + len(pl.Link)
	}
	// Each sub-issue created is two writes: the issue, and putting it under its parent.
	pl.GitHubCreates = 2*len(pl.Export) + pl.CommentsToGitHub + items
	pl.JikuCreates = len(pl.Import) + pl.CommentsToJiku
	settle := time.Duration(0)
	if items > 0 {
		settle = time.Duration(len(pl.Export)) * d.ItemSettle
	}
	pl.Estimate = estimate(pl.GitHubCreates, d.WriteInterval) + settle
	return pl, nil
}

// planUnder plans one synced requirement: its open public tasks and its issue's open sub-issues.
func planUnder(ctx context.Context, d Deps, subs SubIssueLister, pl *TaskPlan, rl *state.Link, tasks []*jiku.Task) error {
	list, err := subs.ListSubIssues(ctx, rl.Repo, rl.Number)
	if err != nil {
		return err
	}
	issues := map[string]*github.Issue{}
	for _, is := range list {
		if !is.Open() || is.IsPR {
			continue
		}
		if !pl.Project.HasRepo(is.Repo) {
			pl.Skipped = append(pl.Skipped, fmt.Sprintf("%s: sub-issue de %s en un repo que el proyecto no mapea", issueRef(is), rl.IssueKey()))
			continue
		}
		if _, err := d.Store.TaskLinkByIssue(ctx, is.NodeID); err == nil {
			continue
		} else if !errors.Is(err, state.ErrNotFound) {
			return err
		}
		if _, err := d.Store.LinkByIssue(ctx, is.NodeID); err == nil {
			pl.Skipped = append(pl.Skipped, fmt.Sprintf("%s: sub-issue de %s, pero ya está vinculado como requisito (sigue siéndolo)", issueRef(is), rl.IssueKey()))
			continue
		} else if !errors.Is(err, state.ErrNotFound) {
			return err
		}
		issues[strings.ToLower(issueRef(is))] = is
	}
	byID := map[int64]*jiku.Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	pair := func(t *jiku.Task, is *github.Issue, why string) {
		pl.Link = append(pl.Link, TaskItem{Req: rl, Task: t, Issue: is, Reason: why})
		delete(byID, t.ID)
		delete(issues, strings.ToLower(issueRef(is)))
	}
	for _, is := range sortedIssues(issues) {
		if id, ok := text.TaskID(is.Body); ok {
			if t := byID[id]; t != nil {
				pair(t, is, "marker")
			} else {
				delete(issues, strings.ToLower(issueRef(is)))
				pl.Skipped = append(pl.Skipped, fmt.Sprintf("%s: creado por el conector para la tarea %d, que no está entre las abiertas y públicas de %s", issueRef(is), id, rl.IssueKey()))
			}
		}
	}
	byTitleT := map[string][]*jiku.Task{}
	for _, t := range byID {
		byTitleT[normTitle(t.Title)] = append(byTitleT[normTitle(t.Title)], t)
	}
	byTitleI := map[string][]*github.Issue{}
	for _, is := range issues {
		byTitleI[normTitle(is.Title)] = append(byTitleI[normTitle(is.Title)], is)
	}
	for title, ts := range byTitleT {
		is := byTitleI[title]
		if len(ts) == 1 && len(is) == 1 {
			pair(ts[0], is[0], "title")
		} else if len(is) > 0 {
			pl.Skipped = append(pl.Skipped, fmt.Sprintf("título repetido %q bajo %s (%d tareas, %d sub-issues): se crean por separado", ts[0].Title, rl.IssueKey(), len(ts), len(is)))
		}
	}
	rest := make([]*jiku.Task, 0, len(byID))
	for _, t := range byID {
		rest = append(rest, t)
	}
	sort.Slice(rest, func(a, b int) bool { return rest[a].ID < rest[b].ID })
	for _, t := range rest {
		pl.Export = append(pl.Export, TaskItem{Req: rl, Task: t})
	}
	for _, is := range sortedIssues(issues) {
		pl.Import = append(pl.Import, TaskItem{Req: rl, Issue: is})
	}
	return nil
}

func countTaskComments(ctx context.Context, d Deps, pl *TaskPlan, t *jiku.Task, is *github.Issue) error {
	if t != nil {
		cs, err := d.Jiku.ListComments(ctx, jiku.TaskRef(t.ID))
		if err != nil {
			return err
		}
		self := d.Jiku.SelfID()
		for _, c := range cs {
			if c.AuthorID != self && c.VisibilityLevel == "public" {
				pl.CommentsToGitHub++
			}
		}
	}
	if is != nil {
		cs, err := d.GitHub.ListComments(ctx, is.Repo, is.Number)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if !text.HasMarker(c.Body) {
				pl.CommentsToJiku++
			}
		}
	}
	return nil
}

// Render prints the tasks' plan for a person to review.
func (pl *TaskPlan) Render(w io.Writer) {
	fmt.Fprintf(w, "Sincronización inicial de las tareas del proyecto %d\n\n", pl.Project.JikuProject)
	fmt.Fprintf(w, "Se vinculan (ya existen en los dos lados): %d\n", len(pl.Link))
	for _, x := range pl.Link {
		fmt.Fprintf(w, "  - tarea %d ↔ %s bajo %s (%s) %q\n", x.Task.ID, issueRef(x.Issue), x.Req.IssueKey(), x.Reason, x.Task.Title)
	}
	fmt.Fprintf(w, "\nTareas que crean su sub-issue: %d\n", len(pl.Export))
	for _, x := range pl.Export {
		fmt.Fprintf(w, "  - tarea %d [%s] bajo %s %q\n", x.Task.ID, x.Task.State, x.Req.IssueKey(), x.Task.Title)
	}
	fmt.Fprintf(w, "\nSub-issues que crean su tarea: %d\n", len(pl.Import))
	for _, x := range pl.Import {
		fmt.Fprintf(w, "  - %s (requisito %d) %q\n", issueRef(x.Issue), x.Req.ReqID, x.Issue.Title)
	}
	if len(pl.Skipped) > 0 {
		fmt.Fprintf(w, "\nSe saltean: %d\n", len(pl.Skipped))
		for _, s := range pl.Skipped {
			fmt.Fprintf(w, "  - %s\n", s)
		}
	}
	fmt.Fprintf(w, "\nComentarios a copiar: %d de Jiku a GitHub, %d de GitHub a Jiku\n", pl.CommentsToGitHub, pl.CommentsToJiku)
	fmt.Fprintf(w, "Escrituras: %d en GitHub, %d creaciones en Jiku\n", pl.GitHubCreates, pl.JikuCreates)
	fmt.Fprintf(w, "Tiempo estimado: %s\n", pl.Estimate.Round(time.Second))
}

// ApplyTasks runs the tasks' plan. Like Apply, it continues past failures and is safe to run
// again; when everything went well it is recorded and never repeated.
func ApplyTasks(ctx context.Context, e *engine.Engine, jk jiku.API, st *state.Store, pl *TaskPlan, progress io.Writer) error {
	p := pl.Project
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
		l := &state.Link{TaskID: x.Task.ID, ReqID: x.Req.ReqID, ParentNode: x.Req.IssueNodeID, JikuProject: p.JikuProject,
			IssueNodeID: x.Issue.NodeID, Repo: x.Issue.Repo, Number: x.Issue.Number}
		err := st.SaveTaskLink(ctx, l)
		if err == nil {
			err = e.SyncLink(ctx, l, opts)
		}
		step(fmt.Sprintf("vincular tarea %d ↔ %s", x.Task.ID, issueRef(x.Issue)), err)
	}
	for _, x := range pl.Export {
		t, err := jk.GetTask(ctx, x.Task.ID) // with its files
		if err == nil {
			_, err = e.ExportTask(ctx, p, t, x.Req, opts)
		}
		step(fmt.Sprintf("exportar tarea %d", x.Task.ID), err)
	}
	for _, x := range pl.Import {
		_, err := e.ImportSubIssue(ctx, p, x.Issue, x.Req, opts)
		step("importar "+issueRef(x.Issue), err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return st.UpdateProject(ctx, p.JikuProject, func(ps *state.ProjectState) { ps.TaskInitialSyncAt = time.Now().UTC() })
}
