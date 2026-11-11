package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func get(t *testing.T, h *Handler, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}

func TestHealthzAlwaysOK(t *testing.T) {
	h := New()
	h.Add("bus", func(context.Context) error { return errors.New("down") })
	if code := get(t, h, "/healthz"); code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", code)
	}
}

func TestReadyzFollowsChecks(t *testing.T) {
	h := New()
	if code := get(t, h, "/readyz"); code != http.StatusOK {
		t.Fatalf("readyz without checks = %d, want 200", code)
	}
	h.Add("bus", func(context.Context) error { return errors.New("down") })
	if code := get(t, h, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with failing check = %d, want 503", code)
	}
	h.Add("bus", func(context.Context) error { return nil })
	if code := get(t, h, "/readyz"); code != http.StatusOK {
		t.Fatalf("readyz after recovery = %d, want 200", code)
	}
}
