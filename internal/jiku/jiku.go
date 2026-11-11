// Package jiku is the connector's view of Jiku: the handful of queries and commands it uses,
// over the public bus contract (jiku-go). Nothing here needs a change in Jiku.
package jiku

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	ID              int64     `json:"id"`
	Body            string    `json:"body"`
	AuthorID        string    `json:"authorId"`
	VisibilityLevel string    `json:"visibilityLevel"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
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
	EditRequirement(ctx context.Context, id int64, fields map[string]any) error
	ListComments(ctx context.Context, requirementID int64) ([]*Comment, error)
	CreateComment(ctx context.Context, requirementID int64, body, visibility string) (int64, error)
	EditComment(ctx context.Context, requirementID, commentID int64, body string) error
}

// ErrNotFound means the requirement does not exist (or the connector cannot see it).
var ErrNotFound = errors.New("jiku: not found")

// Client implements API with jiku-go.
type Client struct {
	c *jikugo.Client
}

// New wraps a connected jiku-go client.
func New(c *jikugo.Client) *Client { return &Client{c: c} }

// SelfID implements API.
func (j *Client) SelfID() string { return j.c.UserID() }

var requirementFields = []string{"id", "title", "type", "priority", "state", "projectId", "createdBy", "visibilityLevel", "createdAt", "updatedAt"}

// GetRequirement implements API.
func (j *Client) GetRequirement(ctx context.Context, id int64) (*Requirement, error) {
	item, err := j.c.Get(ctx, "requirements", jikugo.Get{ID: id, Fields: requirementFields, Include: []string{"description"}})
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
		Filter: f, Fields: requirementFields, Include: []string{"description"}, Sort: []string{"createdAt"},
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
func (j *Client) CreateComment(ctx context.Context, requirementID int64, body, visibility string) (int64, error) {
	data, err := j.c.Command(ctx, "requirements."+strconv.FormatInt(requirementID, 10)+".comment",
		map[string]any{"comment": body, "visibilityLevel": visibility})
	if err != nil {
		return 0, err
	}
	return decodeID(data)
}

// EditComment implements API.
func (j *Client) EditComment(ctx context.Context, requirementID, commentID int64, body string) error {
	_, err := j.c.Command(ctx, fmt.Sprintf("requirements.%d.comment.%d.edit", requirementID, commentID),
		map[string]any{"comment": body})
	return err
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
