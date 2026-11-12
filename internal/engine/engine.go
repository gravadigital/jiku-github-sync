// Package engine is the sync engine. It is state-based, not event-based: whatever triggers it (a
// Jiku event, a GitHub poll, a webhook, the periodic reconciliation), it reads both sides as they
// are now, compares each field with what both sides looked like after the last sync, and pushes
// the side that changed. When both changed, the most recent change wins.
//
// That makes every run idempotent: a duplicated or out-of-order trigger finds nothing to do, and
// the connector's own writes never echo back, because after a sync both sides equal the snapshot.
package engine

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
)

// DefaultItemSettle is how long the engine waits for a Project's "item added" workflow.
const DefaultItemSettle = 8 * time.Second

// AutomationLogin is the actor of GitHub's built-in Project workflows.
const AutomationLogin = "github-project-automation"

// GitHub is what the engine needs from GitHub; *github.Client implements it.
type GitHub interface {
	GetIssue(ctx context.Context, repo string, number int) (*github.Issue, error)
	IssueRef(ctx context.Context, nodeID string) (string, int, error)
	ListIssues(ctx context.Context, repo string, since time.Time, state string) ([]*github.Issue, error)
	CreateIssue(ctx context.Context, repo string, in github.IssueInput) (*github.Issue, error)
	UpdateIssue(ctx context.Context, repo string, number int, in github.IssueInput) (*github.Issue, error)
	ListComments(ctx context.Context, repo string, number int) ([]*github.Comment, error)
	CreateComment(ctx context.Context, repo string, number int, body string) (*github.Comment, error)
	UpdateComment(ctx context.Context, repo string, id int64, body string) error
	ProjectSchema(ctx context.Context, projectID string) (*github.ProjectSchema, error)
	ItemForIssue(ctx context.Context, issueNodeID, projectID string) (*github.Item, error)
	AddItem(ctx context.Context, projectID, contentNodeID string) (string, error)
	SetSelect(ctx context.Context, projectID, itemID, fieldID, optionID string) error
	ClearField(ctx context.Context, projectID, itemID, fieldID string) error
	LastStatusChange(ctx context.Context, issueNodeID, projectID string) (*github.StatusChange, error)
	// Attachments.
	UploadAsset(ctx context.Context, repo, name, contentType string, data []byte) (string, error)
	Download(ctx context.Context, url string) ([]byte, string, error)
	IssueHTML(ctx context.Context, repo string, number int) (string, error)
	CommentHTML(ctx context.Context, repo string, id int64) (string, error)
}

// Engine syncs requirements and issues.
type Engine struct {
	// ghFor returns the GitHub client of a project: its client's credentials.
	ghFor func(*mapping.Project) GitHub
	jk    jiku.API
	st    *state.Store
	log   *slog.Logger

	cfgMu sync.RWMutex
	cfg   *mapping.File

	schemaMu  sync.Mutex
	schemas   map[string]schemaEntry
	schemaTTL time.Duration

	now func() time.Time
	// ItemSettle is how long to wait for the Project's "item added" workflow after adding an
	// item, before writing its fields (otherwise the workflow can overwrite them).
	ItemSettle time.Duration
}

type schemaEntry struct {
	s  *github.ProjectSchema
	at time.Time
}

// Options configures an Engine.
type Options struct {
	// GitHub is the client of every project, when there is a single credential (tests, trials).
	GitHub GitHub
	// GitHubFor returns the client of a project's credentials; it takes precedence over GitHub.
	GitHubFor func(*mapping.Project) GitHub
	Jiku      jiku.API
	Store     *state.Store
	Config    *mapping.File
	Logger    *slog.Logger
	SchemaTTL time.Duration
	Now       func() time.Time
}

