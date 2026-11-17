package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
)

type parentsGH struct {
	GitHub
	list []github.IssueParent
}

func (f *parentsGH) IssueParents(context.Context, string) ([]github.IssueParent, error) {
	return f.list, nil
}

// Linking a sub-issue does not touch updated_at: the poller sees it by comparing parents.
func TestSweepParentsQueuesWhatChanged(t *testing.T) {
	ctx := context.Background()
	w := New(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	p := &mapping.Project{JikuProject: 7}
	gh := &parentsGH{list: []github.IssueParent{
		{NodeID: "A", Parent: ""},
		{NodeID: "B", Parent: "acme/api#1"},
	}}
	drain := func() map[string]bool {
		got := map[string]bool{}
		for {
			k, ok := w.q.Pop()
			if !ok {
				return got
			}
			got[k.Node] = true
		}
	}
	if err := w.sweepParents(ctx, gh, p, "acme/api"); err != nil {
		t.Fatal(err)
	}
	if got := drain(); len(got) != 1 || !got["B"] {
		t.Fatalf("first sight: only issues with a parent are checked, got %v", got)
	}
	if err := w.sweepParents(ctx, gh, p, "acme/api"); err != nil {
		t.Fatal(err)
	}
	if got := drain(); len(got) != 0 {
		t.Fatalf("nothing changed: nothing queued, got %v", got)
	}
	gh.list = []github.IssueParent{{NodeID: "A", Parent: "acme/api#1"}, {NodeID: "B", Parent: ""}}
	if err := w.sweepParents(ctx, gh, p, "acme/api"); err != nil {
		t.Fatal(err)
	}
	if got := drain(); len(got) != 2 || !got["A"] || !got["B"] {
		t.Fatalf("both changed parent: both queued, got %v", got)
	}
}
