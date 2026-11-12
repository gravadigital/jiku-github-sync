// Package webhook receives GitHub webhooks (from the GitHub App, in v1) and turns them into sync
// triggers. A webhook is only a hint that something changed: the engine reads the current state
// anyway, so a lost, duplicated or out-of-order delivery does no harm, and the poller and the
// reconciliation keep covering what GitHub does not redeliver.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// Trigger is what a delivery asks to sync.
type Trigger struct {
	Repo   string
	Number int
	Node   string // issue node id, for Project item events
	// ProjectNode is the Project v2 node id of a Project item event.
	ProjectNode string
}

// Handler verifies and parses deliveries.
type Handler struct {
	Secret  []byte
	Enqueue func(Trigger)
	Log     *slog.Logger
}

// MaxBody bounds a delivery (GitHub caps payloads at 25 MB).
const MaxBody = 25 << 20

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	if !Verify(h.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		h.Log.Warn("webhook with an invalid signature", "delivery", r.Header.Get("X-GitHub-Delivery"))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	t, ok, err := Parse(event, body)
	if err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if ok {
		h.Enqueue(t)
		h.Log.Debug("webhook queued", "event", event, "delivery", r.Header.Get("X-GitHub-Delivery"))
	}
	w.WriteHeader(http.StatusAccepted)
}

// Verify checks GitHub's X-Hub-Signature-256 ("sha256=<hex hmac>") in constant time.
func Verify(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Sign computes the header value for a body (tests, and to simulate deliveries).
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Parse extracts the trigger of the events the connector cares about. ok is false for any other
// event (ping, push, ...), which is acknowledged and ignored.
func Parse(event string, body []byte) (Trigger, bool, error) {
	switch event {
	case "issues", "issue_comment":
		var p struct {
			Issue struct {
				Number      int       `json:"number"`
				PullRequest *struct{} `json:"pull_request"`
			} `json:"issue"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return Trigger{}, false, err
		}
		if p.Issue.PullRequest != nil || p.Issue.Number == 0 {
			return Trigger{}, false, nil
		}
		return Trigger{Repo: p.Repository.FullName, Number: p.Issue.Number}, true, nil
	case "projects_v2_item":
		var p struct {
			ProjectsV2Item struct {
				ContentNodeID string `json:"content_node_id"`
				ContentType   string `json:"content_type"`
				ProjectNodeID string `json:"project_node_id"`
			} `json:"projects_v2_item"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return Trigger{}, false, err
		}
		if p.ProjectsV2Item.ContentType != "Issue" || p.ProjectsV2Item.ContentNodeID == "" {
			return Trigger{}, false, nil
		}
		return Trigger{Node: p.ProjectsV2Item.ContentNodeID, ProjectNode: p.ProjectsV2Item.ProjectNodeID}, true, nil
	}
	return Trigger{}, false, nil
}
