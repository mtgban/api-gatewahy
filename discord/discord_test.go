package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestPostSendsContent(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	if err := New(srv.URL).Post(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got["content"] != "hello" {
		t.Errorf("payload %v", got)
	}
	if am, ok := got["allowed_mentions"].(map[string]any); !ok || am["parse"] == nil {
		t.Errorf("mentions not disabled: %v", got)
	}
}

func TestPostNoHookIsNoop(t *testing.T) {
	if err := New("").Post(context.Background(), "hello"); err != nil {
		t.Error(err)
	}
}

func TestPostErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	if err := New(srv.URL).Post(context.Background(), "hello"); err == nil {
		t.Error("expected error")
	}
}

func TestPostErrorHidesHook(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	hook := srv.URL + "/api/webhooks/123456/s3cr3t-token"
	srv.Close()

	err := New(hook).Post(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected an error against a closed server")
	}
	u, perr := url.Parse(hook)
	if perr != nil {
		t.Fatal(perr)
	}
	msg := err.Error()
	if strings.Contains(msg, u.Host) {
		t.Errorf("error leaks the hook host: %s", msg)
	}
	if strings.Contains(msg, u.Path) || strings.Contains(msg, "s3cr3t-token") {
		t.Errorf("error leaks the hook path: %s", msg)
	}
}
