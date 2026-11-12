package engine

import (
	"context"
	"errors"
	"fmt"
	"html"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

// Attachments travel as files, not as links: a Jiku file becomes a GitHub attachment and a GitHub
// attachment becomes a Jiku file. GitHub only takes images and videos that way, so any other Jiku
// file is named on GitHub ("📎 informe.pdf (adjunto en Jiku)"); a GitHub attachment Jiku refuses
// (type, size) is named in Jiku with a link to it. Each pair remembers which files are the same
// on both sides (Link.Assets), so a text that goes back and forth does not copy them again.

// maxFilesPerWrite is Jiku's limit of files linked by one command.
const maxFilesPerWrite = 10

// toGitHub turns a Jiku text into GitHub's form. With appendRest, the attachments the text does
// not reference (comments made before Jiku embedded them) are added at the end.
func (e *Engine) toGitHub(ctx context.Context, p *mapping.Project, l *state.Link, repo, body string, atts []jiku.Attachment, appendRest bool) (string, error) {
	byFile := map[int64]jiku.Attachment{}
	byAttach := map[int64]jiku.Attachment{}
	for _, a := range atts {
		byFile[a.FileID] = a
		byAttach[a.ID] = a
	}
	used := map[int64]bool{}
	var firstErr error
	render := func(fileID int64, name string) string {
		if firstErr != nil {
			return ""
		}
		used[fileID] = true
		out, err := e.fileToGitHub(ctx, p, l, repo, fileID, byFile[fileID], name)
		if err != nil {
			firstErr = err
		}
		return out
	}
	body = text.JikuAPIRef.ReplaceAllStringFunc(body, func(m string) string {
		g := text.JikuAPIRef.FindStringSubmatch(m)
		id := parseID(g[4])
		if g[3] == "attachments" {
			a, ok := byAttach[id]
			if !ok {
				return text.FileNote(orDefault(g[2], "adjunto"))
			}
			return render(a.FileID, orDefault(g[2], a.FileName))
		}
		return render(id, g[2])
	})
	body = text.JikuFileRef.ReplaceAllStringFunc(body, func(m string) string {
		return render(parseID(text.JikuFileRef.FindStringSubmatch(m)[2]), "")
	})
	body = text.JikuAttachRef.ReplaceAllStringFunc(body, func(m string) string {
		a, ok := byAttach[parseID(text.JikuAttachRef.FindStringSubmatch(m)[2])]
		if !ok {
			return text.FileNote("adjunto")
		}
		return render(a.FileID, a.FileName)
	})
	if appendRest {
		var rest []string
		for _, a := range atts {
			if !used[a.FileID] {
				rest = append(rest, render(a.FileID, a.FileName))
			}
		}
		if len(rest) > 0 {
			body = strings.TrimRight(body, "\n") + "\n\n" + strings.Join(rest, "\n")
		}
	}
	return body, firstErr
}

// fileToGitHub is the GitHub form of one Jiku file.
func (e *Engine) fileToGitHub(ctx context.Context, p *mapping.Project, l *state.Link, repo string, fileID int64, a jiku.Attachment, name string) (string, error) {
	if name == "" {
		name = a.FileName
	}
	if u, ok := l.AssetURL(fileID); ok {
		return fmt.Sprintf("![%s](%s)", altText(name), u), nil
	}
	known := a.FileID != 0
	if known && (!github.AttachableTypes[a.MimeType] || a.FileSize > github.MaxAttachmentBytes) {
		return text.FileNote(orDefault(name, "adjunto")), nil
	}
	f, data, err := e.jk.DownloadFile(ctx, fileID)
	if errors.Is(err, jiku.ErrNotFound) {
		return text.FileNote(orDefault(name, "adjunto")), nil
	}
	if err != nil {
		return "", err
	}
	if name == "" {
		name = f.Name
	}
	if !github.AttachableTypes[f.MimeType] || len(data) > github.MaxAttachmentBytes {
		return text.FileNote(name), nil
	}
	u, err := e.gh(p).UploadAsset(ctx, repo, f.Name, f.MimeType, data)
	if rejected(err) {
		e.log.Warn("github refused an attachment: named instead", "file", fileID, "error", err)
		return text.FileNote(name), nil
	}
	if err != nil {
		return "", err
	}
	l.AddAsset(fileID, u)
	e.log.Info("attachment copied to github", "requirement", l.ReqID, "file", fileID)
	return fmt.Sprintf("![%s](%s)", altText(name), u), nil
}

// toJiku turns a GitHub text into Jiku's form, copying its attachments into Jiku. bodyHTML gives
// the rendered text (with signed links); linked are the files the target already has, which can
// be referenced again without a copy. It returns the files to link.
func (e *Engine) toJiku(ctx context.Context, p *mapping.Project, l *state.Link, body string, bodyHTML func() (string, error), linked []jiku.Attachment) (string, []int64, error) {
	// A Jiku file GitHub only names comes back as the reference it was, when the target has it.
	body = text.FileNoteRe.ReplaceAllStringFunc(body, func(m string) string {
		name := text.FileNoteRe.FindStringSubmatch(m)[1]
		for _, a := range linked {
			if text.EscapedName(a.FileName) == name {
				return placeholder(a.FileID, strings.HasPrefix(a.MimeType, "image/"))
			}
		}
		return m
	})
	if !hasGHRefs(body) {
		return body, nil, nil
	}
	isLinked := map[int64]bool{}
	for _, a := range linked {
		isLinked[a.FileID] = true
	}
	var (
		files    []int64
		firstErr error
		rendered *string
		done     = map[string]string{}
	)
	signed := func(u string) string {
		if rendered == nil {
			h, err := bodyHTML()
			if err != nil {
				e.log.Warn("could not read the rendered text: attachments are read unsigned", "error", err)
			}
			rendered = &h
		}
		return signedURL(*rendered, u)
	}
	copyRef := func(u, name string, image bool) string {
		if firstErr != nil {
			return ""
		}
		if out, ok := done[u]; ok {
			return out
		}
		out, err := e.fileToJiku(ctx, p, l, u, name, image, isLinked, &files, signed)
		if err != nil {
			firstErr = err
		}
		done[u] = out
		return out
	}
	body = text.GHImageRef.ReplaceAllStringFunc(body, func(m string) string {
		g := text.GHImageRef.FindStringSubmatch(m)
		return copyRef(g[2], g[1], true)
	})
	body = text.GHImgTag.ReplaceAllStringFunc(body, func(m string) string {
		g := text.GHImgTag.FindStringSubmatch(m)
		return copyRef(g[1], text.ImgAlt(m), true)
	})
	body = text.GHBareRef.ReplaceAllStringFunc(body, func(m string) string {
		return copyRef(strings.TrimSpace(m), "", true)
	})
	body = text.GHFileRef.ReplaceAllStringFunc(body, func(m string) string {
		g := text.GHFileRef.FindStringSubmatch(m)
		return copyRef(g[2], g[1], false)
	})
	return body, files, firstErr
}

func (e *Engine) fileToJiku(ctx context.Context, p *mapping.Project, l *state.Link, u, name string, image bool, isLinked map[int64]bool, files *[]int64, signed func(string) string) (string, error) {
	if id, ok := l.AssetFile(u); ok && isLinked[id] {
		return placeholder(id, image), nil
	}
	shown := orDefault(name, path.Base(u))
	if len(*files) >= maxFilesPerWrite {
		return text.NotCopiedNote(shown, fmt.Sprintf("Jiku admite hasta %d adjuntos por texto", maxFilesPerWrite), u), nil
	}
	data, ct, err := e.gh(p).Download(ctx, signed(u))
	if rejected(err) {
		e.log.Warn("could not download a github attachment", "url", u, "error", err)
		return text.NotCopiedNote(shown, "GitHub no permitió descargarlo", u), nil
	}
	if err != nil {
		return "", err
	}
	fname, mt := fileName(name, u, ct)
	id, err := e.jk.UploadFile(ctx, fname, mt, data)
	if errors.Is(err, jiku.ErrFileRejected) {
		return text.NotCopiedNote(fname, "Jiku no admite este tipo de archivo o su tamaño", u), nil
	}
	if err != nil {
		return "", err
	}
	*files = append(*files, id)
	l.AddAsset(id, u)
	e.log.Info("attachment copied to jiku", "requirement", l.ReqID, "file", id)
	return placeholder(id, strings.HasPrefix(mt, "image/")), nil
}

func placeholder(id int64, image bool) string {
	if image {
		return fmt.Sprintf("![file:%d]", id)
	}
	return fmt.Sprintf("[file:%d]", id)
}

func hasGHRefs(s string) bool { return strings.Contains(s, "github.com/user-attachments/") }

// signedURL finds, in the rendered text, the link GitHub signed for an attachment (it carries the
// asset's uuid or the file's id); without one, the plain URL is used (public repos).
func signedURL(rendered, u string) string {
	key := path.Base(u)
	if strings.Contains(u, "/user-attachments/files/") {
		key = "/files/" + path.Base(path.Dir(u)) + "/"
	}
	re := regexp.MustCompile(`(?:src|href)="([^"]*` + regexp.QuoteMeta(key) + `[^"]*)"`)
	if m := re.FindStringSubmatch(rendered); m != nil {
		return html.UnescapeString(m[1])
	}
	return u
}

// jikuTypes are the extensions Jiku accepts and their media types (what Jiku checks: both).
var jikuTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp", ".svg": "image/svg+xml", ".pdf": "application/pdf",
	".doc": "application/msword", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls": "application/vnd.ms-excel", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".txt": "text/plain", ".csv": "text/csv",
	".mp4": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm", ".zip": "application/zip",
}

