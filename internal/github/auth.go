package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TokenSource gives the token of each request. It is called on every request and must cache.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a fixed token (a user or service account token).
type StaticToken string

// Token implements TokenSource.
func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }

// App is the connector's GitHub App: its id (or client id) and private key. One App serves every
// client; each client's organization has its own installation.
type App struct {
	ID         string
	PrivateKey *rsa.PrivateKey
	BaseURL    string
	HTTP       *http.Client
	now        func() time.Time
}

// ParsePrivateKey reads the App's PEM private key (PKCS#1, as GitHub hands it out, or PKCS#8).
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github app: the private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app: private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app: the private key is not RSA")
	}
	return rk, nil
}

// JWT is the App's own short-lived token (RS256), used only to get installation tokens.
func (a *App) JWT() (string, error) {
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	// iat 60 s in the past absorbs clock drift; GitHub accepts at most 10 minutes of life.
	claims, err := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.ID})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.PrivateKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Installation returns the token source of one installation of the App.
func (a *App) Installation(id int64) *InstallationToken {
	return &InstallationToken{app: a, id: id}
}

// InstallationToken is an App installation's token, renewed before it expires (they last 1 h).
type InstallationToken struct {
	app *App
	id  int64

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token implements TokenSource.
func (t *InstallationToken) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.app.now != nil {
		now = t.app.now()
	}
	if t.token != "" && now.Before(t.expires.Add(-5*time.Minute)) {
		return t.token, nil
	}
	jwt, err := t.app.JWT()
	if err != nil {
		return "", err
	}
	base := strings.TrimRight(t.app.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", base, t.id), bytes.NewReader(nil))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "jiku-github-sync")
	hc := t.app.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("github app: installation %d: %w", t.id, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github app: installation %d token: HTTP %d: %s", t.id, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("github app: installation %d: unexpected answer", t.id)
	}
	t.token, t.expires = out.Token, out.ExpiresAt
	return t.token, nil
}
