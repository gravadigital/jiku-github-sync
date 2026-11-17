package engine

import (
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

// The same board, with the tasks' states mapped onto the same Status field, and tasks turned on
// in both projects (8 has no Project: open/closed only).
const taskConfig = `
fieldMappings:
  board:
    status:
      field: Status
      toJiku: { "Inbox": analisis, "Backlog": planificacion, "Ready": en_cola, "In Progress": desarrollo, "Review": revision, "Done": resuelto }
      toGithub: { analisis: "Inbox", planificacion: "Backlog", en_cola: "Ready", desarrollo: "In Progress", revision: "Review", resuelto: "Done", cancelado: null }
    taskStatus:
      field: Status
      toJiku:
        "Inbox": backlog
        "Backlog": backlog
        "Ready": backlog
        "In Progress": activo
        "Blocked": { state: activo, comment: "Marcada como {option} en GitHub por @{login}" }
        "Review": en_revision
        "Done": finalizado
      toGithub: { backlog: "Backlog", activo: "In Progress", en_revision: "Review", finalizado: "Done", cancelado: null }
    priority:
      field: Priority
      map: { urgente: "Critical", alta: "High", media: "Medium", baja: "Low", sin_prioridad: null }
projects:
  - jikuProject: 7
    repos: [acme/api, acme/web]
    defaultRepo: acme/api
    githubProject: P
    fieldMapping: board
    tasks: { enabled: true, issueType: Task }
  - jikuProject: 8
    repos: [acme/plain]
    defaultRepo: acme/plain
    tasks: { enabled: true }
`

func newTaskHarness(t *testing.T, cfgText string) *harness {
	t.Helper()
	cfg, err := mapping.Parse([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	gh := newFakeGH(clk)
	gh.schema = schemaOf(map[string][]string{
		"Status":   {"Inbox", "Backlog", "Ready", "In Progress", "Blocked", "Review", "Done"},
		"Priority": {"Critical", "High", "Medium", "Low"},
	})
	gh.automationStatus = "Inbox"
	jk := newFakeJiku(clk)
	st := state.New(state.NewMem())
	e := New(Options{GitHub: gh, Jiku: jk, Store: st, Config: cfg, Now: clk.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	e.ItemSettle = 0
	h := &harness{t: t, ctx: t.Context(), clk: clk, gh: gh, jk: jk, st: st, e: e}
	for _, p := range []int64{7, 8} {
		if _, err := e.EnsureStarted(h.ctx, p); err != nil {
			t.Fatal(err)
		}
		if cfg.Project(p).TasksEnabled() {
			if _, err := e.EnsureTasksStarted(h.ctx, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	clk.tick()
	return h
}

func (h *harness) syncTask(id int64) {
	h.t.Helper()
	if err := h.e.HandleTask(h.ctx, id, false); err != nil {
		h.t.Fatalf("HandleTask(%d): %v", id, err)
	}
}

func (h *harness) taskLink(id int64) *state.Link {
	h.t.Helper()
	l, err := h.st.TaskLink(h.ctx, id)
	if err != nil {
		h.t.Fatalf("task link of %d: %v", id, err)
	}
	return l
}

func (h *harness) subIssueOf(taskID int64) *github.Issue {
	h.t.Helper()
	l := h.taskLink(taskID)
	is, err := h.gh.GetIssue(h.ctx, l.Repo, l.Number)
	if err != nil {
		h.t.Fatal(err)
	}
	return is
}

func (h *harness) task(id int64) *jiku.Task {
	t, _ := h.jk.GetTask(h.ctx, id)
	return t
}

// quietTask asserts a second sync of the task writes nothing anywhere.
func (h *harness) quietTask(id int64) {
	h.t.Helper()
	h.gh.takeWrites()
	h.jk.takeWrites()
	h.syncTask(id)
	if w := h.gh.takeWrites(); len(w) > 0 {
		h.t.Fatalf("expected no GitHub writes on a task resync, got %v", w)
	}
	if w := h.jk.takeWrites(); len(w) > 0 {
		h.t.Fatalf("expected no Jiku writes on a task resync, got %v", w)
	}
}

// syncedReq creates a public requirement and its issue.
func (h *harness) syncedReq(project int64, title string) (*jiku.Requirement, *github.Issue) {
	h.t.Helper()
	r := h.jk.newReq(project, title, "detalle", "funcionalidad", "public")
	h.syncReq(r.ID)
	return r, h.issueOf(r.ID)
}

// ---- T1: task -> sub-issue ----

func TestTaskOfSyncedRequirementBecomesSubIssue(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, parent := h.syncedReq(7, "Login")
	tk := h.jk.newTask(7, r.ID, "Formulario", "Campos y validación", "public")
	h.jk.editTask(tk.ID, func(x *jiku.Task) { x.Priority = "alta" })
	h.syncTask(tk.ID)

	is := h.subIssueOf(tk.ID)
	if is.Repo != parent.Repo || is.ParentKey() != parent.Repo+"#"+strconv.Itoa(parent.Number) {
		t.Fatalf("the sub-issue must be under the requirement's issue, in its repo: %+v", is)
	}
	if id, ok := text.TaskID(is.Body); !ok || id != tk.ID || text.Normalize(is.Body) != "Campos y validación" {
		t.Fatalf("body must carry the description and the task marker: %q", is.Body)
	}
	if is.IssueType != "Task" {
		t.Fatalf("tasks.issueType must be set, got %q", is.IssueType)
	}
	// The Project's workflow put the new card in Inbox, which taskStatus also reads as backlog: it
	// is left there, as with requirements.
	if tg, ok := h.e.Config().Project(7).TaskStatus().Target(h.status(is.NodeID)); !ok || tg.State != "backlog" {
		t.Fatalf("the card must say the task's state (backlog), got %q", h.status(is.NodeID))
	}
	it, _ := h.gh.ItemForIssue(h.ctx, is.NodeID, "P")
	if it.Values["Priority"].Option != "High" {
		t.Fatalf("priority not exported: %+v", it.Values)
	}
	if _, err := h.st.LinkByIssue(h.ctx, is.NodeID); err == nil {
		t.Fatal("a sub-issue must never be linked as a requirement")
	}
	h.quietTask(tk.ID)
	h.quiet(r.ID)
}

func TestTasksThatStayInJiku(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, _ := h.syncedReq(7, "Login")
	internal := h.jk.newReq(7, "Interno", "", "otro", "internal")
	h.syncReq(internal.ID)
	h.gh.takeWrites()
	for _, tk := range []*jiku.Task{
		h.jk.newTask(7, 0, "Sin requisito", "", "public"),
		h.jk.newTask(7, r.ID, "Interna", "", "internal"),
		h.jk.newTask(7, internal.ID, "De un requisito interno", "", "public"),
	} {
		h.syncTask(tk.ID)
	}
	if w := h.gh.takeWrites(); len(w) > 0 {
		t.Fatalf("no task above may reach GitHub, got %v", w)
	}
}

func TestRequirementBringsItsTasksWhenItIsSynced(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r := h.jk.newReq(7, "Todavía interno", "", "otro", "internal")
	tk := h.jk.newTask(7, r.ID, "Tarea pública", "", "public")
	h.syncReq(r.ID)
	h.syncTask(tk.ID)
	if _, err := h.st.TaskLink(h.ctx, tk.ID); err == nil {
		t.Fatal("a task of an internal requirement must not be synced")
	}
	h.jk.editReq(r.ID, func(x *jikuReq) { x.VisibilityLevel = "public" })
	if err := h.e.HandleRequirement(h.ctx, r.ID, true); err != nil {
		t.Fatal(err)
	}
	is := h.subIssueOf(tk.ID)
	if is.ParentNumber != h.issueOf(r.ID).Number {
		t.Fatalf("the task must follow its requirement to GitHub: %+v", is)
	}
	h.quietTask(tk.ID)
}

// ---- T2: sub-issue -> task ----

func TestSubIssueOfSyncedIssueBecomesTask(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, parent := h.syncedReq(7, "Login")
	sub := h.gh.newIssue("acme/web", "Pantalla de error", "Mostrar el motivo", "dev")
	h.gh.setParent(sub.NodeID, parent, "dev")
	h.syncIssue(sub)

	l, err := h.st.TaskLinkByIssue(h.ctx, sub.NodeID)
	if err != nil {
		t.Fatalf("the sub-issue must become a task: %v", err)
	}
	tk := h.task(l.TaskID)
	if tk.Req() != r.ID || !tk.Public() || tk.Title != "Pantalla de error" || tk.Desc() != "Mostrar el motivo" || tk.State != "backlog" {
		t.Fatalf("unexpected task: %+v", tk)
	}
	for _, rr := range h.jk.reqs {
		if rr.ID != r.ID {
			t.Fatalf("a sub-issue must not create a requirement, got %q", rr.Title)
		}
	}
	h.quietTask(tk.ID)
}

func TestNestedSubIssueIsIgnored(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, _ := h.syncedReq(7, "Login")
	tk := h.jk.newTask(7, r.ID, "Formulario", "", "public")
	h.syncTask(tk.ID)
	nested := h.gh.newIssue("acme/api", "Sub de sub", "", "dev")
	h.gh.setParent(nested.NodeID, h.subIssueOf(tk.ID), "dev")
	h.jk.takeWrites()
	h.syncIssue(nested)
	if w := h.jk.takeWrites(); len(w) > 0 {
		t.Fatalf("a sub-issue of a task's sub-issue must not reach Jiku, got %v", w)
	}
}

func TestWithoutTasksASubIssueIsARequirement(t *testing.T) {
	h := newHarness(t) // testConfig: tasks off
	r := h.jk.newReq(7, "Login", "", "otro", "public")
	h.syncReq(r.ID)
	sub := h.gh.newIssue("acme/api", "Hija", "", "dev")
	h.gh.setParent(sub.NodeID, h.issueOf(r.ID), "dev")
	id := h.importIssue(sub)
	if _, err := h.jk.GetRequirement(h.ctx, id); err != nil {
		t.Fatalf("with tasks off a sub-issue is imported as before: %v", err)
	}
}

// Decision 1: an issue already synced as a requirement that becomes a sub-issue stays a
// requirement, with an internal note left once.
func TestRequirementIssueAttachedAsSubIssueStaysRequirement(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	_, parent := h.syncedReq(7, "Épica")
	child, childIssue := h.syncedReq(7, "Ya era requisito")
	h.gh.setParent(childIssue.NodeID, parent, "dev")
	h.syncIssue(childIssue)
	h.syncIssue(childIssue)
	notes := 0
	for _, c := range h.jk.comms[jiku.ReqRef(child.ID)] {
		if c.VisibilityLevel == "internal" && strings.Contains(c.Body, "sub-issue") {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("expected exactly one internal note, got %d", notes)
	}
	if _, err := h.st.TaskLinkByIssue(h.ctx, childIssue.NodeID); err == nil {
		t.Fatal("it must not become a task")
	}
	h.quiet(child.ID)
}

// ---- T3/T4: fields, state and comments ----

func TestTaskFieldsAndStateBothWays(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, _ := h.syncedReq(7, "Login")
	tk := h.jk.newTask(7, r.ID, "Formulario", "v1", "public")
	h.syncTask(tk.ID)
	is := h.subIssueOf(tk.ID)

	// GitHub: title and card.
	h.gh.edit(is.NodeID, func(i *github.Issue) { i.Title = "Formulario de alta" })
	h.gh.setField(is.NodeID, "Status", "In Progress", "dev")
	h.syncTask(tk.ID)
	if got := h.task(tk.ID); got.Title != "Formulario de alta" || got.State != "activo" {
		t.Fatalf("GitHub's changes must reach the task: %+v", got)
	}
	h.quietTask(tk.ID)

	// Jiku: description and review.
	h.jk.editTask(tk.ID, func(x *jiku.Task) { d := "v2"; x.Description = &d; x.State = "en_revision" })
	h.syncTask(tk.ID)
	is = h.subIssueOf(tk.ID)
	if text.Normalize(is.Body) != "v2" || h.status(is.NodeID) != "Review" {
		t.Fatalf("Jiku's changes must reach the sub-issue: body=%q status=%q", is.Body, h.status(is.NodeID))
	}
	h.quietTask(tk.ID)

	// Closing the sub-issue finishes the task; cancelling the task closes it as not planned.
	h.gh.edit(is.NodeID, func(i *github.Issue) { i.State, i.StateReason = "closed", "completed" })
	h.syncTask(tk.ID)
	if got := h.task(tk.ID).State; got != "finalizado" {
		t.Fatalf("closing the sub-issue must finish the task, got %s", got)
	}
	h.jk.editTask(tk.ID, func(x *jiku.Task) { x.State = "cancelado" })
	h.syncTask(tk.ID)
	if is = h.subIssueOf(tk.ID); is.State != "closed" || is.StateReason != "not_planned" {
		t.Fatalf("a cancelled task closes as not planned: %s %s", is.State, is.StateReason)
	}
	h.quietTask(tk.ID)
	// Reopened on GitHub: back to the last open state.
	h.gh.edit(is.NodeID, func(i *github.Issue) { i.State, i.StateReason = "open", "reopened" })
	h.syncTask(tk.ID)
	if got := h.task(tk.ID).State; got != "en_revision" {
		t.Fatalf("a reopened sub-issue goes back to the last open state, got %s", got)
	}
	h.quietTask(tk.ID)
}

func TestTaskWithoutProjectSyncsOpenClosed(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, _ := h.syncedReq(8, "Plano")
	tk := h.jk.newTask(8, r.ID, "Tarea", "", "public")
	h.syncTask(tk.ID)
	is := h.subIssueOf(tk.ID)
	if is.Repo != "acme/plain" || is.IssueType != "" {
		t.Fatalf("unexpected sub-issue: %+v", is)
	}
	h.jk.editTask(tk.ID, func(x *jiku.Task) { x.State = "finalizado" })
	h.syncTask(tk.ID)
	if is = h.subIssueOf(tk.ID); is.State != "closed" || is.StateReason != "completed" {
		t.Fatalf("finalizado closes the sub-issue: %s %s", is.State, is.StateReason)
	}
	h.quietTask(tk.ID)
}

func TestTaskCommentsBothWays(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, _ := h.syncedReq(7, "Login")
	tk := h.jk.newTask(7, r.ID, "Formulario", "", "public")
	h.syncTask(tk.ID)
	is := h.subIssueOf(tk.ID)

	h.jk.personCommentOn(jiku.TaskRef(tk.ID), "Listo para revisar", "public")
	h.jk.personCommentOn(jiku.TaskRef(tk.ID), "nota interna", "internal")
	h.gh.comment(is.NodeID, "dev", "Lo miro hoy")
	h.syncTask(tk.ID)

	gcs, _ := h.gh.ListComments(h.ctx, is.Repo, is.Number)
	var mirrored []string
	for _, c := range gcs {
		mirrored = append(mirrored, firstLine(c.Body))
	}
	if len(gcs) != 2 || !strings.Contains(gcs[1].Body, "**Ana Pérez** comentó en Jiku:") {
		t.Fatalf("the public task comment must reach GitHub (and only it): %q", mirrored)
	}
	jcs, _ := h.jk.ListComments(h.ctx, jiku.TaskRef(tk.ID))
	found := false
	for _, c := range jcs {
		if strings.HasPrefix(c.Body, "**@dev** comentó en [GitHub]") {
			found = c.VisibilityLevel == "public"
		}
	}
	if !found {
		t.Fatal("the GitHub comment must reach the task as a public comment")
	}
	h.quietTask(tk.ID)
}

// ---- T5: hierarchy ----

func TestTaskMovedInJikuMovesTheSubIssue(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	a, _ := h.syncedReq(7, "A")
	b, issueB := h.syncedReq(7, "B")
	tk := h.jk.newTask(7, a.ID, "Tarea", "", "public")
	h.syncTask(tk.ID)
	h.jk.editTask(tk.ID, func(x *jiku.Task) { x.RequirementID = &b.ID })
	h.syncTask(tk.ID)
	if is := h.subIssueOf(tk.ID); is.ParentNumber != issueB.Number {
		t.Fatalf("the sub-issue must move under B's issue, parent=%s", is.ParentKey())
	}
	if l := h.taskLink(tk.ID); l.ReqID != b.ID || l.ParentNode != issueB.NodeID {
		t.Fatalf("the pair must remember the new parent: %+v", l)
	}
	h.quietTask(tk.ID)
}

func TestSubIssueMovedOnGitHubMovesTheTask(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	a, _ := h.syncedReq(7, "A")
	b, issueB := h.syncedReq(7, "B")
	tk := h.jk.newTask(7, a.ID, "Tarea", "", "public")
	h.syncTask(tk.ID)
	h.gh.setParent(h.subIssueOf(tk.ID).NodeID, issueB, "dev")
	h.syncTask(tk.ID)
	if got := h.task(tk.ID).Req(); got != b.ID {
		t.Fatalf("the task must move to B, got requirement %d", got)
	}
	h.quietTask(tk.ID)
}

func TestMovedOnBothSidesTheLatestWins(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	a, _ := h.syncedReq(7, "A")
	b, _ := h.syncedReq(7, "B")
	c, issueC := h.syncedReq(7, "C")
	tk := h.jk.newTask(7, a.ID, "Tarea", "", "public")
	h.syncTask(tk.ID)
	h.jk.editTask(tk.ID, func(x *jiku.Task) { x.RequirementID = &b.ID })
	h.gh.setParent(h.subIssueOf(tk.ID).NodeID, issueC, "dev") // later
	h.syncTask(tk.ID)
	if got := h.task(tk.ID).Req(); got != c.ID {
		t.Fatalf("GitHub moved it last: the task must end under C, got %d", got)
	}
	if is := h.subIssueOf(tk.ID); is.ParentNumber != issueC.Number {
		t.Fatalf("the sub-issue must stay under C, got %s", is.ParentKey())
	}
	h.quietTask(tk.ID)
}

// Decision 2: a sub-issue detached on GitHub stops syncing and the task stays where it is; put
// back under a synced issue, it syncs again.
func TestDetachedSubIssueUnlinksAndReattachedRevives(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	a, _ := h.syncedReq(7, "A")
	b, issueB := h.syncedReq(7, "B")
	tk := h.jk.newTask(7, a.ID, "Tarea", "", "public")
	h.syncTask(tk.ID)
	sub := h.subIssueOf(tk.ID)
	h.gh.setParent(sub.NodeID, nil, "dev")
	h.syncTask(tk.ID)
	l := h.taskLink(tk.ID)
	if !l.Unlinked || l.UnlinkedBy != "github" || h.task(tk.ID).Req() != a.ID {
		t.Fatalf("detached: the pair unlinks and the task stays under A: %+v req=%d", l, h.task(tk.ID).Req())
	}
	notes := 0
	for _, c := range h.jk.comms[jiku.TaskRef(tk.ID)] {
		if c.VisibilityLevel == "internal" {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("expected one internal note on the task, got %d", notes)
	}
	// Jiku edits do not revive it: only GitHub, which unlinked it.
	h.jk.editTask(tk.ID, func(x *jiku.Task) { x.Title = "Otro título" })
	h.gh.takeWrites()
	h.syncTask(tk.ID)
	if w := h.gh.takeWrites(); len(w) > 0 {
		t.Fatalf("an unlinked pair writes nothing, got %v", w)
	}
	h.gh.setParent(sub.NodeID, issueB, "dev")
	h.syncTask(tk.ID)
	if l := h.taskLink(tk.ID); l.Unlinked || h.task(tk.ID).Req() != b.ID {
		t.Fatalf("reattached under B: the pair revives and the task moves to B: %+v", l)
	}
	if is := h.subIssueOf(tk.ID); is.Title != "Otro título" {
		t.Fatalf("on revival Jiku wins the fields, got %q", is.Title)
	}
	h.quietTask(tk.ID)
}

func TestTaskMadeInternalUnlinksAndPublicRevives(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, issueR := h.syncedReq(7, "A")
	tk := h.jk.newTask(7, r.ID, "Tarea", "", "public")
	h.syncTask(tk.ID)
	h.jk.editTask(tk.ID, func(x *jiku.Task) { x.VisibilityLevel = "internal" })
	h.syncTask(tk.ID)
	if l := h.taskLink(tk.ID); !l.Unlinked || l.UnlinkedBy != "jiku" {
		t.Fatalf("an internal task unlinks: %+v", l)
	}
	if is := h.subIssueOf(tk.ID); is.ParentNumber != issueR.Number {
		t.Fatal("the sub-issue keeps its parent")
	}
	h.jk.editTask(tk.ID, func(x *jiku.Task) { x.VisibilityLevel = "public" })
	h.syncTask(tk.ID)
	if l := h.taskLink(tk.ID); l.Unlinked {
		t.Fatalf("public again: revived: %+v", l)
	}
	h.quietTask(tk.ID)
}

func TestTaskMovedToUnsyncedRequirementLeavesTheParent(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, _ := h.syncedReq(7, "A")
	other := h.jk.newReq(7, "Interno", "", "otro", "internal")
	tk := h.jk.newTask(7, r.ID, "Tarea", "", "public")
	h.syncTask(tk.ID)
	h.jk.editTask(tk.ID, func(x *jiku.Task) { x.RequirementID = &other.ID })
	h.syncTask(tk.ID)
	if l := h.taskLink(tk.ID); !l.Unlinked || l.UnlinkedBy != "jiku" {
		t.Fatalf("moved to an unsynced requirement: unlinked: %+v", l)
	}
	if is := h.subIssueOf(tk.ID); is.HasParent() {
		t.Fatalf("the sub-issue no longer belongs under A: %s", is.ParentKey())
	}
}

func TestRequirementMadeInternalUnlinksItsTasks(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, issueR := h.syncedReq(7, "A")
	tk := h.jk.newTask(7, r.ID, "Tarea", "", "public")
	h.syncTask(tk.ID)
	h.jk.editReq(r.ID, func(x *jikuReq) { x.VisibilityLevel = "internal" })
	h.syncReq(r.ID)
	if l := h.taskLink(tk.ID); !l.Unlinked || l.UnlinkedBy != "jiku" {
		t.Fatalf("its tasks unlink with it: %+v", l)
	}
	if is := h.subIssueOf(tk.ID); is.ParentNumber != issueR.Number {
		t.Fatal("the sub-issue stays under the (no longer synced) issue")
	}
	// Public again: the requirement revives, and its task with it.
	h.jk.editReq(r.ID, func(x *jikuReq) { x.VisibilityLevel = "public" })
	h.syncReq(r.ID)
	if l := h.taskLink(tk.ID); l.Unlinked {
		t.Fatalf("the task revives with its requirement: %+v", l)
	}
	h.quietTask(tk.ID)
}

// ---- recovery and compatibility ----

func TestInterruptedTaskExportIsRecovered(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, parent := h.syncedReq(7, "A")
	tk := h.jk.newTask(7, r.ID, "Tarea", "", "public")
	// The sub-issue was created, but the connector stopped before linking it (and before putting
	// it under its parent).
	if err := h.st.SetPendingTask(h.ctx, tk.ID, state.Pending{At: h.clk.now(), Title: tk.Title}); err != nil {
		t.Fatal(err)
	}
	h.gh.CreateIssue(h.ctx, "acme/api", github.IssueInput{Title: strp("Tarea"), Body: strp(text.SubIssueBody("", tk.ID))})
	h.gh.takeWrites()
	h.syncTask(tk.ID)
	creates := 0
	for _, w := range h.gh.takeWrites() {
		if strings.HasPrefix(w, "create issue") {
			creates++
		}
	}
	if creates != 0 {
		t.Fatal("the interrupted export must be recovered, not repeated")
	}
	if is := h.subIssueOf(tk.ID); is.ParentNumber != parent.Number {
		t.Fatalf("the recovered sub-issue must end under its parent: %s", is.ParentKey())
	}
	h.quietTask(tk.ID)
}

func TestTasksBeforeTheyWereTurnedOnWaitForTheInitialSync(t *testing.T) {
	h := newTaskHarness(t, taskConfig)
	r, parent := h.syncedReq(7, "A")
	old := h.jk.newTask(7, r.ID, "Vieja", "", "public")
	h.jk.editTask(old.ID, func(x *jiku.Task) { x.CreatedAt = x.CreatedAt.Add(-time.Hour) })
	h.syncTask(old.ID)
	oldSub := h.gh.newIssue("acme/api", "Sub vieja", "", "dev")
	h.gh.edit(oldSub.NodeID, func(i *github.Issue) { i.CreatedAt = i.CreatedAt.Add(-time.Hour) })
	h.gh.setParent(oldSub.NodeID, parent, "dev")
	h.syncIssue(oldSub)
	if _, err := h.st.TaskLink(h.ctx, old.ID); err == nil {
		t.Fatal("a task older than tasks being turned on is the initial sync's job")
	}
	if _, err := h.st.TaskLinkByIssue(h.ctx, oldSub.NodeID); err == nil {
		t.Fatal("a sub-issue older than tasks being turned on is the initial sync's job")
	}
	if _, err := h.st.LinkByIssue(h.ctx, oldSub.NodeID); err == nil {
		t.Fatal("and it must not become a requirement either")
	}
}