// mediaExt is the extension given to a downloaded image or video, by its media type.
var mediaExt = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
	"image/svg+xml": ".svg", "video/mp4": ".mp4", "video/quicktime": ".mov", "video/webm": ".webm",
}

func extFor(contentType string) string {
	if e, ok := mediaExt[contentType]; ok {
		return e
	}
	if exts, _ := mime.ExtensionsByType(contentType); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// fileName builds the Jiku name and media type of a GitHub attachment: an image takes its alt
// text and the extension of what was downloaded; a file keeps its own name.
func fileName(alt, u, contentType string) (string, string) {
	base := path.Base(u)
	if s, err := url.PathUnescape(base); err == nil {
		base = s
	}
	ext := strings.ToLower(path.Ext(base))
	if strings.Contains(u, "/user-attachments/assets/") {
		ext = extFor(contentType)
		name := strings.TrimSpace(strings.NewReplacer("/", "-", "\\", "-").Replace(alt))
		if name == "" || strings.EqualFold(name, "image") {
			name = "imagen"
		}
		if len(name) > 100 {
			name = name[:100]
		}
		if !strings.EqualFold(path.Ext(name), ext) {
			name += ext
		}
		base = name
	}
	mt := contentType
	// Jiku checks the extension and the media type separately: the extension's type is used.
	if t, ok := jikuTypes[ext]; ok {
		mt = t
	}
	return base, mt
}

// rejected tells a refusal (the file or request is not acceptable: worth naming the file and
// moving on) from a failure worth retrying.
func rejected(err error) bool {
	var ae *github.APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests && ae.Status != http.StatusUnauthorized
}

func altText(s string) string { return strings.NewReplacer("[", "(", "]", ")", "\n", " ").Replace(s) }

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func parseID(s string) int64 {
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}
