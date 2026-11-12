package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/engine"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
)

// listingJiku answers ListRequirements like Jiku would with its filter; the rest is unused.
type listingJiku struct {
	jiku.API
	reqs    []*jiku.Requirement
	filters []map[string]any
}

func (f *listingJiku) ListRequirements(_ context.Context, _ int64, filter map[string]any) ([]*jiku.Requirement, error) {
	f.filters = append(f.filters, filter)
	var out []*jiku.Requirement
	for _, r := range f.reqs {
		if c, ok := filter["createdAt"].(map[string]any); ok {
			from, _ := time.Parse(time.RFC3339, c["gte"].(string))
			if r.CreatedAt.Before(from) {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func TestReconciliationFindsRequirementsThatBecamePublicUnseen(t *testing.T) {
	ctx := context.Background()
	cfg, err := mapping.Parse([]byte("projects:\n  - { jikuProject: 7, repos: [acme/api], defaultRepo: acme/api }\n"))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	st := state.New(state.NewMem())
	if err := st.UpdateProject(ctx, 7, func(ps *state.ProjectState) { ps.StartedAt = started; ps.Links = []int64{1} }); err != nil {
		t.Fatal(err)
	}
	jk := &listingJiku{reqs: []*jiku.Requirement{
		{ID: 1, CreatedAt: started.Add(-time.Hour)},                     // linked
		{ID: 2, CreatedAt: started.Add(-time.Hour)},                     // existed before, public now, no pair
		{ID: 3, CreatedAt: started.Add(time.Hour)},                      // new
		{ID: 4, CreatedAt: started.Add(-time.Hour), State: "cancelado"}, // closed history: stays out
		{ID: 5, CreatedAt: started.Add(time.Hour), State: "resuelto"},   // new, already closed: goes
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := engine.New(engine.Options{Jiku: jk, Store: st, Config: cfg, Logger: log})
	queued := func() map[int64]bool {
		w := New(Options{Engine: eng, Jiku: jk, Store: st, Logger: log})
		if err := w.ReconcileOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got := map[int64]bool{}
		for {
			k, ok := w.q.Pop()
			if !ok {
				return got
			}
			got[k.ReqID] = k.BecamePublic
		}
	}

	// Before the initial sync: what existed before is the initial sync's plan.
	got := queued()
	if _, ok := got[2]; ok || len(got) != 3 || got[3] || got[5] {
		t.Fatalf("before the initial sync: %v", got)
	}

	// After it: an older open public requirement without a pair goes as one that just became
	// public; newer ones go as before; older closed ones stay out.
	if err := st.UpdateProject(ctx, 7, func(ps *state.ProjectState) { ps.InitialSyncAt = started.Add(time.Minute) }); err != nil {
		t.Fatal(err)
	}
	got = queued()
	if _, closed := got[4]; closed || !got[2] || got[3] || got[1] || len(got) != 4 {
		t.Fatalf("after the initial sync: %v", got)
	}
	if _, ok := jk.filters[len(jk.filters)-1]["createdAt"]; ok {
		t.Fatal("after the initial sync every public requirement is listed")
	}
}
