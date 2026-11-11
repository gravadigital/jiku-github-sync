// Package state keeps the connector's own state: which requirement is which issue, what was last
// synced on each side, and the polling cursors. It lives in the connector's space of Jiku's KV
// (the same place jiku-email-tickets keeps its state), never in Jiku's data.
//
// The KV has no listing, so the store keeps its own index: one key per mapped project with the
// requirement ids it has links for.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	jikugo "github.com/gravadigital/jiku-go"
)

// ErrNotFound is a missing key.
var ErrNotFound = errors.New("state: not found")

// KV is the raw key-value storage.
type KV interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
}

// JikuKV adapts the jiku-go key-value space.
type JikuKV struct{ S *jikugo.KV }

// Get implements KV.
func (k JikuKV) Get(ctx context.Context, key string) ([]byte, error) {
	e, err := k.S.Get(ctx, key)
	if errors.Is(err, jikugo.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return e.Value, nil
}

// Put implements KV.
func (k JikuKV) Put(ctx context.Context, key string, value []byte) error {
	_, err := k.S.Put(ctx, key, value)
	return err
}

// Delete implements KV.
func (k JikuKV) Delete(ctx context.Context, key string) error { return k.S.Delete(ctx, key) }

// Mem is an in-memory KV, for tests and dry runs.
type Mem struct {
	mu sync.Mutex
	m  map[string][]byte
}

// NewMem returns an empty in-memory KV.
func NewMem() *Mem { return &Mem{m: map[string][]byte{}} }

// Get implements KV.
func (m *Mem) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.m[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

// Put implements KV.
func (m *Mem) Put(_ context.Context, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[key] = append([]byte(nil), value...)
	return nil
}

// Delete implements KV.
func (m *Mem) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, key)
	return nil
}

// Keys lists the keys (tests only).
func (m *Mem) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ks := make([]string, 0, len(m.m))
	for k := range m.m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Snapshot is what both sides looked like right after the last sync of a pair. A field that
// differs from it on one side changed on that side; on both, the most recent change wins.
type Snapshot struct {
	Title    string `json:"title"`
	BodyHash string `json:"bodyHash"`

	JikuState    string `json:"jikuState"`
	JikuPriority string `json:"jikuPriority"`
	JikuType     string `json:"jikuType"`

	GHOpen        bool   `json:"ghOpen"`
	GHStateReason string `json:"ghStateReason,omitempty"`
	GHStatus      string `json:"ghStatus,omitempty"`
	GHPriority    string `json:"ghPriority,omitempty"`
	GHType        string `json:"ghType,omitempty"`

	// LastOpenState is the last open Jiku state seen, to go back to when an issue is reopened.
	LastOpenState string `json:"lastOpenState,omitempty"`
}

// CommentLink pairs a Jiku comment with its GitHub comment.
type CommentLink struct {
	JikuID int64 `json:"j"`
	GHID   int64 `json:"g"`
	// Origin is the side the comment was written on: "jiku" or "github".
	Origin string `json:"o"`
	// SourceHash is the hash of the source's text at the last sync.
	SourceHash string `json:"h"`
	// Deleted marks a GitHub comment that was deleted (its Jiku mirror says so).
	Deleted bool `json:"d,omitempty"`
}

// Link is a requirement and its issue.
type Link struct {
	ReqID       int64  `json:"reqId"`
	JikuProject int64  `json:"jikuProject"`
	IssueNodeID string `json:"issueNodeId"`
	Repo        string `json:"repo"`
	Number      int    `json:"number"`
	// ItemID is the issue's item in the mapped Project ("" when there is none yet).
	ItemID string `json:"itemId,omitempty"`
	// Unlinked marks a pair that stopped syncing because the requirement became internal. It is
	// kept as a tombstone so the issue is not imported again, and is revived if the requirement
	// becomes public again.
	Unlinked bool          `json:"unlinked,omitempty"`
	Last     Snapshot      `json:"last"`
	Comments []CommentLink `json:"comments,omitempty"`
	SyncedAt time.Time     `json:"syncedAt"`
}

// IssueKey is "owner/repo#number", for logs.
func (l *Link) IssueKey() string { return fmt.Sprintf("%s#%d", l.Repo, l.Number) }

// ProjectState is the per-project bookkeeping.
type ProjectState struct {
	// StartedAt is when the connector started watching the project: issues and requirements
	// created before it are only synced by the initial sync.
	StartedAt time.Time `json:"startedAt"`
	// InitialSyncAt is when the initial sync was applied (zero if never).
	InitialSyncAt time.Time `json:"initialSyncAt,omitempty"`
	// Links are the requirement ids with a Link (the index the KV cannot give).
	Links []int64 `json:"links"`
	// IssueCursor and CommentCursor are per repo: the newest updated_at already processed.
	IssueCursor   map[string]time.Time `json:"issueCursor,omitempty"`
	CommentCursor map[string]time.Time `json:"commentCursor,omitempty"`
}

