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

// model is what differs between a requirement's and a task's states: their names and the Status
// mapping they use. Everything else about a pair is synced the same way.
type model struct {
	task bool
	// status is the Status mapping (nil: only open/closed is synced).
	status *mapping.StatusMapping
	// done and cancelled are the closing states; reopen the default open state.
	done, cancelled, reopen string
}

func (e *Engine) model(p *mapping.Project, task bool) model {
	if task {
		return model{task: true, status: p.TaskStatus(), done: "finalizado", cancelled: "cancelado", reopen: "backlog"}
	}
	m := model{done: "resuelto", cancelled: "cancelado", reopen: "analisis"}
	if p.Fields != nil {
		m.status = p.Fields.Status
	}
	return m
}

func (m model) terminal(state string) bool { return state == m.done || state == m.cancelled }

// closedAs is the state a closed issue means.
func (m model) closedAs(reason string) string {
	if reasonClass(reason) == "not_planned" {
		return m.cancelled
	}
	return m.done
}

// jikuSide is the Jiku half of a pair, requirement or task, in the terms the engine compares.
type jikuSide struct {
	ref         jiku.Ref
	title, desc string
	state       string
	priority    string
	typ         string // requirements only
	updatedAt   time.Time
	attachments []jiku.Attachment
}

func reqSide(r *jiku.Requirement) *jikuSide {
	return &jikuSide{ref: jiku.ReqRef(r.ID), title: r.Title, desc: r.Description, state: r.State,
		priority: r.Priority, typ: r.TypeOrEmpty(), updatedAt: r.UpdatedAt, attachments: r.Attachments}
}

func taskSide(t *jiku.Task) *jikuSide {
	return &jikuSide{ref: jiku.TaskRef(t.ID), title: t.Title, desc: t.Desc(), state: t.State,
		priority: t.Priority, updatedAt: t.UpdatedAt, attachments: t.Attachments}
}

// editJiku writes fields to the requirement or task.
func (e *Engine) editJiku(ctx context.Context, ref jiku.Ref, fields map[string]any) error {
	if ref.Kind == jiku.KindTask {
		return e.jk.EditTask(ctx, ref.ID, fields)
	}
	return e.jk.EditRequirement(ctx, ref.ID, fields)
}

// issueBody is the body the connector writes on the pair's issue: the text and its marker.
func issueBody(l *state.Link, body string) string {
	if l.IsTask() {
		return text.SubIssueBody(body, l.TaskID)
	}
	return text.IssueBody(body, l.ReqID)
}

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

