package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

type side int

const (
	sideNone side = iota
	sideJiku
	sideGitHub
)

// decide picks which side's value wins for one field. jChanged/gChanged say whether each side
// differs from the last synced value; differ whether the two sides differ now.
func decide(jChanged, gChanged, differ bool, jAt, gAt time.Time) side {
	switch {
	case !differ:
		return sideNone
	case jChanged && !gChanged:
		return sideJiku
	case gChanged && !jChanged:
		return sideGitHub
	case jChanged && gChanged:
		if gAt.After(jAt) {
			return sideGitHub
		}
		return sideJiku
	default:
		// They differ but neither moved since the last sync: something outside the rules moved
		// GitHub back (e.g. a Project workflow). Jiku's value is re-applied.
		return sideJiku
	}
}

func terminal(state string) bool { return state == "resuelto" || state == "cancelado" }

func reasonClass(r string) string {
	if r == "not_planned" || r == "duplicate" {
		return "not_planned"
	}
	return "completed"
}

// ghView is GitHub's side of a pair, read once per sync.
type ghView struct {
	issue *github.Issue
	item  *github.Item

	status   string
	statusBy string
	statusAt time.Time

	priority   string
	priorityAt time.Time

	// typeView is the Jiku type GitHub's issue type / labels map to ("" when nothing maps).
	typeView string
}

func (e *Engine) view(ctx context.Context, p *mapping.Project, is *github.Issue, item *github.Item, last state.Snapshot, log *slog.Logger) *ghView {
	v := &ghView{issue: is, item: item}
	if item != nil && p.Fields != nil {
		if s := p.Fields.Status; s != nil {
			fv := item.Values[s.Field]
			v.status, v.statusAt = fv.Option, fv.UpdatedAt
			// Who moved the card is only in the issue's timeline: read it when the card moved.
			if v.status != last.GHStatus && v.status != "" {
				sc, err := e.gh(p).LastStatusChange(ctx, is.NodeID, p.GithubProject)
				switch {
				case err != nil:
					log.Warn("cannot read who moved the card", "error", err)
				case sc != nil && sc.Status == v.status:
					v.statusBy = sc.Actor
					if sc.CreatedAt.After(v.statusAt) {
						v.statusAt = sc.CreatedAt
					}
				}
			}
		}
		if pr := p.Fields.Priority; pr != nil {
			fv := item.Values[pr.Field]
			v.priority, v.priorityAt = fv.Option, fv.UpdatedAt
		}
	}
	v.typeView, _ = p.TypeFromGithub(is.IssueType, is.Labels)
	return v
}

