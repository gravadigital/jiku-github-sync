package engine

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

const testConfig = `
fieldMappings:
  board:
    status:
      field: Status
      toJiku:
        "Inbox": analisis
        "Backlog": planificacion
        "Ready": en_cola
        "In Progress": desarrollo
        "Blocked": { state: en_cola, comment: "Marcado como {option} en GitHub por @{login}" }
        "Review": revision
        "Done": resuelto
      toGithub:
        analisis: "Inbox"
        planificacion: "Backlog"
        en_cola: "Ready"
        desarrollo: "In Progress"
        revision: "Review"
        resuelto: "Done"
        cancelado: null
    priority:
      field: Priority
      map: { urgente: "Critical", alta: "High", media: "Medium", baja: "Low", sin_prioridad: null }
    type:
      sources: [issueType, label]
      issueType: { Bug: incidencia, Feature: funcionalidad, Task: funcionalidad }
      label: { "enhancement": mejora }
      toGithub: { incidencia: Bug, funcionalidad: Feature, mejora: Feature, otro: null }
projects:
  - jikuProject: 7
    repos: [acme/api, acme/web]
    defaultRepo: acme/api
    githubProject: P
    fieldMapping: board
  - jikuProject: 8
    repos: [acme/plain]
    defaultRepo: acme/plain
`

