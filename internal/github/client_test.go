package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTest(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(Options{BaseURL: srv.URL, Token: "t0k"})
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestPaginationAndPullRequestsFiltered(t *testing.T) {
	var base string
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0k" {
			t.Errorf("missing token")
		}
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"node_id":"I3","number":3,"title":"c","state":"open"}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=2>; rel="next", <%s/x>; rel="last"`, base, base))
		fmt.Fprint(w, `[{"node_id":"I1","number":1,"title":"a","state":"open","type":{"name":"Bug"},"labels":[{"name":"x"}]},
		               {"node_id":"P2","number":2,"title":"pr","state":"open","pull_request":{}}]`)
	})
	base = c.base
	is, err := c.ListIssues(context.Background(), "o/r", time.Time{}, "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(is) != 2 || is[0].IssueType != "Bug" || is[0].Labels[0] != "x" || is[1].Number != 3 || is[0].Repo != "o/r" {
		t.Fatalf("unexpected: %+v", is)
	}
}

func TestNotFoundAndGone(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/2") {
			w.WriteHeader(http.StatusGone)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
		fmt.Fprint(w, `{"message":"nope"}`)
	})
	for _, n := range []int{1, 2} {
		_, err := c.GetIssue(context.Background(), "o/r", n)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("issue %d: %v", n, err)
		}
	}
}

func TestSecondaryRateLimitIsRetried(t *testing.T) {
	var calls atomic.Int32
	var slept time.Duration
	c := newTest(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit"}`)
			return
		}
		fmt.Fprint(w, `{"id":5,"body":"b","html_url":"u","issue_url":"https://api.github.com/repos/o/r/issues/9"}`)
	})
	c.sleep = func(_ context.Context, d time.Duration) error { slept += d; return nil }
	cm, err := c.CreateComment(context.Background(), "o/r", 9, "b")
	if err != nil || cm.ID != 5 || cm.IssueNum != 9 {
		t.Fatalf("comment %+v err %v", cm, err)
	}
	if calls.Load() != 2 || slept < 7*time.Second {
		t.Fatalf("expected a retry after Retry-After: calls=%d slept=%s", calls.Load(), slept)
	}
}

func TestRateLimitBeyondMaxWaitFails(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", fmt.Sprint(time.Now().Add(2*time.Hour).Unix()))
		w.WriteHeader(http.StatusForbidden)
	})
	if _, err := c.Viewer(context.Background()); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected a rate limit error, got %v", err)
	}
}

func TestWritesAreSpaced(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":1,"issue_url":"x/1"}`)
	})
	c.writeInterval = time.Hour
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	for i := 0; i < 3; i++ {
		if _, err := c.CreateComment(context.Background(), "o/r", 1, "b"); err != nil {
			t.Fatal(err)
		}
	}
	if len(waits) != 2 {
		t.Fatalf("the 2nd and 3rd creations must wait, got %v", waits)
	}
}

func TestIssueTypePayload(t *testing.T) {
	var got map[string]any
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		fmt.Fprint(w, `{"number":1}`)
	})
	empty := ""
	if _, err := c.UpdateIssue(context.Background(), "o/r", 1, IssueInput{Type: &empty}); err != nil {
		t.Fatal(err)
	}
	if v, ok := got["type"]; !ok || v != nil {
		t.Fatalf(`removing the type sends "type": null, got %v`, got)
	}
}

func TestGraphQLErrorsAndNotFound(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "addProjectV2ItemById") {
			fmt.Fprint(w, `{"errors":[{"type":"FORBIDDEN","message":"Resource not accessible"}]}`)
			return
		}
		fmt.Fprint(w, `{"data":{"node":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve"}]}`)
	})
	if _, err := c.ProjectSchema(context.Background(), "PVT_x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("schema: %v", err)
	}
	if _, err := c.AddItem(context.Background(), "PVT_x", "I_x"); err == nil || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Fatalf("add item: %v", err)
	}
}

func TestItemsAndStatusChange(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(b), "timelineItems"):
			fmt.Fprint(w, `{"data":{"node":{"timelineItems":{"nodes":[
			  {"createdAt":"2026-11-01T10:00:00Z","status":"Inbox","actor":{"login":"github-project-automation"},"project":{"id":"P"}},
			  {"createdAt":"2026-11-01T11:00:00Z","status":"Review","actor":{"login":"ana"},"project":{"id":"P"}},
			  {"createdAt":"2026-11-01T12:00:00Z","status":"Done","actor":{"login":"bob"},"project":{"id":"OTHER"}}]}}}}`)
		default:
			fmt.Fprint(w, `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false},"nodes":[
			  {"id":"IT1","content":{"id":"I1"},"fieldValues":{"nodes":[{},{"name":"Review","updatedAt":"2026-11-01T11:00:00Z","creator":{"login":"github-project-automation"},"field":{"name":"Status"}}]}},
			  {"id":"IT2","content":{},"fieldValues":{"nodes":[]}}]}}}}`)
		}
	})
	items, err := c.ListItems(context.Background(), "P")
	if err != nil || len(items) != 1 || items[0].Values["Status"].Option != "Review" {
		t.Fatalf("items %+v err %v (drafts are left out)", items, err)
	}
	sc, err := c.LastStatusChange(context.Background(), "I1", "P")
	if err != nil || sc == nil || sc.Actor != "ana" || sc.Status != "Review" {
		t.Fatalf("status change %+v err %v (other projects are ignored)", sc, err)
	}
}