// Pending is an in-flight creation, written before the create call and removed after the link
// is stored, so a crash in between can be recovered without creating a duplicate.
type Pending struct {
	At time.Time `json:"at"`
	// Title is what was being created, to recognise it.
	Title string `json:"title"`
}

// Store is the typed state on top of a KV.
type Store struct {
	kv KV
	mu sync.Mutex // serialises read-modify-write of the project index
}

// New returns a Store.
func New(kv KV) *Store { return &Store{kv: kv} }

func (s *Store) getJSON(ctx context.Context, key string, v any) error {
	b, err := s.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (s *Store) putJSON(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.kv.Put(ctx, key, b)
}

// NodeKey turns a GitHub node id into a KV-safe token.
func NodeKey(nodeID string) string {
	return strings.NewReplacer("+", "-", "/", "_").Replace(nodeID)
}

func linkKey(reqID int64) string       { return fmt.Sprintf("link.r.%d", reqID) }
func issueKey(nodeID string) string    { return "link.i." + NodeKey(nodeID) }
func projectKey(p int64) string        { return fmt.Sprintf("project.%d", p) }
func pendingIssueKey(n string) string  { return "pending.i." + NodeKey(n) }
func pendingReqKey(reqID int64) string { return fmt.Sprintf("pending.r.%d", reqID) }

// Link returns the link of a requirement.
func (s *Store) Link(ctx context.Context, reqID int64) (*Link, error) {
	var l Link
	if err := s.getJSON(ctx, linkKey(reqID), &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// LinkByIssue returns the link of an issue (by node id).
func (s *Store) LinkByIssue(ctx context.Context, nodeID string) (*Link, error) {
	b, err := s.kv.Get(ctx, issueKey(nodeID))
	if err != nil {
		return nil, err
	}
	var reqID int64
	if err := json.Unmarshal(b, &reqID); err != nil {
		return nil, err
	}
	return s.Link(ctx, reqID)
}

// SaveLink stores a link, its issue index and the project index.
func (s *Store) SaveLink(ctx context.Context, l *Link) error {
	l.SyncedAt = time.Now().UTC()
	if err := s.putJSON(ctx, linkKey(l.ReqID), l); err != nil {
		return err
	}
	if err := s.putJSON(ctx, issueKey(l.IssueNodeID), l.ReqID); err != nil {
		return err
	}
	return s.UpdateProject(ctx, l.JikuProject, func(p *ProjectState) {
		for _, id := range p.Links {
			if id == l.ReqID {
				return
			}
		}
		p.Links = append(p.Links, l.ReqID)
	})
}

// Project returns a project's state (zero value when there is none yet).
func (s *Store) Project(ctx context.Context, p int64) (*ProjectState, error) {
	var ps ProjectState
	err := s.getJSON(ctx, projectKey(p), &ps)
	if errors.Is(err, ErrNotFound) {
		return &ProjectState{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &ps, nil
}

// UpdateProject applies fn to a project's state and stores it.
func (s *Store) UpdateProject(ctx context.Context, p int64, fn func(*ProjectState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.Project(ctx, p)
	if err != nil {
		return err
	}
	fn(ps)
	return s.putJSON(ctx, projectKey(p), ps)
}

// PendingIssue returns the in-flight creation of a requirement for an issue.
func (s *Store) PendingIssue(ctx context.Context, nodeID string) (*Pending, error) {
	var p Pending
	if err := s.getJSON(ctx, pendingIssueKey(nodeID), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// SetPendingIssue records that a requirement is about to be created for an issue.
func (s *Store) SetPendingIssue(ctx context.Context, nodeID string, p Pending) error {
	return s.putJSON(ctx, pendingIssueKey(nodeID), p)
}

// ClearPendingIssue removes the record.
func (s *Store) ClearPendingIssue(ctx context.Context, nodeID string) error {
	return s.kv.Delete(ctx, pendingIssueKey(nodeID))
}

// PendingRequirement returns the in-flight creation of an issue for a requirement.
func (s *Store) PendingRequirement(ctx context.Context, reqID int64) (*Pending, error) {
	var p Pending
	if err := s.getJSON(ctx, pendingReqKey(reqID), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// SetPendingRequirement records that an issue is about to be created for a requirement.
func (s *Store) SetPendingRequirement(ctx context.Context, reqID int64, p Pending) error {
	return s.putJSON(ctx, pendingReqKey(reqID), p)
}

// ClearPendingRequirement removes the record.
func (s *Store) ClearPendingRequirement(ctx context.Context, reqID int64) error {
	return s.kv.Delete(ctx, pendingReqKey(reqID))
}
