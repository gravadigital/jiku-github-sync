package worker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gravadigital/jiku-github-sync/internal/engine"
	"github.com/gravadigital/jiku-github-sync/internal/initial"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/verify"
)

// initialSync plans the project's initial sync and logs the plan; with initialSync: apply it also
// runs it, once. The plan is logged one line per item, so `docker logs ... | jq` reads it.
func (w *Worker) initialSync(ctx context.Context, projectID int64) error {
	p := w.o.Engine.Config().Project(projectID)
	if p == nil || p.InitialSyncMode() == mapping.InitialSyncOff {
		return nil
	}
	ps, err := w.o.Store.Project(ctx, projectID)
	if err != nil {
		return err
	}
	if !ps.InitialSyncAt.IsZero() {
		w.log.Info("initial sync already applied", "project", projectID, "at", ps.InitialSyncAt)
		return nil
	}
	pl, err := initial.Build(ctx, initial.Deps{GitHub: w.o.GitHubFor(p), Jiku: w.o.Jiku, Store: w.o.Store,
		WriteInterval: w.o.WriteInterval, ItemSettle: engine.DefaultItemSettle}, p)
	if err != nil {
		return fmt.Errorf("initial sync plan: %w", err)
	}
	log := w.log.With("project", projectID, "mode", p.InitialSyncMode())
	for _, x := range pl.Link {
		log.Info("initial sync plan: link", "requirement", x.Req.ID, "issue", fmt.Sprintf("%s#%d", x.Issue.Repo, x.Issue.Number), "by", x.Reason, "title", x.Req.Title)
	}
	for _, r := range pl.Export {
		log.Info("initial sync plan: create issue", "requirement", r.ID, "state", r.State, "title", r.Title)
	}
	for _, is := range pl.Import {
		log.Info("initial sync plan: create requirement", "issue", fmt.Sprintf("%s#%d", is.Repo, is.Number), "title", is.Title)
	}
	for _, s := range pl.Skipped {
		log.Warn("initial sync plan: skipped", "reason", s)
	}
	log.Info("initial sync plan",
		"link", len(pl.Link), "createIssues", len(pl.Export), "createRequirements", len(pl.Import), "skipped", len(pl.Skipped),
		"commentsToGithub", pl.CommentsToGitHub, "commentsToJiku", pl.CommentsToJiku,
		"githubCreates", pl.GitHubCreates, "jikuCreates", pl.JikuCreates, "estimate", pl.Estimate.String(),
		"mailsNote", "every comment copied into Jiku notifies the requirement's subscribers")
	if p.InitialSyncMode() != mapping.InitialSyncApply {
		log.Info("initial sync NOT applied: set initialSync: apply for this project in the mapping to run it (the mapping is reloaded without a restart)")
		return nil
	}
	if len(pl.Link)+len(pl.Export)+len(pl.Import) == 0 {
		log.Info("initial sync: nothing to do")
	}
	log.Info("initial sync: applying")
	progress := logWriter(log.Info)
	defer progress.Close()
	if err := initial.Apply(ctx, w.o.Engine, w.o.Store, pl, progress); err != nil {
		return fmt.Errorf("initial sync finished with errors (retried on the next start or mapping reload): %w", err)
	}
	log.Info("initial sync: done")
	return nil
}

// verify logs a read-only comparison of both sides of every pair of the project.
func (w *Worker) verify(ctx context.Context, projectID int64) error {
	p := w.o.Engine.Config().Project(projectID)
	if p == nil {
		return nil
	}
	gh := w.o.GitHubFor(p)
	rep, err := verify.Run(ctx, gh, gh, w.o.Jiku, w.o.Store, p)
	if err != nil {
		return fmt.Errorf("verification: %w", err)
	}
	log := w.log.With("project", projectID)
	ps, err := w.o.Store.Project(ctx, projectID)
	if err != nil {
		return err
	}
	missing := rep.Missing
	if ps.InitialSyncAt.IsZero() {
		// Without an initial sync, what existed before the project was mapped is expected to be
		// unpaired: it is the initial sync's plan, not a difference.
		log.Info("verification: items waiting for the initial sync", "count", len(missing))
		missing = nil
	}
	const maxLines = 50
	for i, d := range append(append([]string{}, rep.Diffs...), missing...) {
		if i == maxLines {
			log.Warn("verification: more differences not logged", "more", len(rep.Diffs)+len(missing)-maxLines)
			break
		}
		log.Warn("verification: difference (it may be a change still in flight)", "detail", d)
	}
	log.Info("verification", "pairs", rep.Pairs, "pairsOK", rep.PairsOK, "unlinked", rep.Unlinked,
		"differences", len(rep.Diffs), "unpaired", len(missing), "commentsJiku", rep.CommentsJ, "commentsGithub", rep.CommentsGH)
	return nil
}

// logWriter turns the initial sync's progress lines into log lines.
func logWriter(f func(string, ...any)) io.WriteCloser {
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				f("initial sync: " + line)
			}
		}
	}()
	return pw
}
