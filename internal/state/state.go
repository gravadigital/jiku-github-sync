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
	Title string `json:"title"`
	// BodyHash is the description's hash from before attachments were translated, when both
	// sides held the same text. JikuBodyHash and GHBodyHash replace it: with attachments each
	// side holds its own form of the text (`![file:N]` in Jiku, an asset URL on GitHub).
	BodyHash     string `json:"bodyHash,omitempty"`
	JikuBodyHash string `json:"jBody,omitempty"`
	GHBodyHash   string `json:"gBody,omitempty"`

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

// JikuBody is the hash of Jiku's description at the last sync.
func (s Snapshot) JikuBody() string {
	if s.JikuBodyHash != "" {
		return s.JikuBodyHash
	}
	return s.BodyHash
}

// GHBody is the hash of the issue's body at the last sync.
func (s Snapshot) GHBody() string {
	if s.GHBodyHash != "" {
		return s.GHBodyHash
	}
	return s.BodyHash
}

// Bodies sets both description hashes.
func (s *Snapshot) Bodies(jiku, gh string) {
	s.BodyHash, s.JikuBodyHash, s.GHBodyHash = "", jiku, gh
}

// Asset is a file that exists on both sides: a Jiku file and the GitHub attachment it became (or
// came from). It lets a text that goes back and forth reuse the file instead of copying it again.
type Asset struct {
	JikuFile int64  `json:"f"`
	URL      string `json:"u"`
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

// Link is a requirement and its issue, or a task and its sub-issue.
//
// A task's pair (TaskID set) is stored under its own keys. Its ReqID is the requirement the task
// was synced under, and ParentNode that requirement's issue: the sub-issue's parent at the last
// sync. Links written before tasks existed have no TaskID and are requirements.
type Link struct {
	ReqID       int64  `json:"reqId"`
	TaskID      int64  `json:"taskId,omitempty"`
	ParentNode  string `json:"parentNode,omitempty"`
	JikuProject int64  `json:"jikuProject"`
	IssueNodeID string `json:"issueNodeId"`
	Repo        string `json:"repo"`
	Number      int    `json:"number"`
	// ItemID is the issue's item in the mapped Project ("" when there is none yet).
	ItemID string `json:"itemId,omitempty"`
	// Unlinked marks a pair that stopped syncing because the requirement became internal. It is
	// kept as a tombstone so the issue is not imported again, and is revived if the requirement
	// becomes public again.
	Unlinked bool `json:"unlinked,omitempty"`
	// UnlinkedBy is the side that unlinked a task's pair: "jiku" (the task became internal or left
	// the synced requirements) or "github" (the sub-issue was detached). Only that side revives it.
	UnlinkedBy string `json:"unlinkedBy,omitempty"`
	// SubIssueNoted marks a requirement whose issue was found to be a sub-issue of another synced
	// issue, once its internal note was left (it stays a requirement).
	SubIssueNoted bool          `json:"subNoted,omitempty"`
	Last          Snapshot      `json:"last"`
	Comments      []CommentLink `json:"comments,omitempty"`
	Assets        []Asset       `json:"assets,omitempty"`
	SyncedAt      time.Time     `json:"syncedAt"`
}

// AssetURL returns the GitHub URL a Jiku file became, if it did.
func (l *Link) AssetURL(file int64) (string, bool) {
	for _, a := range l.Assets {
		if a.JikuFile == file {
			return a.URL, true
		}
	}
	return "", false
}

// AssetFile returns the Jiku file a GitHub attachment URL corresponds to, if any.
func (l *Link) AssetFile(url string) (int64, bool) {
	for _, a := range l.Assets {
		if a.URL == url {
			return a.JikuFile, true
		}
	}
	return 0, false
}

// AddAsset records a file present on both sides.
func (l *Link) AddAsset(file int64, url string) {
	for _, a := range l.Assets {
		if a.JikuFile == file && a.URL == url {
			return
		}
	}
	l.Assets = append(l.Assets, Asset{JikuFile: file, URL: url})
}

// IsTask reports whether the pair is a task's.
func (l *Link) IsTask() bool { return l.TaskID != 0 }

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
	// TaskLinks are the task ids with a Link (only with tasks turned on).
	TaskLinks []int64 `json:"taskLinks,omitempty"`
	// TasksStartedAt is when the connector started syncing the project's tasks: tasks and
	// sub-issues created before it are only synced by the tasks' initial sync.
	TasksStartedAt time.Time `json:"tasksStartedAt,omitempty"`
	// TaskInitialSyncAt is when the tasks' initial sync was applied (zero if never).
	TaskInitialSyncAt time.Time `json:"taskInitialSyncAt,omitempty"`
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

// A task's pair has its own keys: a sub-issue's node never collides with a requirement's.
func taskLinkKey(taskID int64) string    { return fmt.Sprintf("link.t.%d", taskID) }
func taskIssueKey(nodeID string) string  { return "link.ti." + NodeKey(nodeID) }
func pendingTaskKey(taskID int64) string { return fmt.Sprintf("pending.t.%d", taskID) }
func pendingSubIssueKey(n string) string { return "pending.ti." + NodeKey(n) }

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

// TaskLink returns the link of a task.
func (s *Store) TaskLink(ctx context.Context, taskID int64) (*Link, error) {
	var l Link
	if err := s.getJSON(ctx, taskLinkKey(taskID), &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// TaskLinkByIssue returns the link of a sub-issue (by node id).
func (s *Store) TaskLinkByIssue(ctx context.Context, nodeID string) (*Link, error) {
	b, err := s.kv.Get(ctx, taskIssueKey(nodeID))
	if err != nil {
		return nil, err
	}
	var taskID int64
	if err := json.Unmarshal(b, &taskID); err != nil {
		return nil, err
	}
	return s.TaskLink(ctx, taskID)
}

// SaveTaskLink stores a task's link, its issue index and the project's task index.
func (s *Store) SaveTaskLink(ctx context.Context, l *Link) error {
	if l.TaskID == 0 {
		return errors.New("state: a task link needs its task id")
	}
	l.SyncedAt = time.Now().UTC()
	if err := s.putJSON(ctx, taskLinkKey(l.TaskID), l); err != nil {
		return err
	}
	if err := s.putJSON(ctx, taskIssueKey(l.IssueNodeID), l.TaskID); err != nil {
		return err
	}
	return s.UpdateProject(ctx, l.JikuProject, func(p *ProjectState) {
		for _, id := range p.TaskLinks {
			if id == l.TaskID {
				return
			}
		}
		p.TaskLinks = append(p.TaskLinks, l.TaskID)
	})
}

// Save stores a link of either kind.
func (s *Store) Save(ctx context.Context, l *Link) error {
	if l.IsTask() {
		return s.SaveTaskLink(ctx, l)
	}
	return s.SaveLink(ctx, l)
}

// PendingTask returns the in-flight creation of a sub-issue for a task.
func (s *Store) PendingTask(ctx context.Context, taskID int64) (*Pending, error) {
	var p Pending
	if err := s.getJSON(ctx, pendingTaskKey(taskID), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// SetPendingTask records that a sub-issue is about to be created for a task.
func (s *Store) SetPendingTask(ctx context.Context, taskID int64, p Pending) error {
	return s.putJSON(ctx, pendingTaskKey(taskID), p)
}

// ClearPendingTask removes the record.
func (s *Store) ClearPendingTask(ctx context.Context, taskID int64) error {
	return s.kv.Delete(ctx, pendingTaskKey(taskID))
}

// PendingSubIssue returns the in-flight creation of a task for a sub-issue.
func (s *Store) PendingSubIssue(ctx context.Context, nodeID string) (*Pending, error) {
	var p Pending
	if err := s.getJSON(ctx, pendingSubIssueKey(nodeID), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// SetPendingSubIssue records that a task is about to be created for a sub-issue.
func (s *Store) SetPendingSubIssue(ctx context.Context, nodeID string, p Pending) error {
	return s.putJSON(ctx, pendingSubIssueKey(nodeID), p)
}

// ClearPendingSubIssue removes the record.
func (s *Store) ClearPendingSubIssue(ctx context.Context, nodeID string) error {
	return s.kv.Delete(ctx, pendingSubIssueKey(nodeID))
}