func (e *Engine) view(ctx context.Context, p *mapping.Project, m model, is *github.Issue, item *github.Item, last state.Snapshot, log *slog.Logger) *ghView {
	v := &ghView{issue: is, item: item}
	if item != nil && p.Fields != nil {
		if s := m.status; s != nil {
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
	if !m.task {
		v.typeView, _ = p.TypeFromGithub(is.IssueType, is.Labels)
	}
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
func (e *Engine) ensureItem(ctx context.Context, p *mapping.Project, m model, l *state.Link, is *github.Issue, log *slog.Logger) (*github.Item, error) {
	// A task's sub-issue goes to the Project only when its states are mapped (taskStatus).
	if p.GithubProject == "" || (m.task && m.status == nil) {
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
		settled := item != nil && (m.status == nil || item.Values[m.status.Field].Option != "")
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

func (e *Engine) target(m model, v *ghView, last state.Snapshot) ghTarget {
	is := v.issue
	reopened := is.Open() && !last.GHOpen && last.JikuState != ""
	reopenState := last.LastOpenState
	if reopenState == "" {
		reopenState = m.reopen
	}
	if !is.Open() {
		return ghTarget{state: m.closedAs(is.StateReason), known: true}
	}
	if v.status == "" {
		if reopened {
			return ghTarget{state: reopenState, known: true}
		}
		return ghTarget{}
	}
	t, ok := m.status.Target(v.status)
	if !ok {
		if reopened {
			return ghTarget{state: reopenState, known: true}
		}
		return ghTarget{unmapped: v.status}
	}
	if m.terminal(t.State) {
		if reopened {
			return ghTarget{state: reopenState, known: true}
		}
		return ghTarget{state: t.State, known: true, closeIssue: true}
	}
	return ghTarget{state: t.State, comment: t.Comment, known: true}
}

// SyncLink syncs one pair, field by field, then its comments.
func (e *Engine) SyncLink(ctx context.Context, l *state.Link, opts SyncOptions) error {
	if l.IsTask() {
		return e.syncTask(ctx, l, opts)
	}
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
		if err := e.st.SaveLink(ctx, l); err != nil {
			return err
		}
		return e.followRequirement(ctx, p, l)
	}
	revived := false
	if l.Unlinked {
		log.Info("requirement is public again: pair revived")
		l.Unlinked = false
		revived = true
		// Jiku wins every field on revival: GitHub's side is taken as "unchanged".
		l.Last = state.Snapshot{Title: is.Title, GHOpen: is.Open(), GHStateReason: is.StateReason}
		l.Last.Bodies("", text.Hash(is.Body))
	}
	e.noteSubIssue(ctx, p, l, is, log)

	if err := e.syncFields(ctx, p, e.model(p, false), l, reqSide(req), is, opts, log); err != nil {
		return err
	}
	if revived {
		return e.followRequirement(ctx, p, l)
	}
	return nil
}

// noteSubIssue leaves, once, an internal note on a requirement whose issue is a sub-issue of
// another synced issue: Jiku cannot turn a requirement into a task, so it stays a requirement.
func (e *Engine) noteSubIssue(ctx context.Context, p *mapping.Project, l *state.Link, is *github.Issue, log *slog.Logger) {
	if !p.TasksEnabled() || !is.HasParent() || l.SubIssueNoted {
		return
	}
	parent, err := e.linkOfIssue(ctx, p, is.ParentRepo, is.ParentNumber)
	if err != nil || parent == nil || parent.IsTask() || parent.Unlinked {
		return
	}
	msg := fmt.Sprintf("En GitHub, %s pasó a ser sub-issue de %s (el requisito %d). Sigue sincronizándose como requisito: "+
		"Jiku no convierte un requisito en tarea.", is.HTMLURL, parent.IssueKey(), parent.ReqID)
	if _, err := e.jk.CreateComment(ctx, jiku.ReqRef(l.ReqID), msg, "internal", nil); err != nil {
		log.Warn("could not leave an internal comment", "error", err)
		return
	}
	l.SubIssueNoted = true
	log.Warn("the issue of a requirement is a sub-issue of another synced issue: it stays a requirement", "parent", parent.IssueKey())
}

// syncFields syncs title, description, type, priority and state of a pair, then its comments,
// and stores the pair.
func (e *Engine) syncFields(ctx context.Context, p *mapping.Project, m model, l *state.Link, js *jikuSide, is *github.Issue, opts SyncOptions, log *slog.Logger) error {
	item, err := e.ensureItem(ctx, p, m, l, is, log)
	if err != nil {
		return err
	}
	v := e.view(ctx, p, m, is, item, l.Last, log)
	last := l.Last
	next := last
	issueUpd := github.IssueInput{}
	issueChanged := false
	jikuUpd := map[string]any{}
	var notices []string
	var fieldWrites []func() error

	// Title.
	switch decide(js.title != last.Title, is.Title != last.Title, js.title != is.Title, js.updatedAt, is.UpdatedAt) {
	case sideJiku:
		issueUpd.Title, issueChanged = strp(js.title), true
	case sideGitHub:
		jikuUpd["title"] = is.Title
	}
	// Description. Each side holds its own form of the attachments, so each side is compared with
	// its own snapshot; when neither moved they are taken as equal.
	jh, gh := text.Hash(js.desc), text.Hash(is.Body)
	jBodyChanged, gBodyChanged := jh != last.JikuBody(), gh != last.GHBody()
	next.Bodies(jh, gh)
	switch decide(jBodyChanged, gBodyChanged, jBodyChanged || gBodyChanged, js.updatedAt, is.UpdatedAt) {
	case sideJiku:
		body, err := e.toGitHub(ctx, p, l, is.Repo, js.desc, js.attachments, false)
		if err != nil {
			return err
		}
		full := issueBody(l, body)
		issueUpd.Body, issueChanged = &full, true
		next.Bodies(jh, text.Hash(full))
	case sideGitHub:
		desc, files, err := e.toJiku(ctx, p, l, is.Body, func() (string, error) { return e.gh(p).IssueHTML(ctx, is.Repo, is.Number) }, js.attachments)
		if err != nil {
			return err
		}
		desc = text.Normalize(desc)
		jikuUpd["description"] = desc
		if len(files) > 0 {
			// The full set: what the requirement or task had, plus the new files.
			jikuUpd["fileIds"] = append(jiku.FileIDs(js.attachments), files...)
		}
		next.Bodies(text.Hash(desc), gh)
	}

	// Type (requirements only): the issue type is written apart, because orgs without issue types
	// refuse it.
	jt := js.typ
	var typeWrite *string
	if !m.task {
		// A GitHub side with nothing mapped only differs when Jiku's type just changed: otherwise an
		// org without issue types would be written to on every sync.
		typeDiffer := v.typeView != jt && (v.typeView != "" || jt != last.JikuType)
		switch decide(jt != last.JikuType, v.typeView != last.GHType && v.typeView != "", typeDiffer, js.updatedAt, is.UpdatedAt) {
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
	}

	// Priority (a Project field).
	finalPriority := js.priority
	finalPriorityOpt := v.priority
	if p.Fields != nil && p.Fields.Priority != nil && item != nil {
		gp, gok := p.PriorityFromOption(v.priority)
		jChanged := js.priority != last.JikuPriority
		// An empty Jiku side in the snapshot means the issue was just created from Jiku: Jiku is the
		// origin and wins, whatever the Project's workflows did to the new item meanwhile.
		gPriChanged := v.priority != last.GHPriority && gok && last.JikuPriority != ""
		switch decide(jChanged, gPriChanged, (gok && gp != js.priority) || (!gok && jChanged), js.updatedAt, v.priorityAt) {
		case sideJiku:
			if opt, ok := p.OptionForPriority(js.priority); ok && opt != v.priority {
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
	tg := e.target(m, v, last)
	openChanged := is.Open() != last.GHOpen || (!is.Open() && reasonClass(is.StateReason) != reasonClass(last.GHStateReason))
	statusByPerson := v.status != last.GHStatus && v.statusBy != AutomationLogin
	gChanged := openChanged || statusByPerson
	jChanged := js.state != last.JikuState
	gAt := v.statusAt
	if openChanged && is.UpdatedAt.After(gAt) {
		gAt = is.UpdatedAt
	}
	finalState := js.state
	finalOpen, finalReason, finalStatus := is.Open(), is.StateReason, v.status
	if tg.unmapped != "" && statusByPerson {
		who := "El requisito sigue"
		if m.task {
			who = "La tarea sigue"
		}
		notices = append(notices, fmt.Sprintf(
			"La columna «%s» de GitHub no tiene un estado de Jiku configurado (movido por @%s). %s en «%s».",
			tg.unmapped, v.statusBy, who, js.state))
	}
	// A card with no column on an open issue says nothing (e.g. the Project's options were
	// replaced and every value was wiped): it is put back where Jiku's state says.
	_, jikuHasOption := m.status.Option(js.state)
	emptyCard := item != nil && is.Open() && v.status == "" && jikuHasOption
	differ := (tg.known && tg.state != js.state) || (!tg.known && jChanged) || emptyCard
	if last.JikuState == "" {
		gChanged = false // just exported: Jiku is the origin (see priority above)
	}
	switch decide(jChanged, gChanged && tg.known, differ, js.updatedAt, gAt) {
	case sideJiku:
		wantOpen := !m.terminal(js.state)
		switch {
		case wantOpen && !is.Open():
			issueUpd.State, issueChanged = strp("open"), true
			finalOpen, finalReason = true, "reopened"
		case !wantOpen:
			want := "completed"
			if js.state == m.cancelled {
				want = "not_planned"
			}
			if is.Open() || reasonClass(is.StateReason) != want {
				issueUpd.State, issueUpd.StateReason, issueChanged = strp("closed"), strp(want), true
				finalOpen, finalReason = false, want
			}
		}
		if opt, set := m.status.Option(js.state); set && item != nil && opt != v.status {
			field := m.status.Field
			fieldWrites = append(fieldWrites, func() error { return e.setField(ctx, p, item, field, opt) })
			finalStatus = opt
		}
	case sideGitHub:
		jikuUpd["state"] = tg.state
		finalState = tg.state
		if !m.task && tg.state == "resuelto" && jt == "incidencia" {
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
		if cur, ok := m.status.Target(v.status); item != nil && (!ok || cur.State != tg.state) && !m.terminal(tg.state) {
			if opt, set := m.status.Option(tg.state); set && opt != v.status {
				field := m.status.Field
				fieldWrites = append(fieldWrites, func() error { return e.setField(ctx, p, item, field, opt) })
				finalStatus = opt
			}
		}
		if tg.closeIssue && is.Open() {
			want := "completed"
			if tg.state == m.cancelled {
				want = "not_planned"
			}
			issueUpd.State, issueUpd.StateReason, issueChanged = strp("closed"), strp(want), true
			finalOpen, finalReason = false, want
		}
	}

	// Writes: Jiku first (its validation can refuse), then GitHub.
	if len(jikuUpd) > 0 {
		if err := e.editJiku(ctx, js.ref, jikuUpd); err != nil {
			return fmt.Errorf("editing %s %d: %w", js.ref.Kind, js.ref.ID, err)
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
		if _, err := e.jk.CreateComment(ctx, js.ref, n, "internal", nil); err != nil {
			log.Warn("could not leave an internal comment", "error", err)
		}
	}

	// The new snapshot: both sides as they are after this sync.
	if t, ok := jikuUpd["title"].(string); ok {
		next.Title = t
	} else {
		next.Title = js.title
	}
	next.JikuType, next.GHType = jt, ghTypeAfter
	next.JikuPriority, next.GHPriority = finalPriority, finalPriorityOpt
	next.JikuState = finalState
	next.GHOpen, next.GHStateReason, next.GHStatus = finalOpen, finalReason, finalStatus
	if !m.terminal(finalState) {
		next.LastOpenState = finalState
	}
	l.Last = next

	if err := e.syncComments(ctx, p, l, js.ref, is, opts); err != nil {
		// The fields are synced: keep that progress before reporting the comments' failure.
		_ = e.st.Save(ctx, l)
		return err
	}
	return e.st.Save(ctx, l)
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
