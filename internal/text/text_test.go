package text

import (
	"testing"
	"time"
)

func TestIssueBodyRoundTrip(t *testing.T) {
	b := IssueBody("Descripción\n\n| a | b |\n|---|---|\n", 42)
	if id, ok := RequirementID(b); !ok || id != 42 {
		t.Fatalf("marker not found: %q", b)
	}
	if Normalize(b) != "Descripción\n\n| a | b |\n|---|---|" {
		t.Fatalf("normalize: %q", Normalize(b))
	}
	if Hash(b) != Hash("Descripción\r\n\r\n| a | b |\r\n|---|---|  ") {
		t.Fatal("hash must ignore markers, CRLF and trailing space")
	}
}

func TestCommentsRoundTrip(t *testing.T) {
	d := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	gh := GitHubComment("Ana_Pérez", "hola", 77, &d)
	if id, ok := JikuCommentID(gh); !ok || id != 77 {
		t.Fatalf("jiku comment id: %q", gh)
	}
	if !HasMarker(gh) {
		t.Fatal("GitHub comments written by the connector carry the marker")
	}
	jk := JikuComment("octo", "https://github.com/o/r/issues/1#issuecomment-991", "texto", &d)
	if id, ok := GitHubCommentID(jk); !ok || id != 991 {
		t.Fatalf("github comment id: %q", jk)
	}
	if want := "**@octo** comentó en [GitHub](https://github.com/o/r/issues/1#issuecomment-991) el 2026-11-03:\n\ntexto"; jk != want {
		t.Fatalf("got %q", jk)
	}
	if _, ok := GitHubCommentID("texto sin atribución\n#issuecomment-1)"); ok {
		t.Fatal("only the first line counts")
	}
}

func TestNoticeIsMarked(t *testing.T) {
	if !HasMarker(Notice("x")) {
		t.Fatal("notices must be recognisable")
	}
	if Expand("Marcado como {option} por @{login}", "ana", "Blocked") != "Marcado como Blocked por @ana" {
		t.Fatal("expand")
	}
}

func TestComparableIgnoresAttachments(t *testing.T) {
	jiku := "Ver ![file:12]\n\ny [file:13] y ![attach:4] [x](/api/files/9/preview)"
	gh := "Ver ![captura.png](https://github.com/user-attachments/assets/ab-12)\n\ny 📎 informe.pdf (adjunto en Jiku) y <img alt=\"a\" src=\"https://github.com/user-attachments/assets/cd-34\" />\nhttps://github.com/user-attachments/assets/ef-56\n"
	if Comparable(jiku) != "Ver y y" || Comparable(gh) != Comparable(jiku) {
		t.Fatalf("%q / %q", Comparable(jiku), Comparable(gh))
	}
	if Comparable("Ver A") == Comparable("Ver B") {
		t.Fatal("text differences still count")
	}
}
