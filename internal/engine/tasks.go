package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

// Tasks: a public task of a synced requirement is a sub-issue of the requirement's issue, and a
// new sub-issue of a synced issue is a public task of its requirement. A task's fields, state and
// comments are synced like a requirement's; on top, the task's requirement and the sub-issue's
// parent are kept the same (the latest change wins). Tasks without a requirement, internal ones
// and those of a requirement that is not synced never leave Jiku; sub-issues of sub-issues are
// ignored (Jiku has no subtasks).

// Who unlinked a task's pair; only that side revives it.
const (
	unlinkedByJiku   = "jiku"
	unlinkedByGitHub = "github"
)

func (e *Engine) logTask(l *state.Link) *slog.Logger {
	return e.log.With("task", l.TaskID, "requirement", l.ReqID, "issue", l.IssueKey())
}

// EnsureTasksStarted records when the connector started syncing a project's tasks (only the
// first time).
func (e *Engine) EnsureTasksStarted(ctx context.Context, p int64) (time.Time, error) {
	var started time.Time
	err := e.st.UpdateProject(ctx, p, func(ps *state.ProjectState) {
		if ps.TasksStartedAt.IsZero() {
			ps.TasksStartedAt = e.now().UTC()
		}
		started = ps.TasksStartedAt
	})
	return started, err
}

// liveReqLink returns the pair of a requirement of the project when it is synced (linked and not
// unlinked), nil otherwise.
func (e *Engine) liveReqLink(ctx context.Context, p *mapping.Project, reqID int64) (*state.Link, error) {
	if reqID == 0 {
		return nil, nil
	}
	l, err := e.st.Link(ctx, reqID)
	if errors.Is(err, state.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if l.Unlinked || l.JikuProject != p.JikuProject {
		return nil, nil
	}
	return l, nil
}

// linkOfNode returns the pair (requirement or task) of an issue by node id, nil without one.
func (e *Engine) linkOfNode(ctx context.Context, nodeID string) (*state.Link, error) {
	l, err := e.st.LinkByIssue(ctx, nodeID)
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, state.ErrNotFound) {
		return nil, err
	}
	l, err = e.st.TaskLinkByIssue(ctx, nodeID)
	if errors.Is(err, state.ErrNotFound) {
		return nil, nil
	}
	return l, err
}

// linkOfIssue returns the pair (requirement or task) of repo#number, nil without one.
func (e *Engine) linkOfIssue(ctx context.Context, p *mapping.Project, repo string, number int) (*state.Link, error) {
	is, err := e.gh(p).GetIssue(ctx, repo, number)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e.linkOfNode(ctx, is.NodeID)
}

