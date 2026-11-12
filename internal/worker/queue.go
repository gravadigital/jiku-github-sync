// Package worker runs the connector: a single queue of things to sync, fed by the GitHub poller,
// Jiku's event stream, the webhook receiver and the periodic reconciliation, and drained by one
// goroutine so the engine never runs two syncs at once (GitHub's write limits want writes serial
// anyway).
package worker

import (
	"fmt"
	"sync"
)

// Kind of a queued item.
type Kind int

const (
	// Requirement is a Jiku requirement id.
	Requirement Kind = iota
	// Issue is an issue by repo and number.
	Issue
	// IssueNode is an issue by node id.
	IssueNode
	// InitialSync runs (or plans) a project's initial sync, in the same serial queue as
	// everything else so it never races the regular sync of the same pair.
	InitialSync
	// Verify compares both sides of every pair of a project and logs the differences.
	Verify
)

// Key identifies something to sync.
type Key struct {
	Kind   Kind
	ReqID  int64
	Repo   string
	Number int
	Node   string
	// Project is the Jiku project of InitialSync and Verify.
	Project int64
	// BecamePublic is set when Jiku says the requirement just became public (R4).
	BecamePublic bool
}

func (k Key) id() string {
	switch k.Kind {
	case Requirement:
		return fmt.Sprintf("r:%d", k.ReqID)
	case Issue:
		return fmt.Sprintf("i:%s#%d", k.Repo, k.Number)
	case InitialSync:
		return fmt.Sprintf("s:%d", k.Project)
	case Verify:
		return fmt.Sprintf("v:%d", k.Project)
	default:
		return "n:" + k.Node
	}
}

// Queue holds each key at most once (enqueuing a key already waiting is a no-op: the sync reads
// current state anyway, but BecamePublic is kept) in two lanes: what just changed (Jiku events,
// webhooks, polls) always goes before background work (reconciliation, verification, initial
// sync), so a full sweep of many pairs never delays a fresh change.
type Queue struct {
	mu      sync.Mutex
	urgent  []string
	backlog []string
	items   map[string]Key
	lane    map[string]bool // id -> urgent
	signal  chan struct{}
	limit   int
	dropped int
}

// NewQueue returns a queue that holds at most limit distinct keys.
func NewQueue(limit int) *Queue {
	return &Queue{items: map[string]Key{}, lane: map[string]bool{}, signal: make(chan struct{}, 1), limit: limit}
}

// Push adds a key to the urgent lane. It returns false when the queue is full (the
// reconciliation will catch up).
func (q *Queue) Push(k Key) bool { return q.push(k, true) }

// PushBackground adds a key to the background lane.
func (q *Queue) PushBackground(k Key) bool { return q.push(k, false) }

func (q *Queue) push(k Key, urgent bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	id := k.id()
	if cur, ok := q.items[id]; ok {
		cur.BecamePublic = cur.BecamePublic || k.BecamePublic
		q.items[id] = cur
		if urgent && !q.lane[id] {
			// Waiting in the background and now it changed: promote it.
			q.backlog = remove(q.backlog, id)
			q.urgent = append(q.urgent, id)
			q.lane[id] = true
		}
		return true
	}
	if q.limit > 0 && len(q.items) >= q.limit {
		q.dropped++
		return false
	}
	q.items[id] = k
	q.lane[id] = urgent
	if urgent {
		q.urgent = append(q.urgent, id)
	} else {
		q.backlog = append(q.backlog, id)
	}
	select {
	case q.signal <- struct{}{}:
	default:
	}
	return true
}

// Pop takes the oldest urgent key, or else the oldest background one; ok is false when empty.
func (q *Queue) Pop() (Key, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var id string
	switch {
	case len(q.urgent) > 0:
		id, q.urgent = q.urgent[0], q.urgent[1:]
	case len(q.backlog) > 0:
		id, q.backlog = q.backlog[0], q.backlog[1:]
	default:
		return Key{}, false
	}
	k := q.items[id]
	delete(q.items, id)
	delete(q.lane, id)
	return k, true
}

// Len is the number of waiting keys.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Signal is notified when something is pushed.
func (q *Queue) Signal() <-chan struct{} { return q.signal }

func remove(xs []string, x string) []string {
	for i, v := range xs {
		if v == x {
			return append(xs[:i], xs[i+1:]...)
		}
	}
	return xs
}
