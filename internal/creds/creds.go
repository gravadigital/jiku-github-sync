// Package creds turns the mapping's credentials (one per client) into GitHub clients, and gives
// each project the client of its credentials. Each client keeps its own write spacing and
// rate-limit handling, as GitHub counts its limits per token / per App installation.
package creds

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
)

// Options are the service-wide settings.
type Options struct {
	BaseURL       string
	WriteInterval time.Duration
	Logger        *slog.Logger
	// DefaultToken is GITHUB_TOKEN: the credential of projects that name none.
	DefaultToken string
	// AppID and AppKey are the connector's GitHub App (only needed by `app` credentials).
	AppID  string
	AppKey []byte
	// Getenv and ReadFile read the secrets (os.Getenv / os.ReadFile; replaceable in tests).
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
}

// Registry holds one GitHub client per credential (per organization for an App).
type Registry struct {
	o  Options
	mu sync.RWMutex
	// clients by key: credential name, or "name/org" for an App installation.
	clients map[string]*github.Client
	// built remembers the definition behind each key, so a reload keeps the clients (and their
	// rate-limit state) of credentials that did not change.
	built map[string]string
}

// New returns an empty registry; call Load with the mapping.
func New(o Options) *Registry {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.ReadFile == nil {
		o.ReadFile = os.ReadFile
	}
	return &Registry{o: o, clients: map[string]*github.Client{}, built: map[string]string{}}
}

func key(name, org string) string {
	if org == "" {
		return name
	}
	return name + "/" + strings.ToLower(org)
}

// Load (re)builds the clients of a mapping. On error nothing changes, so a bad reload keeps the
// previous credentials working.
func (r *Registry) Load(f *mapping.File) error {
	clients := map[string]*github.Client{}
	built := map[string]string{}
	var errs []error
	var app *github.App

	r.mu.RLock()
	reuse := func(k, def string, mk func() (*github.Client, error)) {
		if c, ok := r.clients[k]; ok && r.built[k] == def {
			clients[k], built[k] = c, def
			return
		}
		c, err := mk()
		if err != nil {
			errs = append(errs, fmt.Errorf("credentials %s: %w", k, err))
			return
		}
		clients[k], built[k] = c, def
	}
	if r.o.DefaultToken != "" {
		reuse(mapping.DefaultCredential, "token:"+r.o.DefaultToken, func() (*github.Client, error) {
			return r.client(github.StaticToken(r.o.DefaultToken)), nil
		})
	}
	for name, c := range f.Credentials {
		switch {
		case c.Token != nil:
			tok, err := r.secret(c.Token)
			if err != nil {
				errs = append(errs, fmt.Errorf("credentials %s: %w", name, err))
				continue
			}
			reuse(key(name, ""), "token:"+tok, func() (*github.Client, error) { return r.client(github.StaticToken(tok)), nil })
		case c.App != nil:
			if app == nil {
				var err error
				if app, err = r.app(); err != nil {
					errs = append(errs, fmt.Errorf("credentials %s: %w", name, err))
					continue
				}
			}
			for org, id := range c.App.Installations {
				id := id
				reuse(key(name, org), fmt.Sprintf("app:%s:%d", r.o.AppID, id), func() (*github.Client, error) {
					return r.client(app.Installation(id)), nil
				})
			}
		}
	}
	r.mu.RUnlock()

	// Every project must resolve to a client.
	for _, p := range f.Projects {
		if _, ok := clients[resolveKey(f, p)]; !ok {
			if p.CredentialName() == mapping.DefaultCredential {
				errs = append(errs, fmt.Errorf("project %d names no credentials and GITHUB_TOKEN is not set", p.JikuProject))
			} else if len(errs) == 0 {
				errs = append(errs, fmt.Errorf("project %d: no client for credentials %q", p.JikuProject, p.CredentialName()))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	r.mu.Lock()
	r.clients, r.built = clients, built
	r.mu.Unlock()
	return nil
}

func resolveKey(f *mapping.File, p *mapping.Project) string {
	name := p.CredentialName()
	if c := f.Credentials[name]; c != nil && c.App != nil {
		return key(name, mapping.Owner(p.DefaultRepo))
	}
	return key(name, "")
}

// For returns the client of a project of the given mapping. It never returns nil: a project
// without a client gets one whose every call fails, so nothing panics on a misconfiguration.
func (r *Registry) For(f *mapping.File, p *mapping.Project) *github.Client {
	r.mu.RLock()
	c := r.clients[resolveKey(f, p)]
	r.mu.RUnlock()
	if c == nil {
		return r.client(failing{fmt.Errorf("project %d has no GitHub credentials", p.JikuProject)})
	}
	return c
}

// Each calls fn for every credential's client (name, or name/org for an App).
func (r *Registry) Each(fn func(name string, c *github.Client)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k, c := range r.clients {
		fn(k, c)
	}
}

// IsApp reports whether a client key belongs to an App installation.
func IsApp(name string) bool { return strings.Contains(name, "/") }

func (r *Registry) client(ts github.TokenSource) *github.Client {
	return github.New(github.Options{BaseURL: r.o.BaseURL, Auth: ts, WriteInterval: r.o.WriteInterval, Logger: r.o.Logger})
}

func (r *Registry) secret(s *mapping.Secret) (string, error) {
	if s.Env != "" {
		v := strings.TrimSpace(r.o.Getenv(s.Env))
		if v == "" {
			return "", fmt.Errorf("environment variable %s is empty", s.Env)
		}
		return v, nil
	}
	b, err := r.o.ReadFile(s.File)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", s.File, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s is empty", s.File)
	}
	return v, nil
}

func (r *Registry) app() (*github.App, error) {
	if r.o.AppID == "" || len(r.o.AppKey) == 0 {
		return nil, errors.New("an app credential needs GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY_FILE (or _B64)")
	}
	k, err := github.ParsePrivateKey(r.o.AppKey)
	if err != nil {
		return nil, err
	}
	return &github.App{ID: r.o.AppID, PrivateKey: k, BaseURL: r.o.BaseURL}, nil
}

// DecodeB64 is a helper for GITHUB_APP_PRIVATE_KEY_B64.
func DecodeB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimSpace(s))
}

type failing struct{ err error }

func (f failing) Token(context.Context) (string, error) { return "", f.err }
