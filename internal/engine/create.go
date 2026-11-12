package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

var attrRe = regexp.MustCompile(`^\*\*@([^*]+)\*\* comentó en \[GitHub\]\(([^)]+)\)`)

// ImportIssue creates the requirement of an issue (R2) and links them. It returns the link.
//
// It is safe to retry: an issue the connector created carries the requirement id, and a creation
// interrupted between Jiku's answer and the link is found again through its pending record.
func (e *Engine) ImportIssue(ctx context.Context, p *mapping.Project, is *github.Issue, opts SyncOptions) (*state.Link, error) {
	log := e.log.With("issue", fmt.Sprintf("%s#%d", is.Repo, is.Number))

	// An issue the connector created from a requirement: link it, never import it.
	if reqID, ok := text.RequirementID(is.Body); ok {
		if l, err := e.st.Link(ctx, reqID); err == nil {
			return l, nil
		}
		log.Info("recovering the link of an issue created by the connector", "requirement", reqID)
		l := &state.Link{ReqID: reqID, JikuProject: p.JikuProject, IssueNodeID: is.NodeID, Repo: is.Repo, Number: is.Number,
			Last: state.Snapshot{Title: is.Title, BodyHash: text.Hash(is.Body), GHOpen: is.Open(), GHStateReason: is.StateReason}}
		if err := e.st.SaveLink(ctx, l); err != nil {
			return nil, err
		}
		_ = e.st.ClearPendingRequirement(ctx, reqID)
		return l, e.SyncLink(ctx, l, opts)
	}

	// A creation interrupted before its link was stored.
	if pend, err := e.st.PendingIssue(ctx, is.NodeID); err == nil {
		found, err := e.findCreatedRequirement(ctx, p.JikuProject, pend)
		if err != nil {
			return nil, err
		}
		if found != nil {
			log.Info("recovering an interrupted import", "requirement", found.ID)
			return e.linkImported(ctx, p, is, found, opts)
		}
	} else if !errors.Is(err, state.ErrNotFound) {
		return nil, err
	}

	in := jiku.NewRequirement{
		Title:           is.Title,
		Description:     text.Normalize(is.Body),
		ProjectID:       p.JikuProject,
		VisibilityLevel: "public",
	}
	if strings.TrimSpace(in.Description) == "" {
		in.Description = "_(sin descripción en GitHub)_"
	}
	if t, ok := p.TypeFromGithub(is.IssueType, is.Labels); ok {
		in.Type = &t
	} else {
		otro := "otro"
		in.Type = &otro
	}
	if err := e.st.SetPendingIssue(ctx, is.NodeID, state.Pending{At: e.now().UTC(), Title: is.Title}); err != nil {
		return nil, err
	}
	id, err := e.jk.CreateRequirement(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("creating the requirement of %s#%d: %w", is.Repo, is.Number, err)
	}
	log.Info("requirement created from issue", "requirement", id)
	req := &jiku.Requirement{ID: id, Title: in.Title, Description: in.Description, Type: in.Type,
		Priority: "sin_prioridad", State: "analisis", ProjectID: p.JikuProject, VisibilityLevel: "public"}
	return e.linkImported(ctx, p, is, req, opts)
}

func (e *Engine) linkImported(ctx context.Context, p *mapping.Project, is *github.Issue, req *jiku.Requirement, opts SyncOptions) (*state.Link, error) {
	// Jiku's side is taken as "as created" and GitHub's as unknown, so the first sync carries
	// GitHub's state, priority and type over to the new requirement.
	l := &state.Link{
		ReqID: req.ID, JikuProject: p.JikuProject, IssueNodeID: is.NodeID, Repo: is.Repo, Number: is.Number,
		Last: state.Snapshot{
			Title: is.Title, BodyHash: text.Hash(is.Body),
			JikuState: req.State, JikuPriority: req.Priority, JikuType: req.TypeOrEmpty(),
			GHOpen: true, GHType: req.TypeOrEmpty(),
		},
	}
	if err := e.st.SaveLink(ctx, l); err != nil {
		return nil, err
	}
	_ = e.st.ClearPendingIssue(ctx, is.NodeID)
	return l, e.SyncLink(ctx, l, opts)
}