type harness struct {
	t   *testing.T
	ctx context.Context
	clk *clock
	gh  *fakeGH
	jk  *fakeJiku
	st  *state.Store
	e   *Engine
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg, err := mapping.Parse([]byte(testConfig))
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
	h := &harness{t: t, ctx: context.Background(), clk: clk, gh: gh, jk: jk, st: st, e: e}
	// The connector started watching both projects before anything in the tests happens.
	for _, p := range []int64{7, 8} {
		if _, err := e.EnsureStarted(h.ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	clk.tick()
	return h
}

func (h *harness) syncReq(id int64) {
	h.t.Helper()
	if err := h.e.HandleRequirement(h.ctx, id, false); err != nil {
		h.t.Fatalf("HandleRequirement(%d): %v", id, err)
	}
}

func (h *harness) syncIssue(is *github.Issue) {
	h.t.Helper()
	if err := h.e.HandleIssue(h.ctx, is.Repo, is.Number); err != nil {
		h.t.Fatalf("HandleIssue(%s#%d): %v", is.Repo, is.Number, err)
	}
}

func (h *harness) link(reqID int64) *state.Link {
	h.t.Helper()
	l, err := h.st.Link(h.ctx, reqID)
	if err != nil {
		h.t.Fatalf("link of %d: %v", reqID, err)
	}
	return l
}

func (h *harness) issueOf(reqID int64) *github.Issue {
	l := h.link(reqID)
	is, _ := h.gh.GetIssue(h.ctx, l.Repo, l.Number)
	return is
}

func (h *harness) status(node string) string {
	it, _ := h.gh.ItemForIssue(h.ctx, node, "P")
	if it == nil {
		return "<no item>"
	}
	return it.Values["Status"].Option
}

func (h *harness) req(id int64) string {
	r, _ := h.jk.GetRequirement(h.ctx, id)
	return r.State
}

// quiet asserts a second sync writes nothing anywhere: no echo, no loop.
func (h *harness) quiet(reqID int64) {
	h.t.Helper()
	h.gh.takeWrites()
	h.jk.takeWrites()
	h.syncReq(reqID)
	if w := h.gh.takeWrites(); len(w) > 0 {
		h.t.Fatalf("expected no GitHub writes on a resync, got %v", w)
	}
	if w := h.jk.takeWrites(); len(w) > 0 {
		h.t.Fatalf("expected no Jiku writes on a resync, got %v", w)
	}
}

func (h *harness) importIssue(is *github.Issue) int64 {
	h.t.Helper()
	h.syncIssue(is)
	l, err := h.st.LinkByIssue(h.ctx, is.NodeID)
	if err != nil {
		h.t.Fatalf("issue %d not linked: %v", is.Number, err)
	}
	return l.ReqID
}

// ---- R2: issue -> requirement ----

func TestImportIssueCreatesPublicRequirement(t *testing.T) {
	h := newHarness(t)
	is := h.gh.newIssue("acme/web", "Falla el login", "Pasos:\n\n1. entrar", "client")
	h.gh.edit(is.NodeID, func(i *github.Issue) { i.IssueType = "Bug" })
	id := h.importIssue(is)

	r, _ := h.jk.GetRequirement(h.ctx, id)
	if r.ProjectID != 7 || !r.Public() || r.Title != "Falla el login" || r.Description != "Pasos:\n\n1. entrar" || r.TypeOrEmpty() != "incidencia" {
		t.Fatalf("unexpected requirement: %+v type=%s", r, r.TypeOrEmpty())
	}
	if got := h.status(is.NodeID); got != "Inbox" {
		t.Fatalf("R3: the issue must be added to the Project (Inbox by the workflow), got %q", got)
	}
	h.quiet(id)
}

func TestImportCarriesStatusAndPrioritySetByAPerson(t *testing.T) {
	h := newHarness(t)
	is := h.gh.newIssue("acme/api", "Pedido", "texto", "client")
	h.gh.AddItem(h.ctx, "P", is.NodeID)
	h.gh.setField(is.NodeID, "Status", "Backlog", "client")
	h.gh.setField(is.NodeID, "Priority", "High", "client")
	id := h.importIssue(is)
	r, _ := h.jk.GetRequirement(h.ctx, id)
	if r.State != "planificacion" || r.Priority != "alta" || r.TypeOrEmpty() != "otro" {
		t.Fatalf("got state=%s priority=%s type=%s", r.State, r.Priority, r.TypeOrEmpty())
	}
	h.quiet(id)
}

func TestIssuesBeforeStartAndPullRequestsAreIgnored(t *testing.T) {
	h := newHarness(t)
	old := h.gh.newIssue("acme/api", "viejo", "", "client")
	h.gh.edit(old.NodeID, func(i *github.Issue) { i.CreatedAt = i.CreatedAt.Add(-time.Hour) })
	h.syncIssue(old)
	pr := h.gh.newIssue("acme/api", "PR", "", "dev")
	h.gh.edit(pr.NodeID, func(i *github.Issue) { i.IsPR = true })
	h.syncIssue(pr)
	other := h.gh.newIssue("other/repo", "no mapeado", "", "x")
	h.syncIssue(other)
	if w := h.jk.takeWrites(); len(w) > 0 {
		t.Fatalf("nothing should reach Jiku, got %v", w)
	}
}

// ---- R1: requirement -> issue ----

func TestExportPublicRequirement(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Nuevo reporte", "Detalle", "incidencia", "public")
	h.jk.editReq(r.ID, func(x *jikuReq) { x.State = "desarrollo"; x.Priority = "urgente" })
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)
	if is.Repo != "acme/api" || is.Title != "Nuevo reporte" || is.IssueType != "Bug" {
		t.Fatalf("unexpected issue: %+v", is)
	}
	if id, ok := text.RequirementID(is.Body); !ok || id != r.ID || text.Normalize(is.Body) != "Detalle" {
		t.Fatalf("body must carry the description and the marker: %q", is.Body)
	}
	if got := h.status(is.NodeID); got != "In Progress" {
		t.Fatalf("status must follow Jiku over the workflow's Inbox, got %q", got)
	}
	it, _ := h.gh.ItemForIssue(h.ctx, is.NodeID, "P")
	if it.Values["Priority"].Option != "Critical" {
		t.Fatalf("priority not exported: %+v", it.Values)
	}
	h.quiet(r.ID)
}

func TestInternalRequirementsAreNotExported(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Interno", "x", "", "internal")
	h.syncReq(r.ID)
	if w := h.gh.takeWrites(); len(w) > 0 {
		t.Fatalf("internal requirement reached GitHub: %v", w)
	}
}

func TestExportWithoutIssueTypesFallsBack(t *testing.T) {
	h := newHarness(t)
	h.gh.issueTypes = map[string]bool{} // the org has no issue types
	r := h.jk.newReq(7, "Sin tipos", "x", "incidencia", "public")
	h.syncReq(r.ID)
	if is := h.issueOf(r.ID); is.IssueType != "" {
		t.Fatalf("type should have been dropped, got %q", is.IssueType)
	}
	h.quiet(r.ID)
}

func TestExportWithoutProjectOnlyOpenClosed(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(8, "Simple", "x", "", "public")
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)
	if h.status(is.NodeID) != "<no item>" {
		t.Fatal("no Project mapped: no item expected")
	}
	h.jk.editReq(r.ID, func(x *jikuReq) { x.State = "desarrollo" })
	h.quiet(r.ID) // an open stage has nowhere to go
	h.jk.editReq(r.ID, func(x *jikuReq) { x.State = "resuelto" })
	h.syncReq(r.ID)
	if is := h.issueOf(r.ID); is.State != "closed" || is.StateReason != "completed" {
		t.Fatalf("resuelto must close as completed: %+v", is)
	}
	h.quiet(r.ID)
}

