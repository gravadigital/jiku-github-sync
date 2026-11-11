package webhook

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var secret = []byte("s3cret")

func deliver(t *testing.T, h *Handler, event, body, sig string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func handler(got *[]Trigger) *Handler {
	return &Handler{Secret: secret, Enqueue: func(t Trigger) { *got = append(*got, t) },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestSignedIssueEvent(t *testing.T) {
	var got []Trigger
	h := handler(&got)
	body := `{"action":"edited","issue":{"number":12},"repository":{"full_name":"acme/api"}}`
	if code := deliver(t, h, "issues", body, Sign(secret, []byte(body))); code != http.StatusAccepted {
		t.Fatalf("code %d", code)
	}
	if len(got) != 1 || got[0].Repo != "acme/api" || got[0].Number != 12 {
		t.Fatalf("trigger: %+v", got)
	}
}

func TestBadSignatureRejected(t *testing.T) {
	var got []Trigger
	h := handler(&got)
	body := `{"issue":{"number":1},"repository":{"full_name":"a/b"}}`
	for _, sig := range []string{"", "sha256=00", Sign([]byte("other"), []byte(body)), "sha1=abc"} {
		if code := deliver(t, h, "issues", body, sig); code != http.StatusUnauthorized {
			t.Fatalf("sig %q: code %d", sig, code)
		}
	}
	if len(got) != 0 {
		t.Fatal("nothing must be queued")
	}
	if Verify(nil, []byte(body), Sign(nil, []byte(body))) {
		t.Fatal("an empty secret never verifies")
	}
}

func TestProjectItemAndIgnoredEvents(t *testing.T) {
	var got []Trigger
	h := handler(&got)
	item := `{"projects_v2_item":{"content_node_id":"I_x","content_type":"Issue"}}`
	draft := `{"projects_v2_item":{"content_node_id":"DI_x","content_type":"DraftIssue"}}`
	pr := `{"issue":{"number":3,"pull_request":{}},"repository":{"full_name":"a/b"}}`
	for ev, body := range map[string]string{"projects_v2_item": item, "ping": `{"zen":"x"}`} {
		if code := deliver(t, h, ev, body, Sign(secret, []byte(body))); code != http.StatusAccepted {
			t.Fatalf("%s: %d", ev, code)
		}
	}
	deliver(t, h, "projects_v2_item", draft, Sign(secret, []byte(draft)))
	deliver(t, h, "issue_comment", pr, Sign(secret, []byte(pr)))
	if len(got) != 1 || got[0].Node != "I_x" {
		t.Fatalf("only the issue item should queue: %+v", got)
	}
}
