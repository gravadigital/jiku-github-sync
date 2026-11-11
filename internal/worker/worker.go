package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/engine"
	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
)

// GitHubPoller is what polling needs from GitHub.
type GitHubPoller interface {
	ListIssues(ctx context.Context, repo string, since time.Time, state string) ([]*github.Issue, error)
	ListRepoComments(ctx context.Context, repo string, since time.Time) ([]*github.Comment, error)
	ListItems(ctx context.Context, projectID string) ([]*github.Item, error)
	GetRepo(ctx context.Context, repo string) (*github.Repo, error)
}

// Events is Jiku's event stream; Run blocks, calling handle for each requirement event.
type Events interface {
	Run(ctx context.Context, handle func(Event)) error
}

// Event is the part of a Jiku event the worker needs.
type Event struct {
	ID           string
	Type         string
	ReqID        int64
	ProjectID    int64
	ActorID      string
	BecamePublic bool
}

// Options configures a Worker.
type Options struct {
	Engine            *engine.Engine
	GitHub            GitHubPoller
	Jiku              jiku.API
	Store             *state.Store
	Events            Events // nil: no event stream (polling and reconciliation only)
	Logger            *slog.Logger
	PollInterval      time.Duration
	ReconcileInterval time.Duration
	MappingFile       string
	ConfigReload      time.Duration
}

// Worker runs everything.
type Worker struct {
	o     Options
	q     *Queue
	log   *slog.Logger
	items sync.Map // project item id -> hash of its values

	lastPollOK  atomic.Int64 // unix seconds
	lastEventOK atomic.Int64
	synced      atomic.Int64
	failed      atomic.Int64
}

// New builds a Worker.
func New(o Options) *Worker {
	w := &Worker{o: o, q: NewQueue(10000), log: o.Logger}
	if w.log == nil {
		w.log = slog.Default()
	}
	return w
}

// Enqueue adds something to sync (used by the webhook receiver).
func (w *Worker) Enqueue(k Key) {
	if !w.q.Push(k) {
		w.log.Warn("queue full: dropping a trigger (the reconciliation will catch up)", "key", k.id())
	}
}

// Ready reports whether GitHub was polled recently.
func (w *Worker) Ready(context.Context) error {
	last := w.lastPollOK.Load()
	if last == 0 {
		return errors.New("first GitHub poll not done yet")
	}
	if age := time.Since(time.Unix(last, 0)); age > 3*w.o.PollInterval+time.Minute {
		return fmt.Errorf("last successful GitHub poll %s ago", age.Round(time.Second))
	}
	return nil
}

// Stats is a snapshot of counters, for logs and tests.
func (w *Worker) Stats() (queued int, synced, failed int64) {
	return w.q.Len(), w.synced.Load(), w.failed.Load()
}

// Run starts every loop and blocks until ctx is done.
func (w *Worker) Run(ctx context.Context) error {
	w.startProjects(ctx)
	var wg sync.WaitGroup
	loops := []func(context.Context){w.processLoop, w.pollLoop, w.reconcileLoop, w.reloadLoop}
	if w.o.Events != nil {
		loops = append(loops, w.eventsLoop)
	}
	for _, f := range loops {
		wg.Add(1)
		go func(f func(context.Context)) {
			defer wg.Done()
			f(ctx)
		}(f)
	}
	wg.Wait()
	return nil
}

func (w *Worker) startProjects(ctx context.Context) {
	for _, p := range w.o.Engine.Config().Projects {
		started, err := w.o.Engine.EnsureStarted(ctx, p.JikuProject)
		if err != nil {
			w.log.Error("project state", "project", p.JikuProject, "error", err)
			continue
		}
		w.log.Info("watching project", "project", p.JikuProject, "repos", p.Repos, "githubProject", p.GithubProject, "since", started)
		for _, r := range p.Repos {
			repo, err := w.o.GitHub.GetRepo(ctx, r)
			if err != nil {
				w.log.Error("cannot read a mapped repo", "repo", r, "error", err)
				continue
			}
			if repo.Visibility == "public" {
				// R10: in a public repo anybody's issue becomes a public requirement.
				w.log.Warn("mapped repo is PUBLIC: any GitHub user can create public requirements in Jiku", "repo", r)
			}
		}
	}
}

