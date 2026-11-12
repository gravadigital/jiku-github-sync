package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadAssetSendsTheRepositoryAndReturnsTheURL(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r":
			fmt.Fprint(w, `{"id":77,"full_name":"o/r"}`)
		case "/user-attachments/assets":
			q := r.URL.Query()
			body, _ := io.ReadAll(r.Body)
			if q.Get("repository_id") != "77" || q.Get("name") != "a.png" || q.Get("content_type") != "image/png" ||
				r.Header.Get("Authorization") != "Bearer t0k" || string(body) != "PNG" {
				t.Errorf("upload request: %v %q", q, body)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"url":"https://github.com/user-attachments/assets/abc"}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
	u, err := c.UploadAsset(context.Background(), "o/r", "a.png", "image/png", []byte("PNG"))
	if err != nil || u != "https://github.com/user-attachments/assets/abc" {
		t.Fatalf("%q %v", u, err)
	}
}

func TestUploadRefusalIsAnAPIError(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r" {
			fmt.Fprint(w, `{"id":1}`)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"message":"Validation Failed","errors":[{"message":"content_type is not included"}]}`)
	})
	_, err := c.UploadAsset(context.Background(), "o/r", "a.pdf", "application/pdf", []byte("x"))
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 422 || !strings.Contains(ae.Message, "content_type") {
		t.Fatalf("%v", err)
	}
}

func TestDownloadSendsTheTokenOnlyToGitHub(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the token must not leave GitHub")
		}
		w.Header().Set("Content-Type", "image/png; charset=binary")
		fmt.Fprint(w, "PNG")
	}))
	defer other.Close()
	// Another host, as a signed URL is (Go drops the token on a redirect to another host).
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0k" {
			t.Error("GitHub itself gets the token")
		}
		http.Redirect(w, r, otherURL+"/signed", http.StatusFound)
	})
	// The test server's host is the client's base: it stands for github.com.
	b, ct, err := c.Download(context.Background(), c.base+"/user-attachments/assets/x")
	if err != nil || string(b) != "PNG" || ct != "image/png" {
		t.Fatalf("%q %q %v", b, ct, err)
	}
	if _, _, err := c.Download(context.Background(), otherURL+"/direct"); err != nil {
		t.Fatal(err)
	}
}
