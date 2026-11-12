package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// GitHub has no documented API for issue attachments. The GitHub CLI (`gh ... --attach`, since
// 2.99) uploads images and videos with a token to uploads.github.com/user-attachments/assets,
// which answers the asset's URL; the connector does the same. Other files (PDFs, documents) are
// only accepted from a browser session.

// AttachableTypes are the media types GitHub accepts as token-uploaded attachments.
var AttachableTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"video/mp4": true, "video/quicktime": true, "video/webm": true,
}

// MaxAttachmentBytes is GitHub's size limit for images (videos may go further on paid plans).
const MaxAttachmentBytes = 10 << 20

// maxDownloadBytes bounds what a download keeps in memory.
const maxDownloadBytes = 100 << 20

// UploadAsset uploads an image or video for the issues of repo and returns its URL
// (https://github.com/user-attachments/assets/...), to embed in a body or a comment.
func (c *Client) UploadAsset(ctx context.Context, repo, name, contentType string, data []byte) (string, error) {
	id, err := c.repoID(ctx, repo)
	if err != nil {
		return "", err
	}
	q := url.Values{"name": {name}, "content_type": {contentType}, "repository_id": {strconv.FormatInt(id, 10)}}
	var out struct {
		URL string `json:"url"`
	}
	if err := c.raw(ctx, http.MethodPost, c.uploads+"/user-attachments/assets?"+q.Encode(), contentType, data, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", fmt.Errorf("github: the upload of %s returned no url", name)
	}
	return out.URL, nil
}

func (c *Client) repoID(ctx context.Context, repo string) (int64, error) {
	key := strings.ToLower(repo)
	c.repoMu.Lock()
	id, ok := c.repoIDs[key]
	c.repoMu.Unlock()
	if ok {
		return id, nil
	}
	r, err := c.GetRepo(ctx, repo)
	if err != nil {
		return 0, err
	}
	c.repoMu.Lock()
	if c.repoIDs == nil {
		c.repoIDs = map[string]int64{}
	}
	c.repoIDs[key] = r.ID
	c.repoMu.Unlock()
	return r.ID, nil
}

// raw sends a non-JSON body (one try: uploads are not retried blindly).
func (c *Client) raw(ctx context.Context, method, u, contentType string, data []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "jiku-github-sync")
	if c.auth != nil {
		tok, err := c.auth.Token(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var m struct {
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(b, &m)
		msg := m.Message
		for _, e := range m.Errors {
			msg += "; " + e.Message
		}
		return &APIError{Status: resp.StatusCode, Method: method, Path: "/user-attachments/assets", Message: msg}
	}
	return json.Unmarshal(b, out)
}

// Download reads an attachment. The token is only sent to GitHub itself (signed URLs such as
// private-user-images.githubusercontent.com carry their own authorization), and Go drops it on a
// redirect to another host.
func (c *Client) Download(ctx context.Context, u string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "jiku-github-sync")
	if c.auth != nil && c.sendsToken(req.URL) {
		tok, err := c.auth.Token(ctx)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", &APIError{Status: resp.StatusCode, Method: http.MethodGet, Path: req.URL.Host + req.URL.Path, Message: "download failed"}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > maxDownloadBytes {
		return nil, "", fmt.Errorf("github: %s is larger than %d bytes", req.URL.Path, maxDownloadBytes)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return b, ct, nil
}

func (c *Client) sendsToken(u *url.URL) bool {
	if u.Scheme != "https" && !strings.HasPrefix(c.base, "http://") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "github.com" || host == "api.github.com" {
		return true
	}
	b, err := url.Parse(c.base)
	return err == nil && strings.EqualFold(b.Host, u.Host)
}

// IssueHTML returns an issue's body rendered by GitHub: in private and internal repos its
// attachment links come signed (valid a few minutes), which is how they can be downloaded.
func (c *Client) IssueHTML(ctx context.Context, repo string, number int) (string, error) {
	var r struct {
		BodyHTML string `json:"body_html"`
	}
	_, err := c.doAccept(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil, &r, false, "application/vnd.github.full+json")
	return r.BodyHTML, err
}

// CommentHTML is IssueHTML for a comment.
func (c *Client) CommentHTML(ctx context.Context, repo string, id int64) (string, error) {
	var r struct {
		BodyHTML string `json:"body_html"`
	}
	_, err := c.doAccept(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, id), nil, &r, false, "application/vnd.github.full+json")
	return r.BodyHTML, err
}
