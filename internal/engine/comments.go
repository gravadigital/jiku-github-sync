package engine

import (
	"context"
	"sort"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

// syncComments mirrors comments both ways:
//   - a PUBLIC Jiku comment by anyone but the connector becomes a GitHub comment;
//   - every GitHub comment not written by the connector becomes a PUBLIC Jiku comment;
//   - edits follow the source; a GitHub comment that disappears leaves its Jiku mirror saying so.
//
// The mirrors are recognisable on their own (a marker on GitHub, the attribution link in Jiku),
// so a crash between writing a mirror and saving the link does not produce a duplicate.
func (e *Engine) syncComments(ctx context.Context, p *mapping.Project, l *state.Link, req *jiku.Requirement, is *github.Issue, opts SyncOptions) error {
	log := e.logPair(l)
	jcs, err := e.jk.ListComments(ctx, req.ID)
	if err != nil {
		return err
	}
	gcs, err := e.gh(p).ListComments(ctx, is.Repo, is.Number)
	if err != nil {
		return err
	}
	byJ := map[int64]int{}
	byG := map[int64]int{}
	for i, c := range l.Comments {
		byJ[c.JikuID] = i
		byG[c.GHID] = i
	}
	add := func(c state.CommentLink) {
		l.Comments = append(l.Comments, c)
		byJ[c.JikuID] = len(l.Comments) - 1
		byG[c.GHID] = len(l.Comments) - 1
	}
	jByID := map[int64]*jiku.Comment{}
	for _, c := range jcs {
		jByID[c.ID] = c
	}
	gByID := map[int64]*github.Comment{}
	for _, c := range gcs {
		gByID[c.ID] = c
	}

	// Recover links the state lost: mirrors carry where they come from.
	for _, g := range gcs {
		if jid, ok := text.JikuCommentID(g.Body); ok {
			if _, linked := byJ[jid]; !linked {
				h := ""
				if jc := jByID[jid]; jc != nil {
					h = text.Hash(jc.Body)
				}
				add(state.CommentLink{JikuID: jid, GHID: g.ID, Origin: "jiku", SourceHash: h})
			}
		}
	}
	self := e.jk.SelfID()
	for _, j := range jcs {
		if j.AuthorID != self {
			continue
		}
		if gid, ok := text.GitHubCommentID(j.Body); ok {
			if _, linked := byG[gid]; !linked {
				h := ""
				if gc := gByID[gid]; gc != nil {
					h = text.Hash(gc.Body)
				}
				add(state.CommentLink{JikuID: j.ID, GHID: gid, Origin: "github", SourceHash: h})
			}
		}
	}

	// One timeline, oldest first, so the mirrors keep the conversation's order.
	type entry struct {
		at time.Time
		j  *jiku.Comment
		g  *github.Comment
	}
	var timeline []entry
	for _, j := range jcs {
		timeline = append(timeline, entry{at: j.CreatedAt, j: j})
	}
	for _, g := range gcs {
		timeline = append(timeline, entry{at: g.CreatedAt, g: g})
	}
	sort.SliceStable(timeline, func(a, b int) bool { return timeline[a].at.Before(timeline[b].at) })

	for _, en := range timeline {
		switch {
		case en.j != nil:
			j := en.j
			if j.AuthorID == self || j.VisibilityLevel != "public" {
				continue // the connector's own mirrors and notices; internal comments never leave Jiku
			}
			var date *time.Time
			if opts.WithDates {
				date = &j.CreatedAt
			}
			render := func() (string, error) {
				content, err := e.toGitHub(ctx, p, l, is.Repo, j.Body, j.Attachments, true)
				if err != nil {
					return "", err
				}
				return text.GitHubComment(j.AuthorName(), content, j.ID, date), nil
			}
			if i, ok := byJ[j.ID]; ok {
				cl := &l.Comments[i]
				if cl.Origin == "jiku" && text.Hash(j.Body) != cl.SourceHash {
					if _, alive := gByID[cl.GHID]; alive {
						body, err := render()
						if err != nil {
							return err
						}
						if err := e.gh(p).UpdateComment(ctx, is.Repo, cl.GHID, body); err != nil {
							return err
						}
						log.Info("comment edit mirrored to github", "jikuComment", j.ID)
					}
					cl.SourceHash = text.Hash(j.Body)
				}
				continue
			}
			body, err := render()
			if err != nil {
				return err
			}
			gc, err := e.gh(p).CreateComment(ctx, is.Repo, is.Number, body)
			if err != nil {
				return err
			}
			add(state.CommentLink{JikuID: j.ID, GHID: gc.ID, Origin: "jiku", SourceHash: text.Hash(j.Body)})
			log.Info("comment mirrored to github", "jikuComment", j.ID, "githubComment", gc.ID)
			if err := e.st.SaveLink(ctx, l); err != nil {
				return err
			}
		case en.g != nil:
			g := en.g
			if text.HasMarker(g.Body) {
				continue // written by the connector
			}
			var date *time.Time
			if opts.WithDates {
				date = &g.CreatedAt
			}
			html := func() (string, error) { return e.gh(p).CommentHTML(ctx, is.Repo, g.ID) }
			if i, ok := byG[g.ID]; ok {
				cl := &l.Comments[i]
				if cl.Origin == "github" && text.Hash(g.Body) != cl.SourceHash {
					var linked []jiku.Attachment
					if jc := jByID[cl.JikuID]; jc != nil {
						linked = jc.Attachments
					}
					content, files, err := e.toJiku(ctx, p, l, g.Body, html, linked)
					if err != nil {
						return err
					}
					var fileIDs []int64
					if len(files) > 0 {
						fileIDs = append(jiku.FileIDs(linked), files...)
					}
					if err := e.jk.EditComment(ctx, req.ID, cl.JikuID, text.JikuComment(g.Author, g.HTMLURL, content, date), fileIDs); err != nil {
						return err
					}
					cl.SourceHash = text.Hash(g.Body)
					log.Info("comment edit mirrored to jiku", "githubComment", g.ID)
				}
				continue
			}
			content, files, err := e.toJiku(ctx, p, l, g.Body, html, nil)
			if err != nil {
				return err
			}
			jid, err := e.jk.CreateComment(ctx, req.ID, text.JikuComment(g.Author, g.HTMLURL, content, date), "public", files)
			if err != nil {
				return err
			}
			add(state.CommentLink{JikuID: jid, GHID: g.ID, Origin: "github", SourceHash: text.Hash(g.Body)})
			log.Info("comment mirrored to jiku", "githubComment", g.ID, "jikuComment", jid)
			if err := e.st.SaveLink(ctx, l); err != nil {
				return err
			}
		}
	}

	// Deleted on GitHub: the Jiku mirror says so (Jiku has no delete).
	for i := range l.Comments {
		cl := &l.Comments[i]
		if cl.Origin != "github" || cl.Deleted {
			continue
		}
		if _, alive := gByID[cl.GHID]; alive {
			continue
		}
		login, url := "", is.HTMLURL
		if jc := jByID[cl.JikuID]; jc != nil {
			login, url = attribution(jc.Body, is.HTMLURL)
		}
		if err := e.jk.EditComment(ctx, req.ID, cl.JikuID, text.DeletedComment(login, url), nil); err != nil {
			return err
		}
		cl.Deleted = true
		log.Info("comment deleted on github: jiku mirror marked", "githubComment", cl.GHID)
	}
	return nil
}

// attribution recovers the login and link from a mirror's first line.
func attribution(body, fallbackURL string) (string, string) {
	login, url := "", fallbackURL
	if m := attrRe.FindStringSubmatch(body); m != nil {
		login, url = m[1], m[2]
	}
	return login, url
}
