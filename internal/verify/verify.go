// Package verify compares both sides of every linked pair of a project, read-only, and reports
// every difference: the check to run after an initial sync, or any time drift is suspected.
package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/engine"
	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

// Report is the outcome of a verification.
type Report struct {
	Pairs      int
	PairsOK    int
	Unlinked   int // tombstones (requirement became internal)
	Diffs      []string
	Missing    []string // open public requirements / open issues with no pair
	CommentsJ  int      // public Jiku comments checked
	CommentsGH int      // GitHub comments checked
}

// Run verifies a project.
func Run(ctx context.Context, gh engine.GitHub, ghl lister, jk jiku.API, st *state.Store, p *mapping.Project) (*Report, error) {
	r := &Report{}
	ps, err := st.Project(ctx, p.JikuProject)
	if err != nil {
		return nil, err
	}
	linkedReq := map[int64]bool{}
	linkedIssue := map[string]bool{}
	for _, id := range ps.Links {
		l, err := st.Link(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("link %d: %w", id, err)
		}
		linkedReq[l.ReqID] = true
		linkedIssue[l.IssueNodeID] = true
		if l.Unlinked {
			r.Unlinked++
			continue
		}
		r.Pairs++
		diffs, err := pair(ctx, gh, jk, p, l, r)
		if err != nil {
			r.Diffs = append(r.Diffs, fmt.Sprintf("req %d ↔ %s: %v", l.ReqID, l.IssueKey(), err))
			continue
		}
		if len(diffs) == 0 {
			r.PairsOK++
		}
		for _, d := range diffs {
			r.Diffs = append(r.Diffs, fmt.Sprintf("req %d ↔ %s: %s", l.ReqID, l.IssueKey(), d))
		}
	}
	reqs, err := jk.ListRequirements(ctx, p.JikuProject, map[string]any{"visibilityLevel": "public"})
	if err != nil {
		return nil, err
	}
	for _, q := range reqs {
		if !linkedReq[q.ID] && q.State != "resuelto" && q.State != "cancelado" {
			r.Missing = append(r.Missing, fmt.Sprintf("requisito público abierto %d sin issue: %q", q.ID, q.Title))
		}
	}
	for _, repo := range p.Repos {
		issues, err := ghl.ListIssues(ctx, repo, zeroTime, "open")
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			if !is.IsPR && !linkedIssue[is.NodeID] {
				r.Missing = append(r.Missing, fmt.Sprintf("issue abierto %s#%d sin requisito: %q", is.Repo, is.Number, is.Title))
			}
		}
	}
	return r, nil
}

func pair(ctx context.Context, gh engine.GitHub, jk jiku.API, p *mapping.Project, l *state.Link, r *Report) ([]string, error) {
	req, err := jk.GetRequirement(ctx, l.ReqID)
	if err != nil {
		return nil, err
	}
	is, err := gh.GetIssue(ctx, l.Repo, l.Number)
	if err != nil {
		return nil, err
	}
	var d []string
	if req.Title != is.Title {
		d = append(d, fmt.Sprintf("título: Jiku %q / GitHub %q", req.Title, is.Title))
	}
	if text.Normalize(req.Description) != text.Normalize(is.Body) {
		d = append(d, "descripción distinta")
	}
	switch req.State {
	case "resuelto", "cancelado":
		want := "completed"
		if req.State == "cancelado" {
			want = "not_planned"
		}
		dupOK := want == "not_planned" && is.StateReason == "duplicate"
		if is.Open() || (is.StateReason != want && !dupOK) {
			d = append(d, fmt.Sprintf("estado: Jiku %s / issue %s %s", req.State, is.State, is.StateReason))
		}
	default:
		if !is.Open() {
			d = append(d, fmt.Sprintf("estado: Jiku %s / issue cerrado", req.State))
		}
	}
	var item *github.Item
	if p.GithubProject != "" {
		if item, err = gh.ItemForIssue(ctx, is.NodeID, p.GithubProject); err != nil {
			return nil, err
		}
		if item == nil {
			d = append(d, "el issue no está en el Project")
		}
	}
	if item != nil && p.Fields != nil {
		if s := p.Fields.Status; s != nil && is.Open() {
			got := item.Values[s.Field].Option
			if t, ok := p.StateFromOption(got); !ok || t.State != req.State {
				d = append(d, fmt.Sprintf("columna %q no corresponde al estado %s", got, req.State))
			}
		}
		if pr := p.Fields.Priority; pr != nil {
			got := item.Values[pr.Field].Option
			if want, ok := p.OptionForPriority(req.Priority); ok && got != want {
				if gp, gok := p.PriorityFromOption(got); !gok || gp != req.Priority {
					d = append(d, fmt.Sprintf("prioridad: Jiku %s / GitHub %q", req.Priority, got))
				}
			}
		}
	}
	if name, set := p.IssueTypeForJiku(req.TypeOrEmpty()); set && name != is.IssueType {
		if t, _ := p.TypeFromGithub(is.IssueType, is.Labels); t != req.TypeOrEmpty() {
			d = append(d, fmt.Sprintf("tipo: Jiku %q / issue type %q", req.TypeOrEmpty(), is.IssueType))
		}
	}

	// Comments: every public Jiku comment by a person has its mirror, and the other way round.
	jcs, err := jk.ListComments(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	gcs, err := gh.ListComments(ctx, is.Repo, is.Number)
	if err != nil {
		return nil, err
	}
	mirroredJ := map[int64]bool{}
	for _, c := range gcs {
		if id, ok := text.JikuCommentID(c.Body); ok {
			mirroredJ[id] = true
		}
	}
	mirroredG := map[int64]bool{}
	self := jk.SelfID()
	for _, c := range jcs {
		if c.AuthorID == self {
			if id, ok := text.GitHubCommentID(c.Body); ok {
				mirroredG[id] = true
			}
		}
	}
	for _, c := range jcs {
		if c.AuthorID == self || c.VisibilityLevel != "public" {
			continue
		}
		r.CommentsJ++
		if !mirroredJ[c.ID] {
			d = append(d, fmt.Sprintf("comentario de Jiku %d sin copia en GitHub", c.ID))
		}
	}
	for _, c := range gcs {
		if text.HasMarker(c.Body) {
			continue
		}
		r.CommentsGH++
		if !mirroredG[c.ID] {
			d = append(d, fmt.Sprintf("comentario de GitHub %d sin copia en Jiku", c.ID))
		}
	}
	return d, nil
}

// Render prints the report; it returns an error when something differs.
func (r *Report) Render(w io.Writer) error {
	fmt.Fprintf(w, "Pares vinculados: %d (%d sin diferencias), desvinculados: %d\n", r.Pairs, r.PairsOK, r.Unlinked)
	fmt.Fprintf(w, "Comentarios verificados: %d de Jiku, %d de GitHub\n", r.CommentsJ, r.CommentsGH)
	if len(r.Diffs) > 0 {
		fmt.Fprintf(w, "\nDiferencias (%d):\n  - %s\n", len(r.Diffs), strings.Join(r.Diffs, "\n  - "))
	}
	if len(r.Missing) > 0 {
		fmt.Fprintf(w, "\nSin vincular (%d):\n  - %s\n", len(r.Missing), strings.Join(r.Missing, "\n  - "))
	}
	if len(r.Diffs)+len(r.Missing) > 0 {
		return errors.New("verification found differences")
	}
	fmt.Fprintln(w, "\nTodo coincide.")
	return nil
}

type lister interface {
	ListIssues(ctx context.Context, repo string, since time.Time, state string) ([]*github.Issue, error)
}

var zeroTime time.Time