func (w *Worker) processLoop(ctx context.Context) {
	for {
		k, ok := w.q.Pop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-w.q.Signal():
				continue
			case <-time.After(time.Second):
				continue
			}
		}
		var err error
		switch k.Kind {
		case Requirement:
			err = w.o.Engine.HandleRequirement(ctx, k.ReqID, k.BecamePublic)
		case Issue:
			err = w.o.Engine.HandleIssue(ctx, k.Repo, k.Number)
		case IssueNode:
			err = w.o.Engine.HandleIssueNode(ctx, k.Node)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.failed.Add(1)
			w.log.Error("sync failed (retried on the next reconciliation)", "key", k.id(), "error", err)
			continue
		}
		w.synced.Add(1)
	}
}

func (w *Worker) every(ctx context.Context, d time.Duration, f func(context.Context)) {
	f(ctx)
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f(ctx)
		}
	}
}

func (w *Worker) pollLoop(ctx context.Context) {
	w.every(ctx, w.o.PollInterval, func(ctx context.Context) {
		if err := w.PollOnce(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("github poll failed", "error", err)
			return
		}
		w.lastPollOK.Store(time.Now().Unix())
	})
}

// PollOnce reads what changed on GitHub since the cursors and queues it.
func (w *Worker) PollOnce(ctx context.Context) error {
	var errs []error
	for _, p := range w.o.Engine.Config().Projects {
		if err := w.pollProject(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("project %d: %w", p.JikuProject, err))
		}
	}
	return errors.Join(errs...)
}

// cursorSkew re-reads a little before the cursor: GitHub's `since` has second resolution and
// updates can land out of order.
const cursorSkew = 5 * time.Second

func (w *Worker) pollProject(ctx context.Context, p *mapping.Project) error {
	ps, err := w.o.Store.Project(ctx, p.JikuProject)
	if err != nil {
		return err
	}
	issueCur := map[string]time.Time{}
	commentCur := map[string]time.Time{}
	for _, repo := range p.Repos {
		since := ps.IssueCursor[repo]
		if since.IsZero() {
			since = ps.StartedAt.Add(-time.Minute)
		}
		issues, err := w.o.GitHub.ListIssues(ctx, repo, since.Add(-cursorSkew), "all")
		if err != nil {
			return err
		}
		maxAt := since
		for _, is := range issues {
			w.Enqueue(Key{Kind: Issue, Repo: is.Repo, Number: is.Number})
			if is.UpdatedAt.After(maxAt) {
				maxAt = is.UpdatedAt
			}
		}
		issueCur[repo] = maxAt

		csince := ps.CommentCursor[repo]
		if csince.IsZero() {
			csince = ps.StartedAt.Add(-time.Minute)
		}
		comments, err := w.o.GitHub.ListRepoComments(ctx, repo, csince.Add(-cursorSkew))
		if err != nil {
			return err
		}
		cmax := csince
		for _, c := range comments {
			if c.IssueNum > 0 {
				w.Enqueue(Key{Kind: Issue, Repo: repo, Number: c.IssueNum})
			}
			if c.UpdatedAt.After(cmax) {
				cmax = c.UpdatedAt
			}
		}
		commentCur[repo] = cmax
	}
	// Project fields do not touch the issue's updated_at: compare each item's values instead.
	if p.GithubProject != "" {
		items, err := w.o.GitHub.ListItems(ctx, p.GithubProject)
		if err != nil {
			return err
		}
		for _, it := range items {
			h := valuesHash(it.Values)
			if prev, ok := w.items.Load(it.ID); !ok || prev.(string) != h {
				w.items.Store(it.ID, h)
				w.Enqueue(Key{Kind: IssueNode, Node: it.ContentNodeID})
			}
		}
	}
	return w.o.Store.UpdateProject(ctx, p.JikuProject, func(s *state.ProjectState) {
		if s.IssueCursor == nil {
			s.IssueCursor = map[string]time.Time{}
		}
		if s.CommentCursor == nil {
			s.CommentCursor = map[string]time.Time{}
		}
		for k, v := range issueCur {
			s.IssueCursor[k] = v
		}
		for k, v := range commentCur {
			s.CommentCursor[k] = v
		}
	})
}

