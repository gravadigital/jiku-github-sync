package creds

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gravadigital/jiku-github-sync/internal/mapping"
)

func mustParse(t *testing.T, y string) *mapping.File {
	t.Helper()
	f, err := mapping.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

const twoClients = `
credentials:
  client-a: { token: { env: TOKEN_A } }
  client-b: { token: { file: /secrets/b } }
projects:
  - { jikuProject: 1, repos: [a/x], defaultRepo: a/x, credentials: client-a }
  - { jikuProject: 2, repos: [b/y], defaultRepo: b/y, credentials: client-b }
  - { jikuProject: 3, repos: [a/z], defaultRepo: a/z, credentials: client-a }
`

// whoami returns which token a client sends.
func whoami(t *testing.T, srv *httptest.Server, r *Registry, f *mapping.File, p int64) string {
	t.Helper()
	is, err := r.For(f, f.Project(p)).GetIssue(context.Background(), "o/r", 1)
	if err != nil {
		return "error: " + err.Error()
	}
	return is.Title
}

func echoServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"number":1,"title":%q}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
}

func TestEachProjectUsesItsClientsToken(t *testing.T) {
	srv := echoServer()
	defer srv.Close()
	env := map[string]string{"TOKEN_A": "tok-a"}
	files := map[string]string{"/secrets/b": "tok-b\n"}
	r := New(Options{BaseURL: srv.URL, Getenv: func(k string) string { return env[k] },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("no file")
		}})
	f := mustParse(t, twoClients)
	if err := r.Load(f); err != nil {
		t.Fatal(err)
	}
	if got := whoami(t, srv, r, f, 1); got != "tok-a" {
		t.Fatalf("project 1: %s", got)
	}
	if got := whoami(t, srv, r, f, 2); got != "tok-b" {
		t.Fatalf("project 2 (file secret, trimmed): %s", got)
	}
	if r.For(f, f.Project(1)) != r.For(f, f.Project(3)) {
		t.Fatal("projects of the same client share one client (one rate limit)")
	}
	// A reload where client-a did not change keeps its client; a broken reload changes nothing.
	before := r.For(f, f.Project(1))
	env["TOKEN_A"] = "tok-a"
	if err := r.Load(f); err != nil || r.For(f, f.Project(1)) != before {
		t.Fatal("an unchanged credential keeps its client")
	}
	delete(files, "/secrets/b")
	if err := r.Load(f); err == nil || !strings.Contains(err.Error(), "client-b") {
		t.Fatalf("missing secret must fail: %v", err)
	}
	if got := whoami(t, srv, r, f, 2); got != "tok-b" {
		t.Fatalf("after a failed reload the previous credentials keep working: %s", got)
	}
}

func TestDefaultTokenAndMissingCredentials(t *testing.T) {
	srv := echoServer()
	defer srv.Close()
	f := mustParse(t, "projects:\n  - { jikuProject: 1, repos: [a/x], defaultRepo: a/x }\n")
	r := New(Options{BaseURL: srv.URL})
	if err := r.Load(f); err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("no credentials at all must fail: %v", err)
	}
	if got := whoami(t, srv, r, f, 1); !strings.HasPrefix(got, "error:") {
		t.Fatalf("a project without a client fails its calls instead of panicking: %s", got)
	}
	r = New(Options{BaseURL: srv.URL, DefaultToken: "tok-default"})
	if err := r.Load(f); err != nil || whoami(t, srv, r, f, 1) != "tok-default" {
		t.Fatal("projects naming no credentials use GITHUB_TOKEN")
	}
}

func TestAppInstallationPerOrganization(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/app/installations/") {
			id := strings.Split(r.URL.Path, "/")[3]
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"inst-%s","expires_at":"2099-01-01T00:00:00Z"}`, id)
			return
		}
		fmt.Fprintf(w, `{"number":1,"title":%q}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	defer srv.Close()
	f := mustParse(t, `
credentials:
  client-c: { app: { installations: { org-one: 11, org-two: 22 } } }
projects:
  - { jikuProject: 1, repos: [org-one/x], defaultRepo: org-one/x, credentials: client-c }
  - { jikuProject: 2, repos: [Org-Two/y], defaultRepo: Org-Two/y, credentials: client-c }
`)
	if err := New(Options{BaseURL: srv.URL}).Load(f); err == nil || !strings.Contains(err.Error(), "GITHUB_APP_ID") {
		t.Fatalf("an app credential without the App's id and key must fail: %v", err)
	}
	r := New(Options{BaseURL: srv.URL, AppID: "999", AppKey: pemKey})
	if err := r.Load(f); err != nil {
		t.Fatal(err)
	}
	if got := whoami(t, srv, r, f, 1); got != "inst-11" {
		t.Fatalf("org-one: %s", got)
	}
	if got := whoami(t, srv, r, f, 2); got != "inst-22" {
		t.Fatalf("org-two (case-insensitive): %s", got)
	}
}