// ---- R5: last write wins ----

func TestFieldEditsFlowBothWaysAndLastWriteWins(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Título", "Uno", "", "public")
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)

	h.jk.editReq(r.ID, func(x *jikuReq) { x.Title = "Título editado en Jiku" })
	h.syncReq(r.ID)
	if h.issueOf(r.ID).Title != "Título editado en Jiku" {
		t.Fatal("title edit in Jiku must reach GitHub")
	}
	h.gh.edit(is.NodeID, func(i *github.Issue) { i.Body = "Dos\n\n<!-- jiku-github-sync:requirement=1 -->" })
	h.syncReq(r.ID)
	if got, _ := h.jk.GetRequirement(h.ctx, r.ID); got.Description != "Dos" {
		t.Fatalf("body edit in GitHub must reach Jiku without the marker, got %q", got.Description)
	}
	// Both edit the title; GitHub's edit is the latest.
	h.jk.editReq(r.ID, func(x *jikuReq) { x.Title = "A (Jiku)" })
	h.gh.edit(is.NodeID, func(i *github.Issue) { i.Title = "B (GitHub)" })
	h.syncReq(r.ID)
	if got, _ := h.jk.GetRequirement(h.ctx, r.ID); got.Title != "B (GitHub)" || h.issueOf(r.ID).Title != "B (GitHub)" {
		t.Fatalf("latest edit must win on both sides, jiku=%q", got.Title)
	}
	h.quiet(r.ID)
}

// ---- States ----

func TestStatusMovesFlowBothWays(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Estados", "x", "funcionalidad", "public")
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)

	h.gh.setField(is.NodeID, "Status", "Review", "dev")
	h.syncReq(r.ID)
	if h.req(r.ID) != "revision" {
		t.Fatalf("Review -> revision, got %s", h.req(r.ID))
	}
	h.jk.editReq(r.ID, func(x *jikuReq) { x.State = "planificacion" })
	h.syncReq(r.ID)
	if h.status(is.NodeID) != "Backlog" {
		t.Fatalf("planificacion -> Backlog, got %s", h.status(is.NodeID))
	}
	h.quiet(r.ID)
}

func TestStuckGoesToEnColaWithInternalCommentAndNoPushBack(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Trabado", "x", "", "public")
	h.jk.editReq(r.ID, func(x *jikuReq) { x.State = "desarrollo" })
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)
	h.jk.takeWrites()
	h.gh.setField(is.NodeID, "Status", "Blocked", "dev1")
	h.syncReq(r.ID)
	if h.req(r.ID) != "en_cola" {
		t.Fatalf("Blocked -> en_cola, got %s", h.req(r.ID))
	}
	w := h.jk.takeWrites()
	if !slices.ContainsFunc(w, func(s string) bool {
		return strings.HasPrefix(s, "internal comment") && strings.Contains(s, "Marcado como Blocked en GitHub por @dev1")
	}) {
		t.Fatalf("expected the internal notice, got %v", w)
	}
	if h.status(is.NodeID) != "Blocked" {
		t.Fatalf("the card must stay in Blocked, got %s", h.status(is.NodeID))
	}
	h.quiet(r.ID)
}

func TestUnmappedOptionChangesNothingAndWarns(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "QA", "x", "", "public")
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)
	h.gh.schema.Fields["Status"].Options["QA"] = "O_QA"
	h.jk.takeWrites()
	h.gh.setField(is.NodeID, "Status", "QA", "dev")
	h.syncReq(r.ID)
	if h.req(r.ID) != "analisis" {
		t.Fatalf("an unmapped column must not move Jiku, got %s", h.req(r.ID))
	}
	w := h.jk.takeWrites()
	if len(w) != 1 || !strings.Contains(w[0], "«QA»") || !strings.HasPrefix(w[0], "internal") {
		t.Fatalf("expected one internal warning, got %v", w)
	}
	if h.status(is.NodeID) != "QA" {
		t.Fatal("the card stays where the person put it")
	}
	h.quiet(r.ID)
}