// New builds an Engine.
func New(o Options) *Engine {
	e := &Engine{
		ghFor: o.GitHubFor, jk: o.Jiku, st: o.Store, log: o.Logger, cfg: o.Config,
		schemas: map[string]schemaEntry{}, schemaTTL: o.SchemaTTL, now: o.Now,
		ItemSettle: DefaultItemSettle,
	}
	if e.ghFor == nil {
		gh := o.GitHub
		e.ghFor = func(*mapping.Project) GitHub { return gh }
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	if e.schemaTTL == 0 {
		e.schemaTTL = 5 * time.Minute
	}
	if e.now == nil {
		e.now = time.Now
	}
	return e
}

// Config returns the current mapping.
func (e *Engine) Config() *mapping.File {
	e.cfgMu.RLock()
	defer e.cfgMu.RUnlock()
	return e.cfg
}

// SetConfig replaces the mapping (hot reload) and drops the cached Project schemas.
func (e *Engine) SetConfig(f *mapping.File) {
	e.cfgMu.Lock()
	e.cfg = f
	e.cfgMu.Unlock()
	e.InvalidateSchemas()
}

// InvalidateSchemas forces the Project schemas to be read again (names → ids).
func (e *Engine) InvalidateSchemas() {
	e.schemaMu.Lock()
	e.schemas = map[string]schemaEntry{}
	e.schemaMu.Unlock()
}

// gh is the GitHub client of a project.
func (e *Engine) gh(p *mapping.Project) GitHub { return e.ghFor(p) }

func (e *Engine) schema(ctx context.Context, p *mapping.Project) (*github.ProjectSchema, error) {
	projectID := p.GithubProject
	e.schemaMu.Lock()
	ent, ok := e.schemas[projectID]
	e.schemaMu.Unlock()
	if ok && e.now().Sub(ent.at) < e.schemaTTL {
		return ent.s, nil
	}
	s, err := e.gh(p).ProjectSchema(ctx, projectID)
	if err != nil {
		return nil, err
	}
	e.schemaMu.Lock()
	e.schemas[projectID] = schemaEntry{s: s, at: e.now()}
	e.schemaMu.Unlock()
	return s, nil
}

// EnsureStarted records when the connector started watching a project (only the first time).
func (e *Engine) EnsureStarted(ctx context.Context, p int64) (time.Time, error) {
	var started time.Time
	err := e.st.UpdateProject(ctx, p, func(ps *state.ProjectState) {
		if ps.StartedAt.IsZero() {
			ps.StartedAt = e.now().UTC()
		}
		started = ps.StartedAt
	})
	return started, err
}

// HandleRequirement syncs one requirement: its pair if it has one, or the creation of its issue
// when it qualifies (public, in a mapped project, born after the connector started watching, or
// forced because it just became public).
func (e *Engine) HandleRequirement(ctx context.Context, reqID int64, becamePublic bool) error {
	l, err := e.st.Link(ctx, reqID)
	switch {
	case err == nil:
		return e.SyncLink(ctx, l, SyncOptions{})
	case !errors.Is(err, state.ErrNotFound):
		return err
	}
	req, err := e.jk.GetRequirement(ctx, reqID)
	if errors.Is(err, jiku.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	p := e.Config().Project(req.ProjectID)
	if p == nil || !req.Public() {
		return nil
	}
	if !becamePublic {
		started, err := e.EnsureStarted(ctx, p.JikuProject)
		if err != nil {
			return err
		}
		if req.CreatedAt.Before(started) {
			return nil // pre-existing: the initial sync's job
		}
	}
	_, err = e.ExportRequirement(ctx, p, req, SyncOptions{})
	return err
}

// HandleIssue syncs one issue of a mapped repo: its pair if it has one, or the creation of its
// requirement when it qualifies (open, born after the connector started watching).
func (e *Engine) HandleIssue(ctx context.Context, repo string, number int) error {
	p := e.Config().ProjectForRepo(repo)
	if p == nil {
		return nil
	}
	is, err := e.gh(p).GetIssue(ctx, repo, number)
	if errors.Is(err, github.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return e.handleIssue(ctx, is)
}

// HandleIssueNode is HandleIssue by node id, for an item of the given Jiku project's GitHub
// Project (Project item polls and webhooks carry only the node id).
func (e *Engine) HandleIssueNode(ctx context.Context, jikuProject int64, nodeID string) error {
	if l, err := e.st.LinkByIssue(ctx, nodeID); err == nil {
		return e.SyncLink(ctx, l, SyncOptions{})
	}
	p := e.Config().Project(jikuProject)
	if p == nil {
		return nil
	}
	repo, num, err := e.gh(p).IssueRef(ctx, nodeID)
	if errors.Is(err, github.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return e.HandleIssue(ctx, repo, num)
}

func (e *Engine) handleIssue(ctx context.Context, is *github.Issue) error {
	if is.IsPR {
		return nil
	}
	l, err := e.st.LinkByIssue(ctx, is.NodeID)
	switch {
	case err == nil:
		return e.SyncLink(ctx, l, SyncOptions{})
	case !errors.Is(err, state.ErrNotFound):
		return err
	}
	p := e.Config().ProjectForRepo(is.Repo)
	if p == nil {
		return nil
	}
	started, err := e.EnsureStarted(ctx, p.JikuProject)
	if err != nil {
		return err
	}
	if is.CreatedAt.Before(started) || !is.Open() {
		return nil
	}
	_, err = e.ImportIssue(ctx, p, is, SyncOptions{})
	return err
}

// SyncOptions tunes a sync.
type SyncOptions struct {
	// WithDates writes the original date in the attribution of copied comments (initial sync).
	WithDates bool
}

func (e *Engine) logPair(l *state.Link) *slog.Logger {
	return e.log.With("requirement", l.ReqID, "issue", l.IssueKey())
}

func strp(s string) *string { return &s }