func valuesHash(v map[string]github.FieldValue) string {
	ks := make([]string, 0, len(v))
	for k := range v {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	h := sha256.New()
	for _, k := range ks {
		fmt.Fprintf(h, "%s=%s;", k, v[k].Option)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func (w *Worker) reconcileLoop(ctx context.Context) {
	w.every(ctx, w.o.ReconcileInterval, func(ctx context.Context) {
		if err := w.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("reconciliation failed", "error", err)
		}
	})
}

// ReconcileOnce queues every linked pair and every public requirement born after the project
// started that has no issue yet: whatever a lost event or poll missed is caught here.
func (w *Worker) ReconcileOnce(ctx context.Context) error {
	w.o.Engine.InvalidateSchemas()
	var errs []error
	n := 0
	for _, p := range w.o.Engine.Config().Projects {
		ps, err := w.o.Store.Project(ctx, p.JikuProject)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		linked := map[int64]bool{}
		for _, id := range ps.Links {
			linked[id] = true
			w.Enqueue(Key{Kind: Requirement, ReqID: id})
			n++
		}
		reqs, err := w.o.Jiku.ListRequirements(ctx, p.JikuProject, map[string]any{
			"visibilityLevel": "public",
			"createdAt":       map[string]any{"gte": ps.StartedAt.UTC().Format(time.RFC3339)},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("project %d: %w", p.JikuProject, err))
			continue
		}
		for _, r := range reqs {
			if !linked[r.ID] {
				w.Enqueue(Key{Kind: Requirement, ReqID: r.ID})
				n++
			}
		}
	}
	w.log.Info("reconciliation queued", "items", n)
	return errors.Join(errs...)
}

func (w *Worker) eventsLoop(ctx context.Context) {
	self := w.o.Jiku.SelfID()
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		err := w.o.Events.Run(ctx, func(ev Event) {
			w.lastEventOK.Store(time.Now().Unix())
			if ev.ActorID == self && !ev.BecamePublic {
				return // the connector's own writes
			}
			if w.o.Engine.Config().Project(ev.ProjectID) == nil {
				return
			}
			w.Enqueue(Key{Kind: Requirement, ReqID: ev.ReqID, BecamePublic: ev.BecamePublic})
		})
		if ctx.Err() != nil {
			return
		}
		w.log.Error("jiku event stream stopped; reconnecting", "error", err, "in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (w *Worker) reloadLoop(ctx context.Context) {
	if w.o.MappingFile == "" || w.o.ConfigReload <= 0 {
		return
	}
	var lastMod time.Time
	if fi, err := os.Stat(w.o.MappingFile); err == nil {
		lastMod = fi.ModTime()
	}
	t := time.NewTicker(w.o.ConfigReload)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fi, err := os.Stat(w.o.MappingFile)
		if err != nil || !fi.ModTime().After(lastMod) {
			continue
		}
		lastMod = fi.ModTime()
		f, err := mapping.Load(w.o.MappingFile)
		if err != nil {
			w.log.Error("mapping file changed but is invalid: keeping the previous one", "error", err)
			continue
		}
		w.o.Engine.SetConfig(f)
		w.log.Info("mapping reloaded", "projects", len(f.Projects))
		w.startProjects(ctx)
		w.items.Range(func(k, _ any) bool { w.items.Delete(k); return true })
	}
}
