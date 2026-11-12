// Package jiku is the connector's view of Jiku: the handful of queries and commands it uses,
// over the public bus contract (jiku-go). Nothing here needs a change in Jiku.
package jiku

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	jikugo "github.com/gravadigital/jiku-go"
)

// Requirement is the subset of a requirement the connector syncs.
type Requirement struct {
	ID              int64     `json:"id"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	Type            *string   `json:"type"`
	Priority        string    `json:"priority"`
	State           string    `json:"state"`
	ProjectID       int64     `json:"projectId"`
	CreatedBy       string    `json:"createdBy"`
	VisibilityLevel string    `json:"visibilityLevel"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	// Attachments are the files linked to the requirement (its description's).
	Attachments []Attachment `json:"attachments"`
}

// Attachment is a file linked to a requirement or a comment. Texts reference it as `[file:FileID]`
// (or `[attach:ID]` and older forms).
type Attachment struct {
	ID       int64  `json:"id"`
	FileID   int64  `json:"fileId"`
	FileName string `json:"fileName"`
	MimeType string `json:"mimeType"`
	FileSize int64  `json:"fileSize"`
}

// FileIDs returns the file ids of a list of attachments.
func FileIDs(as []Attachment) []int64 {
	out := make([]int64, 0, len(as))
	for _, a := range as {
		out = append(out, a.FileID)
	}
	return out
}

// TypeOrEmpty returns the type, "" for null.
func (r *Requirement) TypeOrEmpty() string {
	if r.Type == nil {
		return ""
	}
	return *r.Type
}

// Public reports whether the requirement is public.
func (r *Requirement) Public() bool { return r.VisibilityLevel == "public" }

// Comment is a requirement comment.
type Comment struct {
	ID              int64        `json:"id"`
	Body            string       `json:"body"`
	AuthorID        string       `json:"authorId"`
	VisibilityLevel string       `json:"visibilityLevel"`
	CreatedAt       time.Time    `json:"createdAt"`
	UpdatedAt       time.Time    `json:"updatedAt"`
	Attachments     []Attachment `json:"attachments"`
	Author          *struct {
		Name     string `json:"name"`
		Username string `json:"username"`
	} `json:"author"`
}

// AuthorName is the name to show for the comment's author.
func (c *Comment) AuthorName() string {
	if c.Author != nil && c.Author.Name != "" && c.Author.Name != c.AuthorID {
		return c.Author.Name
	}
	return "Alguien"
}

// NewRequirement is the payload of requirements.new.
type NewRequirement struct {
	Title           string  `json:"title"`
	Description     string  `json:"description"`
	ProjectID       int64   `json:"projectId"`
	Type            *string `json:"type,omitempty"`
	Priority        string  `json:"priority,omitempty"`
	State           string  `json:"state,omitempty"`
	VisibilityLevel string  `json:"visibilityLevel"`
	FileIDs         []int64 `json:"fileIds,omitempty"`
}

// File is a stored file's metadata.
type File struct {
	Name     string
	MimeType string
	Size     int64
}

// API is what the sync engine needs from Jiku. Client implements it over the bus; tests use a
// fake.
type API interface {
	// SelfID is the connector's own user id (the token's sub).
	SelfID() string
	GetRequirement(ctx context.Context, id int64) (*Requirement, error)
	// ListRequirements lists the requirements of a project matching the extra filter.
	ListRequirements(ctx context.Context, projectID int64, filter map[string]any) ([]*Requirement, error)
	CreateRequirement(ctx context.Context, in NewRequirement) (int64, error)
	// EditRequirement sends requirements.{id}.edit. Never send `tags`: it is a full replacement.
	// `fileIds`, when sent, is the full set of files linked to the requirement.
	EditRequirement(ctx context.Context, id int64, fields map[string]any) error
	ListComments(ctx context.Context, requirementID int64) ([]*Comment, error)
	// CreateComment links fileIDs (files the connector uploaded) to the new comment.
	CreateComment(ctx context.Context, requirementID int64, body, visibility string, fileIDs []int64) (int64, error)
	// EditComment replaces the body; a non-nil fileIDs is the full set of the comment's files.
	EditComment(ctx context.Context, requirementID, commentID int64, body string, fileIDs []int64) error
	// DownloadFile reads a stored file.
	DownloadFile(ctx context.Context, fileID int64) (*File, []byte, error)
	// UploadFile stores a file as the connector's; it can then be linked with fileIds.
	UploadFile(ctx context.Context, name, mimeType string, data []byte) (int64, error)
}

// ErrFileRejected means Jiku refuses the file (type not allowed or too large).
var ErrFileRejected = errors.New("jiku: file rejected")

// ErrNotFound means the requirement (or file) does not exist, or the connector cannot see it.
var ErrNotFound = errors.New("jiku: not found")

// Client implements API with jiku-go.
type Client struct {
	c    *jikugo.Client
	http *http.Client
}

// New wraps a connected jiku-go client.
func New(c *jikugo.Client) *Client {
	return &Client{c: c, http: &http.Client{Timeout: 2 * time.Minute}}
}

// SelfID implements API.
func (j *Client) SelfID() string { return j.c.UserID() }

var requirementFields = []string{"id", "title", "type", "priority", "state", "projectId", "createdBy", "visibilityLevel", "createdAt", "updatedAt"}