func TestClosingAnIncidenciaResolvesIt(t *testing.T) {
	h := newHarness(t)
	is := h.gh.newIssue("acme/api", "Bug", "x", "client")
	h.gh.edit(is.NodeID, func(i *github.Issue) { i.IssueType = "Bug" })
	id := h.importIssue(is)
	h.gh.edit(is.NodeID, func(i *github.Issue) { i.State, i.StateReason, i.ClosedBy = "closed", "completed", "dev1" })
	h.jk.takeWrites()
	h.syncReq(id)
	if h.req(id) != "resuelto" {
		t.Fatalf("closed -> resuelto, got %s", h.req(id))
	}
	w := strings.Join(h.jk.takeWrites(), "|")
	if !strings.Contains(w, "req.resolutionType=otro") || !strings.Contains(w, "Cerrado en GitHub por @dev1") {
		t.Fatalf("incidencia needs its resolution: %s", w)
	}
	h.quiet(id)
}

func TestCanceladoClosesAsNotPlannedAndReopenRestores(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Cancelar", "x", "", "public")
	h.jk.editReq(r.ID, func(x *jikuReq) { x.State = "revision" })
	h.syncReq(r.ID)
	h.jk.editReq(r.ID, func(x *jikuReq) { x.State = "cancelado" })
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)
	if is.State != "closed" || is.StateReason != "not_planned" {
		t.Fatalf("cancelado -> closed not_planned, got %+v", is)
	}
	h.quiet(r.ID) // the workflow moved the card to Done: not a change to carry back
	h.gh.edit(is.NodeID, func(i *github.Issue) { i.State, i.StateReason = "open", "reopened" })
	h.syncReq(r.ID)
	if h.req(r.ID) != "revision" {
		t.Fatalf("reopening goes back to the last open state, got %s", h.req(r.ID))
	}
	if h.status(is.NodeID) != "Review" {
		t.Fatalf("and the card follows it, got %s", h.status(is.NodeID))
	}
	h.quiet(r.ID)
}

func TestCardMovedToDoneWhileOpenClosesTheIssue(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Done", "x", "", "public")
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)
	h.gh.setField(is.NodeID, "Status", "Done", "dev")
	h.syncReq(r.ID)
	if h.req(r.ID) != "resuelto" || h.issueOf(r.ID).State != "closed" {
		t.Fatalf("Done on an open issue resolves and closes, got %s / %s", h.req(r.ID), h.issueOf(r.ID).State)
	}
	h.quiet(r.ID)
}

func TestAutomationOverwriteIsUndone(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Auto", "x", "", "public")
	h.jk.editReq(r.ID, func(x *jikuReq) { x.State = "desarrollo" })
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)
	// The "item added" workflow fires late and puts the card back in Inbox.
	h.gh.setField(is.NodeID, "Status", "Inbox", AutomationLogin)
	h.syncReq(r.ID)
	if h.req(r.ID) != "desarrollo" || h.status(is.NodeID) != "In Progress" {
		t.Fatalf("automation is not a person: jiku=%s card=%s", h.req(r.ID), h.status(is.NodeID))
	}
	h.quiet(r.ID)
}

func TestPriorityAndTypeFromGitHub(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Prio", "x", "funcionalidad", "public")
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)
	h.gh.setField(is.NodeID, "Priority", "Low", "dev")
	h.gh.edit(is.NodeID, func(i *github.Issue) { i.IssueType = "Bug" })
	h.syncReq(r.ID)
	got, _ := h.jk.GetRequirement(h.ctx, r.ID)
	if got.Priority != "baja" || got.TypeOrEmpty() != "incidencia" {
		t.Fatalf("priority/type from GitHub: %s %s", got.Priority, got.TypeOrEmpty())
	}
	h.quiet(r.ID)
	h.jk.editReq(r.ID, func(x *jikuReq) { m := "mejora"; x.Type = &m })
	h.syncReq(r.ID)
	if h.issueOf(r.ID).IssueType != "Feature" {
		t.Fatalf("mejora -> Feature, got %q", h.issueOf(r.ID).IssueType)
	}
	h.quiet(r.ID) // Feature reads back as funcionalidad, but Jiku did not change: no flip-flop
}

// ---- Comments (R6–R8) ----

