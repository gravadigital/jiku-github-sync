package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAppInstallationTokenAndRenewal(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	parsed, err := ParsePrivateKey(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var mints atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/77/access_tokens":
			// The JWT must be RS256-signed by the App's key and name the App as issuer.
			jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			parts := strings.Split(jwt, ".")
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
				t.Errorf("bad JWT signature: %v", err)
			}
			raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims map[string]any
			_ = json.Unmarshal(raw, &claims)
			if claims["iss"] != "12345" {
				t.Errorf("iss: %v", claims["iss"])
			}
			n := mints.Add(1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":%q}`, n, now.Add(time.Hour).Format(time.RFC3339))
		case "/repos/org/r/issues/1":
			fmt.Fprintf(w, `{"number":1,"title":%q}`, r.Header.Get("Authorization"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	app := &App{ID: "12345", PrivateKey: parsed, BaseURL: srv.URL, now: func() time.Time { return now }}
	c := New(Options{BaseURL: srv.URL, Auth: app.Installation(77)})
	is, err := c.GetIssue(context.Background(), "org/r", 1)
	if err != nil || is.Title != "Bearer ghs_1" {
		t.Fatalf("first token: %q %v", is.Title, err)
	}
	if is, _ = c.GetIssue(context.Background(), "org/r", 1); is.Title != "Bearer ghs_1" || mints.Load() != 1 {
		t.Fatal("the installation token is cached")
	}
	now = now.Add(56 * time.Minute) // less than 5 minutes left
	if is, _ = c.GetIssue(context.Background(), "org/r", 1); is.Title != "Bearer ghs_2" {
		t.Fatalf("renewed before expiring, got %q", is.Title)
	}
}

func TestParsePrivateKeyRejectsGarbage(t *testing.T) {
	if _, err := ParsePrivateKey([]byte("not a key")); err == nil {
		t.Fatal("expected an error")
	}
}
