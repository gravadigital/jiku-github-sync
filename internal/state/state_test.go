package state

import (
	"context"
	"errors"
	"testing"
)

// State written by 1.0.0 (before tasks) must still be read as it was.
func TestStateFromBeforeTasksIsReadable(t *testing.T) {
	ctx := context.Background()
	kv := NewMem()
	_ = kv.Put(ctx, "link.r.24", []byte(`{"reqId":24,"jikuProject":91,"issueNodeId":"I_x","repo":"acme-org/example-repo","number":69,`+
		`"itemId":"PVTI_x","last":{"title":"T","jBody":"a","gBody":"b","jikuState":"analisis","jikuPriority":"media","jikuType":"funcionalidad",`+
		`"ghOpen":true,"ghStatus":"Inbox","ghPriority":"Medium","ghType":"funcionalidad","lastOpenState":"analisis"},"syncedAt":"2026-11-12T15:00:00Z"}`))
	_ = kv.Put(ctx, "link.i.I_x", []byte(`24`))
	_ = kv.Put(ctx, "project.91", []byte(`{"startedAt":"2026-11-12T15:00:00Z","initialSyncAt":"2026-11-12T15:05:00Z","links":[24]}`))
	st := New(kv)

	l, err := st.LinkByIssue(ctx, "I_x")
	if err != nil {
		t.Fatal(err)
	}
	if l.IsTask() || l.ReqID != 24 || l.Last.GHStatus != "Inbox" || l.Unlinked {
		t.Fatalf("an old link is a requirement's, as it was: %+v", l)
	}
	if _, err := st.TaskLinkByIssue(ctx, "I_x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an old issue has no task link: %v", err)
	}
	ps, err := st.Project(ctx, 91)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.TasksStartedAt.IsZero() || len(ps.TaskLinks) != 0 || ps.InitialSyncAt.IsZero() || len(ps.Links) != 1 {
		t.Fatalf("an old project state reads with tasks never started: %+v", ps)
	}
	// Saving it again keeps it a requirement link (no task keys).
	if err := st.Save(ctx, l); err != nil {
		t.Fatal(err)
	}
	for _, k := range kv.Keys() {
		if k == "link.t.0" || k == "link.ti.I_x" {
			t.Fatalf("a requirement link must not write task keys: %v", kv.Keys())
		}
	}
}

func TestTaskLinksHaveTheirOwnKeys(t *testing.T) {
	ctx := context.Background()
	st := New(NewMem())
	if err := st.SaveLink(ctx, &Link{ReqID: 5, JikuProject: 1, IssueNodeID: "I_parent"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTaskLink(ctx, &Link{TaskID: 9, ReqID: 5, ParentNode: "I_parent", JikuProject: 1, IssueNodeID: "I_sub"}); err != nil {
		t.Fatal(err)
	}
	if l, err := st.TaskLinkByIssue(ctx, "I_sub"); err != nil || l.TaskID != 9 || l.ReqID != 5 {
		t.Fatalf("task link by issue: %+v %v", l, err)
	}
	if _, err := st.LinkByIssue(ctx, "I_sub"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a sub-issue is not a requirement's issue")
	}
	if l, err := st.Link(ctx, 5); err != nil || l.IsTask() {
		t.Fatalf("the requirement's link is untouched: %+v %v", l, err)
	}
	ps, _ := st.Project(ctx, 1)
	if len(ps.Links) != 1 || len(ps.TaskLinks) != 1 || ps.TaskLinks[0] != 9 {
		t.Fatalf("separate indexes: %+v", ps)
	}
	if err := st.SaveTaskLink(ctx, &Link{ReqID: 5}); err == nil {
		t.Fatal("a task link needs its task id")
	}
}