func TestCommentsBothWays(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Charla", "x", "", "public")
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)

	pub := h.jk.personComment(r.ID, "comentario público", "public")
	h.jk.personComment(r.ID, "comentario interno", "internal")
	gc := h.gh.comment(is.NodeID, "client", "respuesta del cliente")
	h.syncReq(r.ID)

	gcs, _ := h.gh.ListComments(h.ctx, is.Repo, is.Number)
	var mirrored []string
	for _, c := range gcs {
		mirrored = append(mirrored, c.Body)
	}
	all := strings.Join(mirrored, "\n---\n")
	if !strings.Contains(all, "**Ana Pérez** comentó en Jiku:\n\ncomentario público") || strings.Contains(all, "interno") {
		t.Fatalf("only public Jiku comments go to GitHub:\n%s", all)
	}
	jcs, _ := h.jk.ListComments(h.ctx, r.ID)
	var mirror int64
	for _, c := range jcs {
		if strings.Contains(c.Body, "respuesta del cliente") {
			mirror = c.ID
			if c.VisibilityLevel != "public" || !strings.HasPrefix(c.Body, "**@client** comentó en [GitHub](") {
				t.Fatalf("GitHub comment mirror: %+v", c)
			}
		}
	}
	if mirror == 0 {
		t.Fatal("GitHub comment did not reach Jiku")
	}
	h.quiet(r.ID)

	// Edits follow the source.
	h.jk.personEditComment(r.ID, pub.ID, "comentario público (editado)")
	h.gh.edit(is.NodeID, func(*github.Issue) {})
	for _, c := range h.gh.comms[is.NodeID] {
		if c.ID == gc.ID {
			c.Body = "respuesta editada"
		}
	}
	h.syncReq(r.ID)
	jcs, _ = h.jk.ListComments(h.ctx, r.ID)
	if !slices.ContainsFunc(jcs, func(c *jikuComment) bool { return c.ID == mirror && strings.HasSuffix(c.Body, "respuesta editada") }) {
		t.Fatal("GitHub edit must reach the Jiku mirror")
	}
	gcs, _ = h.gh.ListComments(h.ctx, is.Repo, is.Number)
	if !slices.ContainsFunc(gcs, func(c *github.Comment) bool { return strings.Contains(c.Body, "(editado)") }) {
		t.Fatal("Jiku edit must reach the GitHub mirror")
	}
	h.quiet(r.ID)

	// Deleted on GitHub.
	h.gh.deleteComment(is.NodeID, gc.ID)
	h.syncReq(r.ID)
	jcs, _ = h.jk.ListComments(h.ctx, r.ID)
	if !slices.ContainsFunc(jcs, func(c *jikuComment) bool {
		return c.ID == mirror && strings.Contains(c.Body, "comentario eliminado en GitHub") && strings.Contains(c.Body, "@client")
	}) {
		t.Fatal("deleted GitHub comment must be marked in Jiku")
	}
	h.quiet(r.ID)
}

func TestCommentLinksRecoveredWithoutState(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Recuperar", "x", "", "public")
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)
	h.jk.personComment(r.ID, "hola", "public")
	h.gh.comment(is.NodeID, "client", "chau")
	h.syncReq(r.ID)
	// Lose the comment links (as if the KV write had failed after creating the mirrors).
	l := h.link(r.ID)
	l.Comments = nil
	if err := h.st.SaveLink(h.ctx, l); err != nil {
		t.Fatal(err)
	}
	h.quiet(r.ID)
}

// ---- R4: visibility ----

func TestVisibilityChanges(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Visible", "x", "", "public")
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)

	h.jk.editReq(r.ID, func(x *jikuReq) { x.VisibilityLevel = "internal" })
	h.syncReq(r.ID)
	gcs, _ := h.gh.ListComments(h.ctx, is.Repo, is.Number)
	if len(gcs) != 1 || !strings.Contains(gcs[0].Body, "dejó de sincronizarse") {
		t.Fatalf("expected the unlink notice, got %v", gcs)
	}
	h.jk.editReq(r.ID, func(x *jikuReq) { x.Title = "cambio mientras es interno" })
	h.quiet(r.ID)
	// The issue is not imported back as a new requirement.
	h.syncIssue(is)
	if w := h.jk.takeWrites(); len(w) > 0 {
		t.Fatalf("unlinked issue must not be imported, got %v", w)
	}

	h.jk.editReq(r.ID, func(x *jikuReq) { x.VisibilityLevel = "public" })
	if err := h.e.HandleRequirement(h.ctx, r.ID, true); err != nil {
		t.Fatal(err)
	}
	if h.issueOf(r.ID).Title != "cambio mientras es interno" {
		t.Fatal("revived pair must carry Jiku's changes")
	}
	h.quiet(r.ID)
}

