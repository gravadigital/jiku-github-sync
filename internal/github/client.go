// Package github is the connector's small GitHub client: the REST endpoints for issues and
// comments, and the GraphQL ones for Projects v2. It handles pagination, rate limits (primary and
// secondary) and spaces out content-creating writes, which GitHub limits separately (80 per
// minute, 500 per hour).
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the public GitHub API.
const DefaultBaseURL = "https://api.github.com"

// ErrNotFound is a 404 (or a 410 for a deleted issue).
var ErrNotFound = errors.New("github: not found")

// Client talks to the GitHub API with a token.
type Client struct {
	base    string
	uploads string
	auth    TokenSource
	http    *http.Client
	log     *slog.Logger

	// writeInterval is the minimum gap between two content-creating requests.
	writeInterval time.Duration
	writeMu       sync.Mutex
	lastWrite     time.Time

	repoMu  sync.Mutex
	repoIDs map[string]int64

	// maxWait bounds how long a single call sleeps for a rate limit before failing.
	maxWait time.Duration
	sleep   func(context.Context, time.Duration) error
}

// Options configures a Client.
type Options struct {
	BaseURL string
	// Token is a fixed token; Auth (an App installation, say) takes precedence over it.
	Token         string
	Auth          TokenSource
	WriteInterval time.Duration
	HTTPClient    *http.Client
	Logger        *slog.Logger
	// MaxRateLimitWait bounds the sleep on a rate limit (default 5 minutes).
	MaxRateLimitWait time.Duration
	// UploadsURL is where attachments are uploaded (default: uploads.github.com for the public
	// API, BaseURL otherwise).
	UploadsURL string
}

// New builds a Client.
func New(o Options) *Client {
	c := &Client{
		base:          strings.TrimRight(o.BaseURL, "/"),
		auth:          o.Auth,
		http:          o.HTTPClient,
		log:           o.Logger,
		writeInterval: o.WriteInterval,
		maxWait:       o.MaxRateLimitWait,
		sleep:         sleepCtx,
	}
	if c.base == "" {
		c.base = DefaultBaseURL
	}
	c.uploads = strings.TrimRight(o.UploadsURL, "/")
	if c.uploads == "" {
		c.uploads = c.base
		if c.base == DefaultBaseURL {
			c.uploads = "https://uploads.github.com"
		}
	}
	if c.auth == nil && o.Token != "" {
		c.auth = StaticToken(o.Token)
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	if c.log == nil {
		c.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.maxWait == 0 {
		c.maxWait = 5 * time.Minute
	}
	return c
}

// APIError is a non-2xx answer.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}

// Is makes errors.Is(err, ErrNotFound) work for 404 and 410.
func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && (e.Status == http.StatusNotFound || e.Status == http.StatusGone)
}

// do sends one request (with retries on rate limits) and decodes the JSON answer into out.
// It returns the response headers, for pagination.
func (c *Client) do(ctx context.Context, method, path string, body any, out any, creates bool) (http.Header, error) {
	return c.doAccept(ctx, method, path, body, out, creates, "application/vnd.github+json")
}

// doAccept is do with another media type (e.g. the one that adds body_html).
func (c *Client) doAccept(ctx context.Context, method, path string, body any, out any, creates bool, accept string) (http.Header, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	if creates {
		if err := c.waitWriteSlot(ctx); err != nil {
			return nil, err
		}
	}
	url := path
	if !strings.HasPrefix(path, "http") {
		url = c.base + path
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "jiku-github-sync")
		if c.auth != nil {
			tok, err := c.auth.Token(ctx)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if wait, limited := rateLimitWait(resp); limited && attempt < 5 {
			if wait > c.maxWait {
				return nil, fmt.Errorf("github: rate limited for %s (more than %s)", wait, c.maxWait)
			}
			c.log.Warn("github rate limit, waiting", "wait", wait.String(), "path", path)
			if err := c.sleep(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode >= 500 && attempt < 3 {
			if err := c.sleep(ctx, time.Duration(attempt+1)*2*time.Second); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			var m struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(data, &m)
			if m.Message == "" {
				m.Message = strings.TrimSpace(string(data))
			}
			return resp.Header, &APIError{Status: resp.StatusCode, Method: method, Path: path, Message: m.Message}
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return resp.Header, fmt.Errorf("github: decoding %s %s: %w", method, path, err)
			}
		}
		return resp.Header, nil
	}
}

// rateLimitWait reads GitHub's rate limit signals: Retry-After (secondary limit) or an exhausted
// primary limit (x-ratelimit-remaining 0 with x-ratelimit-reset).
func rateLimitWait(resp *http.Response) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if s, err := strconv.Atoi(ra); err == nil {
			return time.Duration(s)*time.Second + time.Second, true
		}
	}
	if resp.Header.Get("X-Ratelimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
			d := time.Until(time.Unix(reset, 0)) + time.Second
			if d < time.Second {
				d = time.Second
			}
			return d, true
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return time.Minute, true
	}
	return 0, false
}

func (c *Client) waitWriteSlot(ctx context.Context) error {
	if c.writeInterval <= 0 {
		return nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if wait := time.Until(c.lastWrite.Add(c.writeInterval)); wait > 0 {
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
	c.lastWrite = time.Now()
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// getAll follows the Link header, appending every page into out (a pointer to a slice).
func getAll[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var all []T
	next := path
	for next != "" {
		var page []T
		h, err := c.do(ctx, http.MethodGet, next, nil, &page, false)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		next = ""
		if m := linkNext.FindStringSubmatch(h.Get("Link")); m != nil {
			next = m[1]
		}
	}
	return all, nil
}

// graphql runs a GraphQL query and decodes `data` into out.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any, creates bool) error {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/graphql", map[string]any{"query": query, "variables": vars}, &resp, creates); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			if e.Type == "NOT_FOUND" {
				return &APIError{Status: http.StatusNotFound, Method: "POST", Path: "/graphql", Message: e.Message}
			}
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("github graphql: %s", strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(resp.Data, out)
	}
	return nil
}