// GetRequirement implements API.
func (j *Client) GetRequirement(ctx context.Context, id int64) (*Requirement, error) {
	item, err := j.c.Get(ctx, "requirements", jikugo.Get{ID: id, Fields: requirementFields, Include: []string{"description", "attachments"}})
	if err != nil {
		if jikugo.IsCode(err, "requirement_not_found") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var r Requirement
	if err := item.Into(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRequirements implements API.
func (j *Client) ListRequirements(ctx context.Context, projectID int64, filter map[string]any) ([]*Requirement, error) {
	f := jikugo.F{"projectId": projectID}
	for k, v := range filter {
		f[k] = v
	}
	var out []*Requirement
	err := j.c.All(ctx, "requirements", jikugo.List{
		Filter: f, Fields: requirementFields, Include: []string{"description", "attachments"}, Sort: []string{"createdAt"},
	}, &out)
	return out, err
}

// CreateRequirement implements API.
func (j *Client) CreateRequirement(ctx context.Context, in NewRequirement) (int64, error) {
	data, err := j.c.Command(ctx, "requirements.new", in)
	if err != nil {
		return 0, err
	}
	return decodeID(data)
}

// EditRequirement implements API.
func (j *Client) EditRequirement(ctx context.Context, id int64, fields map[string]any) error {
	if _, ok := fields["tags"]; ok {
		return errors.New("jiku: the connector never edits tags")
	}
	_, err := j.c.Command(ctx, "requirements."+strconv.FormatInt(id, 10)+".edit", fields)
	return err
}

// ListComments implements API.
func (j *Client) ListComments(ctx context.Context, requirementID int64) ([]*Comment, error) {
	var out []*Comment
	err := j.c.All(ctx, "comments", jikugo.List{
		Filter:  jikugo.F{"entityType": "requirement", "entityId": requirementID},
		Include: []string{"author"},
		Sort:    []string{"createdAt"},
	}, &out)
	return out, err
}

// CreateComment implements API.
func (j *Client) CreateComment(ctx context.Context, requirementID int64, body, visibility string, fileIDs []int64) (int64, error) {
	payload := map[string]any{"comment": body, "visibilityLevel": visibility}
	if len(fileIDs) > 0 {
		payload["fileIds"] = fileIDs
	}
	data, err := j.c.Command(ctx, "requirements."+strconv.FormatInt(requirementID, 10)+".comment", payload)
	if err != nil {
		return 0, err
	}
	return decodeID(data)
}

// EditComment implements API.
func (j *Client) EditComment(ctx context.Context, requirementID, commentID int64, body string, fileIDs []int64) error {
	payload := map[string]any{"comment": body}
	if fileIDs != nil {
		payload["fileIds"] = fileIDs
	}
	_, err := j.c.Command(ctx, fmt.Sprintf("requirements.%d.comment.%d.edit", requirementID, commentID), payload)
	return err
}

// maxFileBytes bounds what the connector moves in memory.
const maxFileBytes = 100 << 20

// DownloadFile implements API: a signed download ticket, then the bytes.
func (j *Client) DownloadFile(ctx context.Context, fileID int64) (*File, []byte, error) {
	data, err := j.c.Command(ctx, fmt.Sprintf("files.%d.request-download", fileID), map[string]any{"disposition": "attachment"})
	if err != nil {
		if jikugo.IsCode(err, "file_not_found") || jikugo.IsCode(err, "file_not_available") {
			return nil, nil, fmt.Errorf("%w: file %d", ErrNotFound, fileID)
		}
		return nil, nil, err
	}
	var t struct {
		DownloadURL string `json:"downloadUrl"`
		FileName    string `json:"fileName"`
		MimeType    string `json:"mimeType"`
		FileSize    int64  `json:"fileSize"`
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, nil, fmt.Errorf("jiku: decoding the download ticket: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.DownloadURL, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := j.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("jiku: downloading file %d: %w", fileID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("jiku: downloading file %d: HTTP %d", fileID, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(b) > maxFileBytes {
		return nil, nil, fmt.Errorf("jiku: file %d is larger than %d bytes", fileID, maxFileBytes)
	}
	return &File{Name: t.FileName, MimeType: t.MimeType, Size: int64(len(b))}, b, nil
}

// UploadFile implements API: a signed upload URL, then the bytes.
func (j *Client) UploadFile(ctx context.Context, name, mimeType string, data []byte) (int64, error) {
	out, err := j.c.Command(ctx, "files.request-upload", map[string]any{"fileName": name, "mimeType": mimeType, "fileSize": len(data)})
	if err != nil {
		if jikugo.IsCode(err, "file_type_not_allowed") || jikugo.IsCode(err, "file_too_large") {
			return 0, fmt.Errorf("%w: %w", ErrFileRejected, err)
		}
		return 0, err
	}
	var t struct {
		ID        int64  `json:"id"`
		UploadURL string `json:"uploadUrl"`
	}
	if err := json.Unmarshal(out, &t); err != nil {
		return 0, fmt.Errorf("jiku: decoding the upload ticket: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, t.UploadURL, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", mimeType)
	req.ContentLength = int64(len(data))
	resp, err := j.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("jiku: uploading %s: %w", name, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, fmt.Errorf("jiku: uploading %s: HTTP %d", name, resp.StatusCode)
	}
	return t.ID, nil
}

func decodeID(data []byte) (int64, error) {
	var v struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return 0, fmt.Errorf("jiku: decoding the created id: %w", err)
	}
	if v.ID == 0 {
		return 0, errors.New("jiku: the command returned no id")
	}
	return v.ID, nil
}
