// Package text builds what the connector writes on each side and recognises it when it reads it
// back: a hidden marker on everything it writes to GitHub, and the attribution line (with a link
// to the original GitHub comment) on everything it writes to Jiku.
package text

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const markerPrefix = "<!-- jiku-github-sync:"

var (
	markerRe      = regexp.MustCompile(`\n*<!-- jiku-github-sync:[^>]*-->\s*`)
	requirementRe = regexp.MustCompile(`<!-- jiku-github-sync:requirement=(\d+) -->`)
	commentRe     = regexp.MustCompile(`<!-- jiku-github-sync:comment=(\d+) -->`)
	ghCommentRe   = regexp.MustCompile(`#issuecomment-(\d+)\)`)
)

// IssueBody is the body of an issue created from a requirement.
func IssueBody(description string, reqID int64) string {
	return strings.TrimRight(description, "\n") + fmt.Sprintf("\n\n%srequirement=%d -->", markerPrefix, reqID)
}

// RequirementID returns the requirement an issue was created from, if it carries the marker.
func RequirementID(body string) (int64, bool) {
	m := requirementRe.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	return id, err == nil
}

// HasMarker reports whether the connector wrote this text on GitHub.
func HasMarker(body string) bool { return strings.Contains(body, markerPrefix) }

// StripMarkers removes the connector's markers.
func StripMarkers(body string) string { return markerRe.ReplaceAllString(body, "") }

// Normalize is the form two texts are compared in: no markers, LF line ends, no trailing space.
func Normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = StripMarkers(s)
	return strings.TrimSpace(s)
}

// Hash is a short stable hash of the normalized text.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(Normalize(s)))
	return hex.EncodeToString(sum[:12])
}

// GitHubComment renders a Jiku comment for GitHub. With date, the original date goes in the
// header (initial sync).
func GitHubComment(author, body string, jikuCommentID int64, date *time.Time) string {
	head := fmt.Sprintf("**%s** comentó en Jiku", escapeName(author))
	if date != nil {
		head += " el " + date.Format("2006-01-02")
	}
	return fmt.Sprintf("%s:\n\n%s\n\n%scomment=%d -->", head, strings.TrimSpace(body), markerPrefix, jikuCommentID)
}

// JikuCommentID returns the Jiku comment a GitHub comment mirrors, if it carries the marker.
func JikuCommentID(body string) (int64, bool) {
	m := commentRe.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	return id, err == nil
}

// JikuComment renders a GitHub comment for Jiku: the attribution line links to the original.
func JikuComment(login, url, body string, date *time.Time) string {
	head := fmt.Sprintf("**@%s** comentó en [GitHub](%s)", login, url)
	if date != nil {
		head += " el " + date.Format("2006-01-02")
	}
	return head + ":\n\n" + strings.TrimSpace(StripMarkers(body))
}

// GitHubCommentID returns the GitHub comment a Jiku comment mirrors, from its attribution link.
func GitHubCommentID(body string) (int64, bool) {
	first, _, _ := strings.Cut(body, "\n")
	m := ghCommentRe.FindStringSubmatch(first)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	return id, err == nil
}

// DeletedComment is the text a Jiku mirror takes when its GitHub comment is deleted.
func DeletedComment(login, url string) string {
	return fmt.Sprintf("**@%s** comentó en [GitHub](%s):\n\n_(comentario eliminado en GitHub)_", login, url)
}

// Notice is a comment the connector itself writes on GitHub (not a mirror of anything).
func Notice(msg string) string {
	return msg + "\n\n" + markerPrefix + "notice -->"
}

// Expand replaces {login} and {option} in a configured comment template.
func Expand(tpl, login, option string) string {
	return strings.NewReplacer("{login}", login, "{option}", option).Replace(tpl)
}

func escapeName(s string) string {
	return strings.NewReplacer("*", "\\*", "_", "\\_").Replace(s)
}

// Attachment references, in the forms each side writes them.
var (
	// Jiku: `![file:N]`/`[file:N]` (a file id), `![attach:N]`/`[attach:N]`/`![img:N]` (an
	// attachment id) and the older links to /api/{attachments,files}/N/preview.
	JikuFileRef   = regexp.MustCompile(`(!?)\[file:(\d+)\]`)
	JikuAttachRef = regexp.MustCompile(`(!?)\[(?:attach|img):(\d+)\]`)
	JikuAPIRef    = regexp.MustCompile(`(!?)\[([^\]\n]*)\]\(/api/(attachments|files)/(\d+)/preview\)`)
	// GitHub: images and videos are github.com/user-attachments/assets/<uuid>, as markdown, as an
	// <img> tag or as a bare line (videos); other files are user-attachments/files/<id>/<name>.
	GHImageRef = regexp.MustCompile(`!\[([^\]\n]*)\]\((https://github\.com/user-attachments/assets/[0-9a-fA-F-]+)\)`)
	GHImgTag   = regexp.MustCompile(`<img\b[^>]*?\bsrc="(https://github\.com/user-attachments/assets/[0-9a-fA-F-]+)"[^>]*>`)
	GHBareRef  = regexp.MustCompile(`(?m)^[ \t]*(https://github\.com/user-attachments/assets/[0-9a-fA-F-]+)[ \t]*$`)
	GHFileRef  = regexp.MustCompile(`\[([^\]\n]*)\]\((https://github\.com/user-attachments/files/\d+/[^)\s]+)\)`)
	// The connector's own notes for a file that could not be copied.
	noteRe   = regexp.MustCompile(`📎 [^\n]*?\((?:adjunto en Jiku|no se pudo copiar: [^)\n]*)\)(?: — \[ver en GitHub\]\([^)\s]*\))?`)
	altRe    = regexp.MustCompile(`\balt="([^"]*)"`)
	spacesRe = regexp.MustCompile(`\s+`)
)

// ImgAlt returns the alt text of an <img> tag.
func ImgAlt(tag string) string {
	if m := altRe.FindStringSubmatch(tag); m != nil {
		return m[1]
	}
	return ""
}

// Comparable is a text with every attachment reference and the connector's file notes removed
// and its whitespace collapsed: the two sides of a description hold their own form of each
// attachment, so this is what tells whether they say the same.
func Comparable(s string) string {
	s = Normalize(s)
	for _, re := range []*regexp.Regexp{JikuAPIRef, JikuFileRef, JikuAttachRef, GHImageRef, GHImgTag, GHFileRef, GHBareRef, noteRe} {
		s = re.ReplaceAllString(s, " ")
	}
	return strings.TrimSpace(spacesRe.ReplaceAllString(s, " "))
}

// FileNote is what stands for a Jiku file that cannot become a GitHub attachment.
func FileNote(name string) string {
	return fmt.Sprintf("📎 %s (adjunto en Jiku)", escapeName(name))
}

// FileNoteRe finds the notes FileNote writes; the group is the (escaped) name.
var FileNoteRe = regexp.MustCompile(`📎 ([^\n]+?) \(adjunto en Jiku\)`)

// EscapedName is a file name as FileNote writes it.
func EscapedName(name string) string { return escapeName(name) }

// NotCopiedNote is what stands in Jiku for a GitHub attachment that could not be copied.
func NotCopiedNote(name, reason, url string) string {
	return fmt.Sprintf("📎 %s (no se pudo copiar: %s) — [ver en GitHub](%s)", escapeName(name), reason, url)
}