// findCreatedRequirement looks for a requirement the connector created after a pending record.
func (e *Engine) findCreatedRequirement(ctx context.Context, project int64, pend *state.Pending) (*jiku.Requirement, error) {
	reqs, err := e.jk.ListRequirements(ctx, project, map[string]any{
		"createdBy": e.jk.SelfID(),
		"createdAt": map[string]any{"gte": pend.At.Add(-time.Minute).Format(time.RFC3339)},
	})
	if err != nil {
		return nil, err
	}
	for _, r := range reqs {
		if r.Title == pend.Title {
			return r, nil
		}
	}
	return nil, nil
}

// ExportRequirement creates the issue of a requirement (R1) in the default repo, adds it to the
// Project and links them. Safe to retry, like ImportIssue.
func (e *Engine) ExportRequirement(ctx context.Context, p *mapping.Project, req *jiku.Requirement, opts SyncOptions) (*state.Link, error) {
	log := e.log.With("requirement", req.ID, "repo", p.DefaultRepo)

	if pend, err := e.st.PendingRequirement(ctx, req.ID); err == nil {
		is, err := e.findCreatedIssue(ctx, p, p.DefaultRepo, req.ID, pend)
		if err != nil {
			return nil, err
		}
		if is != nil {
			log.Info("recovering an interrupted export", "issue", is.Number)
			return e.linkExported(ctx, p, req, is, opts)
		}
	} else if !errors.Is(err, state.ErrNotFound) {
		return nil, err
	}

	in := github.IssueInput{Title: strp(req.Title), Body: strp(text.IssueBody(req.Description, req.ID))}
	if name, set := p.IssueTypeForJiku(req.TypeOrEmpty()); set && name != "" {
		in.Type = &name
	}
	if err := e.st.SetPendingRequirement(ctx, req.ID, state.Pending{At: e.now().UTC(), Title: req.Title}); err != nil {
		return nil, err
	}
	is, err := e.gh(p).CreateIssue(ctx, p.DefaultRepo, in)
	if err != nil && in.Type != nil {
		// The org may not have that issue type: create it without, and log why.
		log.Warn("creating the issue with its type failed; retrying without type", "type", *in.Type, "error", err)
		in.Type = nil
		is, err = e.gh(p).CreateIssue(ctx, p.DefaultRepo, in)
	}
	if err != nil {
		return nil, fmt.Errorf("creating the issue of requirement %d: %w", req.ID, err)
	}
	log.Info("issue created from requirement", "issue", is.Number)
	return e.linkExported(ctx, p, req, is, opts)
}

func (e *Engine) linkExported(ctx context.Context, p *mapping.Project, req *jiku.Requirement, is *github.Issue, opts SyncOptions) (*state.Link, error) {
	// GitHub's side is taken as "as created" and Jiku's as unknown, so the first sync carries
	// Jiku's state, priority and type over to the issue and its Project item.
	ghType, _ := p.TypeFromGithub(is.IssueType, is.Labels)
	l := &state.Link{
		ReqID: req.ID, JikuProject: p.JikuProject, IssueNodeID: is.NodeID, Repo: is.Repo, Number: is.Number,
		Last: state.Snapshot{
			Title: req.Title, BodyHash: text.Hash(req.Description),
			GHOpen: is.Open(), GHStateReason: is.StateReason, GHType: ghType,
			JikuType: req.TypeOrEmpty(),
		},
	}
	if err := e.st.SaveLink(ctx, l); err != nil {
		return nil, err
	}
	_ = e.st.ClearPendingRequirement(ctx, req.ID)
	return l, e.SyncLink(ctx, l, opts)
}

// findCreatedIssue looks for the issue the connector created for a requirement after a pending
// record, by its marker.
func (e *Engine) findCreatedIssue(ctx context.Context, p *mapping.Project, repo string, reqID int64, pend *state.Pending) (*github.Issue, error) {
	issues, err := e.gh(p).ListIssues(ctx, repo, pend.At.Add(-time.Minute), "all")
	if err != nil {
		return nil, err
	}
	for _, is := range issues {
		if id, ok := text.RequirementID(is.Body); ok && id == reqID {
			return is, nil
		}
	}
	return nil, nil
}
