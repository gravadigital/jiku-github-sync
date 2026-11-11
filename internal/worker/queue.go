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

// Queue is a FIFO that holds each key at most once: enqueuing a key already waiting is a no-op
// (the sync reads current state anyway), except that BecamePublic is kept.
type Queue struct {
	mu      sync.Mutex
	order   []string
	items   map[string]Key
	signal  chan struct{}
	limit   int
	dropped int
}

// NewQueue returns a queue that holds at most limit distinct keys.
func NewQueue(limit int) *Queue {
	return &Queue{items: map[string]Key{}, signal: make(chan struct{}, 1), limit: limit}
}

// Push adds a key. It returns false when the queue is full (the reconciliation will catch up).
func (q *Queue) Push(k Key) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	id := k.id()
	if cur, ok := q.items[id]; ok {
		cur.BecamePublic = cur.BecamePublic || k.BecamePublic
		q.items[id] = cur
		return true
	}
	if q.limit > 0 && len(q.order) >= q.limit {
		q.dropped++
		return false
	}
	q.items[id] = k
	q.order = append(q.order, id)
	select {
	case q.signal <- struct{}{}:
	default:
	}
	return true
}

// Pop takes the oldest key; ok is false when the queue is empty.
func (q *Queue) Pop() (Key, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) == 0 {
		return Key{}, false
	}
	id := q.order[0]
	q.order = q.order[1:]
	k := q.items[id]
	delete(q.items, id)
	return k, true
}

// Len is the number of waiting keys.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order)
}

// Signal is notified when something is pushed.
func (q *Queue) Signal() <-chan struct{} { return q.signal }