func TestRequirementMadePublicLaterIsExported(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Antes interno", "x", "", "internal")
	h.jk.editReq(r.ID, func(x *jikuReq) { x.CreatedAt = x.CreatedAt.Add(-48 * time.Hour); x.VisibilityLevel = "public" })
	h.syncReq(r.ID)
	if _, err := h.st.Link(h.ctx, r.ID); err == nil {
		t.Fatal("an old requirement is only exported by the initial sync or by becoming public")
	}
	if err := h.e.HandleRequirement(h.ctx, r.ID, true); err != nil {
		t.Fatal(err)
	}
	h.link(r.ID)
}

// ---- Crash recovery ----

func TestInterruptedExportIsRecovered(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Interrumpido", "x", "", "public")
	// The issue was created but the link was never stored.
	if err := h.st.SetPendingRequirement(h.ctx, r.ID, state.Pending{At: h.clk.now(), Title: r.Title}); err != nil {
		t.Fatal(err)
	}
	h.gh.CreateIssue(h.ctx, "acme/api", github.IssueInput{Title: &r.Title, Body: strp(text.IssueBody("x", r.ID))})
	h.gh.takeWrites()
	h.syncReq(r.ID)
	if w := h.gh.takeWrites(); slices.ContainsFunc(w, func(s string) bool { return strings.HasPrefix(s, "create issue") }) {
		t.Fatalf("no duplicate issue expected, got %v", w)
	}
	h.link(r.ID)
}

func TestInterruptedImportIsRecovered(t *testing.T) {
	h := newHarness(t)
	is := h.gh.newIssue("acme/api", "Importado a medias", "x", "client")
	if err := h.st.SetPendingIssue(h.ctx, is.NodeID, state.Pending{At: h.clk.now(), Title: is.Title}); err != nil {
		t.Fatal(err)
	}
	h.jk.CreateRequirement(h.ctx, jikuNew{Title: is.Title, Description: "x", ProjectID: 7, VisibilityLevel: "public"})
	h.jk.takeWrites()
	h.syncIssue(is)
	if w := h.jk.takeWrites(); slices.ContainsFunc(w, func(s string) bool { return strings.HasPrefix(s, "create requirement") }) {
		t.Fatalf("no duplicate requirement expected, got %v", w)
	}
}

func TestIssueCreatedByConnectorIsLinkedNotImported(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Marcado", "x", "", "public")
	is := h.gh.newIssue("acme/api", "Marcado", text.IssueBody("x", r.ID), "connector")
	h.syncIssue(is)
	if w := h.jk.takeWrites(); slices.ContainsFunc(w, func(s string) bool { return strings.HasPrefix(s, "create requirement") }) {
		t.Fatal("the connector's own issue must not become a requirement")
	}
	if l := h.link(r.ID); l.IssueNodeID != is.NodeID {
		t.Fatal("it must be linked to its requirement")
	}
}

func TestTransferredIssueIsFollowed(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Mover", "x", "", "public")
	h.syncReq(r.ID)
	l := h.link(r.ID)
	h.gh.edit(l.IssueNodeID, func(i *github.Issue) { i.Repo, i.Number = "acme/web", 99 })
	h.syncReq(r.ID)
	if l := h.link(r.ID); l.Repo != "acme/web" || l.Number != 99 {
		t.Fatalf("link must follow the transfer, got %s", l.IssueKey())
	}
}

func TestRenamedOptionIsReadAgain(t *testing.T) {
	h := newHarness(t)
	r := h.jk.newReq(7, "Schema", "x", "", "public")
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)
	// Someone recreates the "Backlog" option: new id, same name.
	h.gh.schema.Fields["Status"].Options["Backlog"] = "O_Backlog_v2"
	h.jk.editReq(r.ID, func(x *jikuReq) { x.State = "planificacion" })
	h.syncReq(r.ID)
	if h.status(is.NodeID) != "Backlog" {
		t.Fatalf("option must be resolved by name again, got %s", h.status(is.NodeID))
	}
}

func TestExportWinsEvenIfTheTimelineLags(t *testing.T) {
	h := newHarness(t)
	h.gh.timelineLag = 1 // the workflow's "Inbox" is not in the timeline yet: its actor is unknown
	r := h.jk.newReq(7, "Lag", "x", "", "public")
	h.jk.editReq(r.ID, func(x *jikuReq) { x.State = "desarrollo"; x.Priority = "alta" })
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)
	if h.req(r.ID) != "desarrollo" || h.status(is.NodeID) != "In Progress" {
		t.Fatalf("Jiku is the origin of a new issue: jiku=%s card=%s", h.req(r.ID), h.status(is.NodeID))
	}
	h.gh.timelineLag = 0
	h.quiet(r.ID)
}