// parentOf reads a sub-issue's parent: its node id ("" without parent) and its pair (nil when it
// has none). hint is the requirement pair the task was under, which saves reading the parent when
// it did not change.
func (e *Engine) parentOf(ctx context.Context, p *mapping.Project, is *github.Issue, hint *state.Link) (string, *state.Link, error) {
	if !is.HasParent() {
		return "", nil, nil
	}
	if hint != nil && strings.EqualFold(hint.Repo, is.ParentRepo) && hint.Number == is.ParentNumber {
		return hint.IssueNodeID, hint, nil
	}
	parent, err := e.gh(p).GetIssue(ctx, is.ParentRepo, is.ParentNumber)
	if errors.Is(err, github.ErrNotFound) {
		return is.ParentKey(), nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	l, err := e.linkOfNode(ctx, parent.NodeID)
	return parent.NodeID, l, err
}

// syncedReq returns l when it is a synced requirement pair of the project, nil otherwise.
func syncedReq(p *mapping.Project, l *state.Link) *state.Link {
	if l == nil || l.IsTask() || l.Unlinked || l.JikuProject != p.JikuProject {
		return nil
	}
	return l
}

// HandleTask syncs one task: its pair if it has one, or the creation of its sub-issue when it
// qualifies (public, of a synced requirement, born after tasks started being synced, or forced
// because it just became public or moved to another requirement).
func (e *Engine) HandleTask(ctx context.Context, taskID int64, forced bool) error {
	l, err := e.st.TaskLink(ctx, taskID)
	switch {
	case err == nil:
		return e.SyncLink(ctx, l, SyncOptions{})
	case !errors.Is(err, state.ErrNotFound):
		return err
	}
	t, err := e.jk.GetTask(ctx, taskID)
	if errors.Is(err, jiku.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	p := e.Config().Project(t.ProjectID)
	if p == nil || !p.TasksEnabled() || !t.Public() || t.Req() == 0 {
		return nil
	}
	rl, err := e.liveReqLink(ctx, p, t.Req())
	if err != nil || rl == nil {
		return err // its requirement is not synced: when it is, its tasks follow
	}
	ps, err := e.st.Project(ctx, p.JikuProject)
	if err != nil {
		return err
	}
	if !e.taskQualifies(ps, t, forced) {
		return nil
	}
	_, err = e.ExportTask(ctx, p, t, rl, SyncOptions{})
	return err
}

// taskQualifies says whether a public task of a synced requirement, with no pair, gets its
// sub-issue now. Before the tasks' initial sync, what existed before tasks were turned on is its
// job; after it, an open one is exported however old (it became public, or its requirement was
// synced, while the connector was not looking), and a closed old one stays out.
func (e *Engine) taskQualifies(ps *state.ProjectState, t *jiku.Task, forced bool) bool {
	if ps.TasksStartedAt.IsZero() || !t.CreatedAt.Before(ps.TasksStartedAt) {
		return true
	}
	open := t.State != "finalizado" && t.State != "cancelado"
	return open && (forced || !ps.TaskInitialSyncAt.IsZero())
}

// followRequirement brings a requirement's tasks along with it: the pairs of its tasks are synced
// (they unlink when the requirement stops being synced) and, when it is synced, its public tasks
// without a pair get their sub-issues.
func (e *Engine) followRequirement(ctx context.Context, p *mapping.Project, rl *state.Link) error {
	if !p.TasksEnabled() {
		return nil
	}
	ps, err := e.st.Project(ctx, p.JikuProject)
	if err != nil {
		return err
	}
	var errs []error
	linked := map[int64]bool{}
	for _, id := range ps.TaskLinks {
		tl, err := e.st.TaskLink(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		linked[id] = true
		if tl.ReqID == rl.ReqID {
			errs = append(errs, e.SyncLink(ctx, tl, SyncOptions{}))
		}
	}
	if rl.Unlinked {
		return errors.Join(errs...)
	}
	tasks, err := e.jk.ListTasks(ctx, p.JikuProject, map[string]any{"requirementId": rl.ReqID, "visibilityLevel": "public"})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, t := range tasks {
		if linked[t.ID] || !e.taskQualifies(ps, t, true) {
			continue
		}
		full, err := e.jk.GetTask(ctx, t.ID) // with its files
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := e.ExportTask(ctx, p, full, rl, SyncOptions{}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// syncTask syncs a task's pair: whether it is still synced, where it lives (its requirement and
// the sub-issue's parent), then its fields and comments like any pair.
func (e *Engine) syncTask(ctx context.Context, l *state.Link, opts SyncOptions) error {
	log := e.logTask(l)
	p := e.Config().Project(l.JikuProject)
	if p == nil || !p.TasksEnabled() {
		log.Debug("tasks of the project not synced; skipping")
		return nil
	}
	t, err := e.jk.GetTask(ctx, l.TaskID)
	if errors.Is(err, jiku.ErrNotFound) {
		log.Warn("task not found; skipping")
		return nil
	}
	if err != nil {
		return err
	}
	is, err := e.fetchIssue(ctx, p, l)
	if errors.Is(err, github.ErrNotFound) {
		log.Warn("sub-issue not found (deleted?); skipping")
		return nil
	}
	if err != nil {
		return err
	}
	prev, err := e.st.Link(ctx, l.ReqID)
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return err
	}
	jReq, err := e.liveReqLink(ctx, p, t.Req())
	if err != nil {
		return err
	}
	gParentNode, parent, err := e.parentOf(ctx, p, is, prev)
	if err != nil {
		return err
	}
	gReq := syncedReq(p, parent)

	if l.Unlinked {
		switch {
		case l.UnlinkedBy == unlinkedByGitHub && gReq != nil && t.Public():
			// The sub-issue was put under a synced issue again: the task follows it.
			if t.Req() != gReq.ReqID {
				if err := e.jk.EditTask(ctx, t.ID, map[string]any{"requirementId": gReq.ReqID}); err != nil {
					return fmt.Errorf("moving task %d to requirement %d: %w", t.ID, gReq.ReqID, err)
				}
				log.Info("task moved to the requirement of the sub-issue's new parent", "to", gReq.ReqID)
				t.RequirementID = &gReq.ReqID
			}
			e.revive(l, is, gReq)
		case l.UnlinkedBy != unlinkedByGitHub && jReq != nil && t.Public():
			if gReq == nil || gReq.ReqID != jReq.ReqID {
				if err := e.gh(p).AddSubIssue(ctx, jReq.Repo, jReq.Number, is.ID, true); err != nil {
					return fmt.Errorf("putting %s under %s: %w", l.IssueKey(), jReq.IssueKey(), err)
				}
			}
			e.revive(l, is, jReq)
		default:
			return nil
		}
		log.Info("task pair revived")
	} else {
		if !t.Public() {
			return e.unlinkTask(ctx, p, l, is, unlinkedByJiku, "Este sub-issue dejó de sincronizarse con Jiku: la tarea pasó a ser interna.", log)
		}
		done, err := e.syncParent(ctx, p, l, t, is, jReq, gReq, gParentNode, log)
		if err != nil || done {
			return err
		}
	}
	return e.syncFields(ctx, p, e.model(p, true), l, taskSide(t), is, opts, log)
}

// revive starts syncing an unlinked task pair again, under rl. Jiku wins every field.
func (e *Engine) revive(l *state.Link, is *github.Issue, rl *state.Link) {
	l.Unlinked, l.UnlinkedBy = false, ""
	l.ReqID, l.ParentNode = rl.ReqID, rl.IssueNodeID
	l.Last = state.Snapshot{Title: is.Title, GHOpen: is.Open(), GHStateReason: is.StateReason}
	l.Last.Bodies("", text.Hash(is.Body))
}

// syncParent keeps the task's requirement and the sub-issue's parent the same. done is true when
// the pair was unlinked.
func (e *Engine) syncParent(ctx context.Context, p *mapping.Project, l *state.Link, t *jiku.Task, is *github.Issue, jReq, gReq *state.Link, gParentNode string, log *slog.Logger) (bool, error) {
	jChanged := t.Req() != l.ReqID
	gChanged := gParentNode != l.ParentNode
	same := jReq != nil && gReq != nil && jReq.ReqID == gReq.ReqID
	var gAt time.Time
	if gChanged && !jChanged {
		gAt = e.now()
	} else if gChanged {
		// Both moved: the timeline says when the parent changed (it lags a few seconds).
		pc, err := e.gh(p).LastParentChange(ctx, is.NodeID)
		if err != nil {
			log.Warn("cannot read when the parent changed", "error", err)
		} else if pc != nil {
			gAt = pc.CreatedAt
		}
	}
	switch decide(jChanged, gChanged, !same, t.UpdatedAt, gAt) {
	case sideJiku:
		if jReq == nil {
			// The task is no longer under a synced requirement: it was moved to one that is not
			// synced (or to none), or its requirement stopped syncing. Only in the first case is
			// the sub-issue taken out of its parent: it no longer belongs there.
			if jChanged && is.HasParent() {
				if err := e.gh(p).RemoveSubIssue(ctx, is.ParentRepo, is.ParentNumber, is.ID); err != nil && !errors.Is(err, github.ErrNotFound) {
					return false, fmt.Errorf("detaching %s: %w", l.IssueKey(), err)
				}
				log.Info("sub-issue detached from its parent: the task left its requirement")
			}
			return true, e.unlinkTask(ctx, p, l, is, unlinkedByJiku,
				"Este sub-issue dejó de sincronizarse con Jiku: la tarea ya no pertenece a un requisito sincronizado.", log)
		}
		if err := e.gh(p).AddSubIssue(ctx, jReq.Repo, jReq.Number, is.ID, is.HasParent()); err != nil {
			return false, fmt.Errorf("putting %s under %s: %w", l.IssueKey(), jReq.IssueKey(), err)
		}
		log.Info("sub-issue moved to the issue of the task's requirement", "parent", jReq.IssueKey())
		l.ReqID, l.ParentNode = jReq.ReqID, jReq.IssueNodeID
	case sideGitHub:
		if gReq == nil {
			// Decision: a sub-issue detached on GitHub (or moved under an issue that is not a
			// synced requirement) stops syncing; the task stays where it is in Jiku.
			msg := fmt.Sprintf("La tarea dejó de sincronizarse con GitHub: %s ya no es sub-issue de un issue sincronizado.", is.HTMLURL)
			if _, err := e.jk.CreateComment(ctx, jiku.TaskRef(t.ID), msg, "internal", nil); err != nil {
				log.Warn("could not leave an internal comment", "error", err)
			}
			l.Unlinked, l.UnlinkedBy = true, unlinkedByGitHub
			l.ParentNode = gParentNode
			log.Info("sub-issue detached on github: task pair unlinked")
			return true, e.st.SaveTaskLink(ctx, l)
		}
		if err := e.jk.EditTask(ctx, t.ID, map[string]any{"requirementId": gReq.ReqID}); err != nil {
			return false, fmt.Errorf("moving task %d to requirement %d: %w", t.ID, gReq.ReqID, err)
		}
		log.Info("task moved to the requirement of the sub-issue's new parent", "to", gReq.ReqID)
		t.RequirementID = &gReq.ReqID
		l.ReqID, l.ParentNode = gReq.ReqID, gReq.IssueNodeID
	default:
		if same {
			l.ReqID, l.ParentNode = jReq.ReqID, jReq.IssueNodeID
		}
	}
	return false, nil
}

// unlinkTask stops syncing a task's pair, leaving a notice on the sub-issue.
func (e *Engine) unlinkTask(ctx context.Context, p *mapping.Project, l *state.Link, is *github.Issue, by, notice string, log *slog.Logger) error {
	if _, err := e.gh(p).CreateComment(ctx, is.Repo, is.Number, text.Notice(notice)); err != nil {
		return err
	}
	l.Unlinked, l.UnlinkedBy = true, by
	log.Info("task pair unlinked", "by", by)
	return e.st.SaveTaskLink(ctx, l)
}

// ExportTask creates the sub-issue of a task under its requirement's issue, in the same repo, and
// links them. Safe to retry: a creation interrupted before its link was stored is found again by
// its marker.
func (e *Engine) ExportTask(ctx context.Context, p *mapping.Project, t *jiku.Task, rl *state.Link, opts SyncOptions) (*state.Link, error) {
	log := e.log.With("task", t.ID, "requirement", rl.ReqID, "repo", rl.Repo)
	if pend, err := e.st.PendingTask(ctx, t.ID); err == nil {
		is, err := e.findCreatedSubIssue(ctx, p, rl.Repo, t.ID, pend)
		if err != nil {
			return nil, err
		}
		if is != nil {
			log.Info("recovering an interrupted export", "issue", is.Number)
			return e.linkExportedTask(ctx, p, t, rl, is, nil, is.HasParent(), opts)
		}
	} else if !errors.Is(err, state.ErrNotFound) {
		return nil, err
	}

	tmp := &state.Link{TaskID: t.ID, ReqID: rl.ReqID, Repo: rl.Repo}
	body, err := e.toGitHub(ctx, p, tmp, rl.Repo, t.Desc(), t.Attachments, false)
	if err != nil {
		return nil, err
	}
	in := github.IssueInput{Title: strp(t.Title), Body: strp(text.SubIssueBody(body, t.ID))}
	if p.Tasks != nil && p.Tasks.IssueType != "" {
		in.Type = strp(p.Tasks.IssueType)
	}
	if err := e.st.SetPendingTask(ctx, t.ID, state.Pending{At: e.now().UTC(), Title: t.Title}); err != nil {
		return nil, err
	}
	is, err := e.gh(p).CreateIssue(ctx, rl.Repo, in)
	if err != nil && in.Type != nil {
		log.Warn("creating the sub-issue with its type failed; retrying without type", "type", *in.Type, "error", err)
		in.Type = nil
		is, err = e.gh(p).CreateIssue(ctx, rl.Repo, in)
	}
	if err != nil {
		return nil, fmt.Errorf("creating the sub-issue of task %d: %w", t.ID, err)
	}
	attached := true
	if err := e.gh(p).AddSubIssue(ctx, rl.Repo, rl.Number, is.ID, false); err != nil {
		// The pair keeps no parent: its next sync puts it under the requirement's issue.
		log.Warn("the sub-issue was created but could not be put under its parent yet", "issue", is.Number, "error", err)
		attached = false
	}
	log.Info("sub-issue created from task", "issue", is.Number, "parent", rl.IssueKey())
	return e.linkExportedTask(ctx, p, t, rl, is, tmp.Assets, attached, opts)
}

func (e *Engine) linkExportedTask(ctx context.Context, p *mapping.Project, t *jiku.Task, rl *state.Link, is *github.Issue, assets []state.Asset, attached bool, opts SyncOptions) (*state.Link, error) {
	l := &state.Link{
		TaskID: t.ID, ReqID: rl.ReqID, JikuProject: p.JikuProject,
		IssueNodeID: is.NodeID, Repo: is.Repo, Number: is.Number,
		Last:   state.Snapshot{Title: t.Title, GHOpen: is.Open(), GHStateReason: is.StateReason},
		Assets: assets,
	}
	if attached {
		l.ParentNode = rl.IssueNodeID
	}
	l.Last.Bodies(text.Hash(t.Desc()), text.Hash(is.Body))
	if err := e.st.SaveTaskLink(ctx, l); err != nil {
		return nil, err
	}
	_ = e.st.ClearPendingTask(ctx, t.ID)
	return l, e.SyncLink(ctx, l, opts)
}

// findCreatedSubIssue looks for the sub-issue the connector created for a task, by its marker.
func (e *Engine) findCreatedSubIssue(ctx context.Context, p *mapping.Project, repo string, taskID int64, pend *state.Pending) (*github.Issue, error) {
	issues, err := e.gh(p).ListIssues(ctx, repo, pend.At.Add(-time.Minute), "all")
	if err != nil {
		return nil, err
	}
	for _, is := range issues {
		if id, ok := text.TaskID(is.Body); ok && id == taskID {
			return is, nil
		}
	}
	return nil, nil
}

// ImportSubIssue creates the public task of a sub-issue of a synced requirement's issue and links
// them. Safe to retry, like ImportIssue.
func (e *Engine) ImportSubIssue(ctx context.Context, p *mapping.Project, is *github.Issue, rl *state.Link, opts SyncOptions) (*state.Link, error) {
	log := e.log.With("issue", fmt.Sprintf("%s#%d", is.Repo, is.Number), "requirement", rl.ReqID)

	// A sub-issue the connector created from a task: link it, never import it.
	if taskID, ok := text.TaskID(is.Body); ok {
		if l, err := e.st.TaskLink(ctx, taskID); err == nil {
			return l, nil
		}
		log.Info("recovering the link of a sub-issue created by the connector", "task", taskID)
		l := &state.Link{TaskID: taskID, ReqID: rl.ReqID, ParentNode: rl.IssueNodeID, JikuProject: p.JikuProject,
			IssueNodeID: is.NodeID, Repo: is.Repo, Number: is.Number,
			Last: state.Snapshot{Title: is.Title, GHOpen: is.Open(), GHStateReason: is.StateReason}}
		l.Last.Bodies(text.Hash(is.Body), text.Hash(is.Body))
		if err := e.st.SaveTaskLink(ctx, l); err != nil {
			return nil, err
		}
		_ = e.st.ClearPendingTask(ctx, taskID)
		return l, e.SyncLink(ctx, l, opts)
	}

	if pend, err := e.st.PendingSubIssue(ctx, is.NodeID); err == nil {
		found, err := e.findCreatedTask(ctx, p.JikuProject, pend)
		if err != nil {
			return nil, err
		}
		if found != nil {
			log.Info("recovering an interrupted import", "task", found.ID)
			return e.linkImportedTask(ctx, p, is, rl, found, nil, opts)
		}
	} else if !errors.Is(err, state.ErrNotFound) {
		return nil, err
	}

	tmp := &state.Link{Repo: is.Repo, Number: is.Number}
	desc, files, err := e.toJiku(ctx, p, tmp, is.Body, func() (string, error) { return e.gh(p).IssueHTML(ctx, is.Repo, is.Number) }, nil)
	if err != nil {
		return nil, err
	}
	in := jiku.NewTask{
		Title: is.Title, Description: text.Normalize(desc), FileIDs: files,
		ProjectID: p.JikuProject, RequirementID: rl.ReqID, State: "backlog", VisibilityLevel: "public",
	}
	if err := e.st.SetPendingSubIssue(ctx, is.NodeID, state.Pending{At: e.now().UTC(), Title: is.Title}); err != nil {
		return nil, err
	}
	id, err := e.jk.CreateTask(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("creating the task of %s#%d: %w", is.Repo, is.Number, err)
	}
	log.Info("task created from sub-issue", "task", id)
	desc = in.Description
	t := &jiku.Task{ID: id, Title: in.Title, Description: &desc, Priority: "sin_prioridad", State: in.State,
		ProjectID: p.JikuProject, RequirementID: &rl.ReqID, VisibilityLevel: "public"}
	return e.linkImportedTask(ctx, p, is, rl, t, tmp.Assets, opts)
}

func (e *Engine) linkImportedTask(ctx context.Context, p *mapping.Project, is *github.Issue, rl *state.Link, t *jiku.Task, assets []state.Asset, opts SyncOptions) (*state.Link, error) {
	// Jiku's side is taken as "as created" and GitHub's as unknown, so the first sync carries
	// GitHub's state and priority over to the new task.
	l := &state.Link{
		TaskID: t.ID, ReqID: rl.ReqID, ParentNode: rl.IssueNodeID, JikuProject: p.JikuProject,
		IssueNodeID: is.NodeID, Repo: is.Repo, Number: is.Number,
		Last:   state.Snapshot{Title: is.Title, JikuState: t.State, JikuPriority: t.Priority, GHOpen: true},
		Assets: assets,
	}
	l.Last.Bodies(text.Hash(t.Desc()), text.Hash(is.Body))
	if err := e.st.SaveTaskLink(ctx, l); err != nil {
		return nil, err
	}
	_ = e.st.ClearPendingSubIssue(ctx, is.NodeID)
	return l, e.SyncLink(ctx, l, opts)
}

// findCreatedTask looks for a task the connector created after a pending record.
func (e *Engine) findCreatedTask(ctx context.Context, project int64, pend *state.Pending) (*jiku.Task, error) {
	tasks, err := e.jk.ListTasks(ctx, project, map[string]any{
		"createdBy": e.jk.SelfID(),
		"createdAt": map[string]any{"gte": pend.At.Add(-time.Minute).Format(time.RFC3339)},
	})
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t.Title == pend.Title {
			return t, nil
		}
	}
	return nil, nil
}
