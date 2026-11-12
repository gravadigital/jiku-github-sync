package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/text"
)

func TestJikuAttachmentsReachGitHub(t *testing.T) {
	h := newHarness(t)
	img := h.jk.personFile("captura.png", "image/png", []byte("PNG"))
	doc := h.jk.personFile("informe.pdf", "application/pdf", []byte("PDF"))
	r := h.jk.newReq(7, "Con adjuntos", fmt.Sprintf("Ver ![file:%d]\n\ny [file:%d]", img, doc), "", "public")
	h.jk.editReq(r.ID, func(x *jikuReq) { x.Attachments = h.jk.attachments([]int64{img, doc}) })
	h.syncReq(r.ID)

	is := h.issueOf(r.ID)
	body := text.Normalize(is.Body)
	if !strings.Contains(body, "![captura.png](https://github.com/user-attachments/assets/") {
		t.Fatalf("the image must be uploaded and embedded: %q", body)
	}
	if !strings.Contains(body, "📎 informe.pdf (adjunto en Jiku)") || strings.Contains(body, "[file:") {
		t.Fatalf("a document is named, and no Jiku reference is left: %q", body)
	}
	h.quiet(r.ID)

	// A comment: its referenced image, and an attachment it does not reference, go too.
	shot := h.jk.personFile("pantalla.jpg", "image/jpeg", []byte("JPG"))
	c := h.jk.personComment(r.ID, fmt.Sprintf("Mirá esto ![file:%d]", shot), "public")
	extra := h.jk.personFile("planilla.pdf", "application/pdf", []byte("PDF2"))
	h.jk.mu.Lock()
	c.Attachments = h.jk.attachments([]int64{shot, extra})
	h.jk.mu.Unlock()
	h.syncReq(r.ID)
	gcs, _ := h.gh.ListComments(h.ctx, is.Repo, is.Number)
	last := gcs[len(gcs)-1].Body
	if !strings.Contains(last, "Mirá esto ![pantalla.jpg](https://github.com/user-attachments/assets/") || !strings.Contains(last, "📎 planilla.pdf (adjunto en Jiku)") {
		t.Fatalf("comment attachments: %q", last)
	}
	h.quiet(r.ID)

	// Editing the description keeps the image already uploaded: no second upload.
	h.jk.editReq(r.ID, func(x *jikuReq) { x.Description = fmt.Sprintf("Ver bien ![file:%d]", img) })
	h.syncReq(r.ID)
	for _, w := range h.gh.takeWrites() {
		if strings.HasPrefix(w, "upload") {
			t.Fatalf("the image was uploaded again: %v", w)
		}
	}
	if b := text.Normalize(h.issueOf(r.ID).Body); !strings.HasPrefix(b, "Ver bien ![captura.png](https://github.com/user-attachments/assets/") {
		t.Fatalf("edited body: %q", b)
	}
	h.quiet(r.ID)
}

func TestGitHubAttachmentsReachJiku(t *testing.T) {
	h := newHarness(t)
	shot := h.gh.personAttach("shot.png", "image/png", []byte("PNG"))
	is := h.gh.newIssue("acme/web", "Con captura", "Mirá ![shot]("+shot+")", "client")
	id := h.importIssue(is)

	r, _ := h.jk.GetRequirement(h.ctx, id)
	if len(r.Attachments) != 1 || r.Description != fmt.Sprintf("Mirá ![file:%d]", r.Attachments[0].FileID) || r.Attachments[0].FileName != "shot.png" {
		t.Fatalf("the image must become a Jiku file of the requirement: %q %+v", r.Description, r.Attachments)
	}
	h.quiet(id)

	// A comment with an <img> tag, a file Jiku does not take and a document GitHub does not let
	// the connector download.
	photo := h.gh.personAttach("x", "image/png", []byte("PNG2"))
	anim := h.gh.personAttach("x", "image/webp", []byte("WEBP"))
	h.gh.comment(is.NodeID, "client", `Fotos <img width="10" alt="foto" src="`+photo+`" />
`+anim+`
y [datos.pdf](https://github.com/user-attachments/files/9/datos.pdf)`)
	h.syncReq(id)
	jcs, _ := h.jk.ListComments(h.ctx, id)
	m := jcs[len(jcs)-1]
	if len(m.Attachments) != 1 || m.Attachments[0].FileName != "foto.png" {
		t.Fatalf("the png becomes a file of the comment: %+v", m.Attachments)
	}
	want := fmt.Sprintf("Fotos ![file:%d]", m.Attachments[0].FileID)
	if !strings.Contains(m.Body, want) ||
		!strings.Contains(m.Body, "📎 imagen.webp (no se pudo copiar: Jiku no admite este tipo de archivo o su tamaño) — [ver en GitHub]("+anim+")") ||
		!strings.Contains(m.Body, "📎 datos.pdf (no se pudo copiar: GitHub no permitió descargarlo)") {
		t.Fatalf("comment mirror:\n%s", m.Body)
	}
	h.quiet(id)
}

func TestAttachmentsRoundTripWithoutCopies(t *testing.T) {
	h := newHarness(t)
	img := h.jk.personFile("plano.png", "image/png", []byte("PNG"))
	doc := h.jk.personFile("pliego_final.pdf", "application/pdf", []byte("PDF"))
	r := h.jk.newReq(7, "Ida y vuelta", fmt.Sprintf("Plano: ![file:%d]\n\nPliego: [file:%d]", img, doc), "", "public")
	h.jk.editReq(r.ID, func(x *jikuReq) { x.Attachments = h.jk.attachments([]int64{img, doc}) })
	h.syncReq(r.ID)
	is := h.issueOf(r.ID)

	// Someone edits the text on GitHub, keeping the image and the document's note: Jiku gets the
	// text, and both references, without copying anything.
	h.clk.tick()
	h.gh.edit(is.NodeID, func(i *github.Issue) {
		i.Body = strings.Replace(i.Body, "Plano:", "Plano actualizado:", 1)
	})
	h.jk.takeWrites()
	h.syncReq(r.ID)
	for _, w := range h.jk.takeWrites() {
		if strings.HasPrefix(w, "upload") || strings.HasPrefix(w, "req.fileIds") {
			t.Fatalf("the file already in Jiku must be referenced, not copied: %v", w)
		}
	}
	got, _ := h.jk.GetRequirement(h.ctx, r.ID)
	if got.Description != fmt.Sprintf("Plano actualizado: ![file:%d]\n\nPliego: [file:%d]", img, doc) {
		t.Fatalf("description: %q", got.Description)
	}
	h.quiet(r.ID)
}