// fetchIssue reads the pair's issue, following a transfer (the node id survives it).
func (e *Engine) fetchIssue(ctx context.Context, p *mapping.Project, l *state.Link) (*github.Issue, error) {
	is, err := e.gh(p).GetIssue(ctx, l.Repo, l.Number)
	if errors.Is(err, github.ErrNotFound) {
		repo, num, rerr := e.gh(p).IssueRef(ctx, l.IssueNodeID)
		if rerr != nil {
			return nil, rerr
		}
		if is, err = e.gh(p).GetIssue(ctx, repo, num); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if is.Repo != "" && (is.Repo != l.Repo || is.Number != l.Number) {
		e.logPair(l).Info("issue moved", "to", fmt.Sprintf("%s#%d", is.Repo, is.Number))
		l.Repo, l.Number = is.Repo, is.Number
	}
	if is.Repo == "" {
		is.Repo = l.Repo
	}
	return is, nil
}

// ensureItem returns the issue's item in the mapped Project, adding it when it is not there.
func (e *Engine) ensureItem(ctx context.Context, p *mapping.Project, l *state.Link, is *github.Issue, log *slog.Logger) (*github.Item, error) {
	if p.GithubProject == "" {
		return nil, nil
	}
	item, err := e.gh(p).ItemForIssue(ctx, is.NodeID, p.GithubProject)
	if err != nil {
		return nil, err
	}
	if item != nil {
		l.ItemID = item.ID
		return item, nil
	}
	id, err := e.gh(p).AddItem(ctx, p.GithubProject, is.NodeID)
	if err != nil {
		return nil, fmt.Errorf("adding the issue to the Project: %w", err)
	}
	log.Info("issue added to the Project")
	l.ItemID = id
	// Give the Project's "item added" workflow time to run before reading (and writing) fields.
	deadline := e.now().Add(e.ItemSettle)
	for {
		item, err = e.gh(p).ItemForIssue(ctx, is.NodeID, p.GithubProject)
		if err != nil {
			return nil, err
		}
		settled := item != nil && (p.Fields == nil || p.Fields.Status == nil || item.Values[p.Fields.Status.Field].Option != "")
		if settled || !e.now().Before(deadline) || e.ItemSettle <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if item == nil {
		item = &github.Item{ID: id, ContentNodeID: is.NodeID, Values: map[string]github.FieldValue{}}
	}
	return item, nil
}

// setField writes a single-select field by names; option "" clears it.
func (e *Engine) setField(ctx context.Context, p *mapping.Project, item *github.Item, field, option string) error {
	for attempt := 0; attempt < 2; attempt++ {
		s, err := e.schema(ctx, p)
		if err != nil {
			return err
		}
		f := s.Fields[field]
		var optID string
		if f != nil && option != "" {
			optID = f.Options[option]
		}
		if f == nil || (option != "" && optID == "") {
			if attempt == 0 {
				e.InvalidateSchemas() // the Project may have changed: read it again once
				continue
			}
			if f == nil {
				return fmt.Errorf("the Project has no single-select field %q (check the mapping: check-config)", field)
			}
			return fmt.Errorf("the field %q has no option %q (check the mapping: check-config)", field, option)
		}
		if option == "" {
			return e.gh(p).ClearField(ctx, p.GithubProject, item.ID, f.ID)
		}
		return e.gh(p).SetSelect(ctx, p.GithubProject, item.ID, f.ID, optID)
	}
	return nil
}

// ghTarget is the Jiku state GitHub's side implies.
type ghTarget struct {
	state   string
	comment string // internal comment to leave (configured per option)
	known   bool   // false: GitHub says nothing usable about the open state
	// unmapped is the Status option with no mapping, when that is why known is false.
	unmapped string
	// closeIssue: an open issue was moved to a terminal column; the issue is closed to match.
	closeIssue bool
}

func (e *Engine) target(p *mapping.Project, v *ghView, last state.Snapshot) ghTarget {
	is := v.issue
	reopened := is.Open() && !last.GHOpen && last.JikuState != ""
	reopenState := last.LastOpenState
	if reopenState == "" {
		reopenState = "analisis"
	}
	if !is.Open() {
		if reasonClass(is.StateReason) == "not_planned" {
			return ghTarget{state: "cancelado", known: true}
		}
		return ghTarget{state: "resuelto", known: true}
	}
	if v.status == "" {
		if reopened {
			return ghTarget{state: reopenState, known: true}
		}
		return ghTarget{}
	}
	t, ok := p.StateFromOption(v.status)
	if !ok {
		if reopened {
			return ghTarget{state: reopenState, known: true}
		}
		return ghTarget{unmapped: v.status}
	}
	if terminal(t.State) {
		if reopened {
			return ghTarget{state: reopenState, known: true}
		}
		return ghTarget{state: t.State, known: true, closeIssue: true}
	}
	return ghTarget{state: t.State, comment: t.Comment, known: true}
}

// SyncLink syncs one pair, field by field, then its comments.
func (e *Engine) SyncLink(ctx context.Context, l *state.Link, opts SyncOptions) error {
	log := e.logPair(l)
	p := e.Config().Project(l.JikuProject)
	if p == nil {
		log.Debug("project no longer mapped; skipping")
		return nil
	}
	req, err := e.jk.GetRequirement(ctx, l.ReqID)
	if errors.Is(err, jiku.ErrNotFound) {
		log.Warn("requirement not found; skipping")
		return nil
	}
	if err != nil {
		return err
	}
	is, err := e.fetchIssue(ctx, p, l)
	if errors.Is(err, github.ErrNotFound) {
		log.Warn("issue not found (deleted?); skipping")
		return nil
	}
	if err != nil {
		return err
	}

	// R4: a requirement that stops being public stops syncing; one that is public again revives.
	if !req.Public() {
		if l.Unlinked {
			return nil
		}
		if _, err := e.gh(p).CreateComment(ctx, is.Repo, is.Number, text.Notice(
			"Este issue dejó de sincronizarse con Jiku: el requisito pasó a ser interno.")); err != nil {
			return err
		}
		l.Unlinked = true
		log.Info("requirement became internal: pair unlinked")
		return e.st.SaveLink(ctx, l)
	}
	if l.Unlinked {
		log.Info("requirement is public again: pair revived")
		l.Unlinked = false
		// Jiku wins every field on revival: GitHub's side is taken as "unchanged".
		l.Last = state.Snapshot{Title: is.Title, BodyHash: text.Hash(is.Body), GHOpen: is.Open(), GHStateReason: is.StateReason}
	}

	item, err := e.ensureItem(ctx, p, l, is, log)
	if err != nil {
		return err
	}
	v := e.view(ctx, p, is, item, l.Last, log)
	last := l.Last
	next := last
	issueUpd := github.IssueInput{}
	issueChanged := false
	jikuUpd := map[string]any{}
	var notices []string
	var fieldWrites []func() error

	// Title.
	switch decide(req.Title != last.Title, is.Title != last.Title, req.Title != is.Title, req.UpdatedAt, is.UpdatedAt) {
	case sideJiku:
		issueUpd.Title, issueChanged = strp(req.Title), true
	case sideGitHub:
		jikuUpd["title"] = is.Title
	}
	// Description.
	jh, gh := text.Hash(req.Description), text.Hash(is.Body)
	switch decide(jh != last.BodyHash, gh != last.BodyHash, jh != gh, req.UpdatedAt, is.UpdatedAt) {
	case sideJiku:
		issueUpd.Body, issueChanged = strp(text.IssueBody(req.Description, req.ID)), true
		next.BodyHash = jh
	case sideGitHub:
		jikuUpd["description"] = text.Normalize(is.Body)
		next.BodyHash = gh
	default:
		next.BodyHash = jh
	}

	// Type: the issue type is written apart, because orgs without issue types refuse it.
	jt := req.TypeOrEmpty()
	var typeWrite *string
	// A GitHub side with nothing mapped only differs when Jiku's type just changed: otherwise an
	// org without issue types would be written to on every sync.
	typeDiffer := v.typeView != jt && (v.typeView != "" || jt != last.JikuType)
	switch decide(jt != last.JikuType, v.typeView != last.GHType && v.typeView != "", typeDiffer, req.UpdatedAt, is.UpdatedAt) {
	case sideJiku:
		if name, set := p.IssueTypeForJiku(jt); set && name != is.IssueType {
			typeWrite = &name
		}
	case sideGitHub:
		if v.typeView != "" {
			jikuUpd["type"] = v.typeView
			jt = v.typeView
		}
	}

	// Priority (a Project field).
	finalPriority := req.Priority
	finalPriorityOpt := v.priority
	if p.Fields != nil && p.Fields.Priority != nil && item != nil {
		gp, gok := p.PriorityFromOption(v.priority)
		jChanged := req.Priority != last.JikuPriority
		// An empty Jiku side in the snapshot means the issue was just created from Jiku: Jiku is the
		// origin and wins, whatever the Project's workflows did to the new item meanwhile.
		gPriChanged := v.priority != last.GHPriority && gok && last.JikuPriority != ""
		switch decide(jChanged, gPriChanged, (gok && gp != req.Priority) || (!gok && jChanged), req.UpdatedAt, v.priorityAt) {
		case sideJiku:
			if opt, ok := p.OptionForPriority(req.Priority); ok && opt != v.priority {
				field := p.Fields.Priority.Field
				fieldWrites = append(fieldWrites, func() error { return e.setField(ctx, p, item, field, opt) })
				finalPriorityOpt = opt
			}
		case sideGitHub:
			jikuUpd["priority"] = gp
			finalPriority = gp
		}
	}

	// State: open/closed on the issue, the open stages on the Project's Status.
	tg := e.target(p, v, last)
	openChanged := is.Open() != last.GHOpen || (!is.Open() && reasonClass(is.StateReason) != reasonClass(last.GHStateReason))
	statusByPerson := v.status != last.GHStatus && v.statusBy != AutomationLogin
	gChanged := openChanged || statusByPerson
	jChanged := req.State != last.JikuState
	gAt := v.statusAt
	if openChanged && is.UpdatedAt.After(gAt) {
		gAt = is.UpdatedAt
	}
	finalState := req.State
	finalOpen, finalReason, finalStatus := is.Open(), is.StateReason, v.status
	if tg.unmapped != "" && statusByPerson {
		notices = append(notices, fmt.Sprintf(
			"La columna «%s» de GitHub no tiene un estado de Jiku configurado (movido por @%s). El requisito sigue en «%s».",
			tg.unmapped, v.statusBy, req.State))
	}
	// A card with no column on an open issue says nothing (e.g. the Project's options were
	// replaced and every value was wiped): it is put back where Jiku's state says.
	_, jikuHasOption := p.OptionForState(req.State)
	emptyCard := item != nil && is.Open() && v.status == "" && jikuHasOption
	differ := (tg.known && tg.state != req.State) || (!tg.known && jChanged) || emptyCard
	if last.JikuState == "" {
		gChanged = false // just exported: Jiku is the origin (see priority above)
	}
	switch decide(jChanged, gChanged && tg.known, differ, req.UpdatedAt, gAt) {
	case sideJiku:
		wantOpen := !terminal(req.State)
		switch {
		case wantOpen && !is.Open():
			issueUpd.State, issueChanged = strp("open"), true
			finalOpen, finalReason = true, "reopened"
		case !wantOpen:
			want := "completed"
			if req.State == "cancelado" {
				want = "not_planned"
			}
			if is.Open() || reasonClass(is.StateReason) != want {
				issueUpd.State, issueUpd.StateReason, issueChanged = strp("closed"), strp(want), true
				finalOpen, finalReason = false, want
			}
		}
		if opt, set := p.OptionForState(req.State); set && item != nil && opt != v.status {
			field := p.Fields.Status.Field
			fieldWrites = append(fieldWrites, func() error { return e.setField(ctx, p, item, field, opt) })
			finalStatus = opt
		}
	case sideGitHub:
		jikuUpd["state"] = tg.state
		finalState = tg.state
		if tg.state == "resuelto" && jt == "incidencia" {
			who := is.ClosedBy
			if who == "" {
				who = v.statusBy
			}
			jikuUpd["resolutionType"] = "otro"
			jikuUpd["resolutionConclusion"] = fmt.Sprintf("Cerrado en GitHub por @%s: %s", who, is.HTMLURL)
		}
		if tg.comment != "" {
			notices = append(notices, text.Expand(tg.comment, v.statusBy, v.status))
		}
		// The card may not say the same (a reopened issue still sitting in Done): align it.
		if cur, ok := p.StateFromOption(v.status); item != nil && (!ok || cur.State != tg.state) && !terminal(tg.state) {
			if opt, set := p.OptionForState(tg.state); set && opt != v.status {
				field := p.Fields.Status.Field
				fieldWrites = append(fieldWrites, func() error { return e.setField(ctx, p, item, field, opt) })
				finalStatus = opt
			}
		}
		if tg.closeIssue && is.Open() {
			want := "completed"
			if tg.state == "cancelado" {
				want = "not_planned"
			}
			issueUpd.State, issueUpd.StateReason, issueChanged = strp("closed"), strp(want), true
			finalOpen, finalReason = false, want
		}
	}

	// Writes: Jiku first (its validation can refuse), then GitHub.
	if len(jikuUpd) > 0 {
		if err := e.jk.EditRequirement(ctx, req.ID, jikuUpd); err != nil {
			return fmt.Errorf("editing requirement %d: %w", req.ID, err)
		}
		log.Info("jiku updated from github", "fields", keys(jikuUpd))
	}
	if issueChanged {
		if _, err := e.gh(p).UpdateIssue(ctx, is.Repo, is.Number, issueUpd); err != nil {
			return fmt.Errorf("updating %s#%d: %w", is.Repo, is.Number, err)
		}
		log.Info("github updated from jiku", "fields", issueFields(issueUpd))
	}
	ghTypeAfter := v.typeView
	if typeWrite != nil {
		if _, err := e.gh(p).UpdateIssue(ctx, is.Repo, is.Number, github.IssueInput{Type: typeWrite}); err != nil {
			log.Warn("could not set the issue type", "type", *typeWrite, "error", err)
		} else {
			ghTypeAfter, _ = p.TypeFromGithub(*typeWrite, is.Labels)
			log.Info("github issue type updated from jiku", "type", *typeWrite)
		}
	}
	for _, w := range fieldWrites {
		if err := w(); err != nil {
			log.Error("could not write a Project field", "error", err)
		}
	}
	if len(fieldWrites) > 0 {
		log.Info("project fields updated from jiku", "status", finalStatus, "priority", finalPriorityOpt)
	}
	for _, n := range notices {
		if _, err := e.jk.CreateComment(ctx, req.ID, n, "internal"); err != nil {
			log.Warn("could not leave an internal comment", "error", err)
		}
	}

	// The new snapshot: both sides as they are after this sync.
	if t, ok := jikuUpd["title"].(string); ok {
		next.Title = t
	} else {
		next.Title = req.Title
	}
	next.JikuType, next.GHType = jt, ghTypeAfter
	next.JikuPriority, next.GHPriority = finalPriority, finalPriorityOpt
	next.JikuState = finalState
	next.GHOpen, next.GHStateReason, next.GHStatus = finalOpen, finalReason, finalStatus
	if !terminal(finalState) {
		next.LastOpenState = finalState
	}
	l.Last = next

	if err := e.syncComments(ctx, p, l, req, is, opts); err != nil {
		// The fields are synced: keep that progress before reporting the comments' failure.
		_ = e.st.SaveLink(ctx, l)
		return err
	}
	return e.st.SaveLink(ctx, l)
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func issueFields(in github.IssueInput) []string {
	var out []string
	if in.Title != nil {
		out = append(out, "title")
	}
	if in.Body != nil {
		out = append(out, "body")
	}
	if in.State != nil {
		out = append(out, "state")
	}
	return out
}
