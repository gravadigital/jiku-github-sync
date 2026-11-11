package worker

import "testing"

func TestQueueDeduplicatesAndKeepsBecamePublic(t *testing.T) {
	q := NewQueue(10)
	q.Push(Key{Kind: Requirement, ReqID: 1})
	q.Push(Key{Kind: Issue, Repo: "a/b", Number: 2})
	q.Push(Key{Kind: Requirement, ReqID: 1, BecamePublic: true})
	if q.Len() != 2 {
		t.Fatalf("len %d", q.Len())
	}
	k, _ := q.Pop()
	if k.ReqID != 1 || !k.BecamePublic {
		t.Fatalf("first key keeps its place and gains BecamePublic: %+v", k)
	}
	k, _ = q.Pop()
	if k.Number != 2 {
		t.Fatalf("second key: %+v", k)
	}
	if _, ok := q.Pop(); ok {
		t.Fatal("empty")
	}
}

func TestQueueLimit(t *testing.T) {
	q := NewQueue(1)
	if !q.Push(Key{Kind: IssueNode, Node: "x"}) || q.Push(Key{Kind: IssueNode, Node: "y"}) {
		t.Fatal("a full queue refuses new keys")
	}
	if !q.Push(Key{Kind: IssueNode, Node: "x"}) {
		t.Fatal("a key already waiting is always accepted")
	}
}
