package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

func collectPosts(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var contents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		mu.Lock()
		contents = append(contents, got["content"].(string))
		mu.Unlock()
		w.WriteHeader(204)
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), contents...)
	}
}

func TestPostSplitsLongMessageOnLineBoundaries(t *testing.T) {
	srv, posts := collectPosts(t)
	defer srv.Close()

	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, strings.Repeat("x", 49)+strconv.Itoa(i%10))
	}
	msg := strings.Join(lines, "\n")
	if len([]rune(msg)) < 5000 {
		t.Fatalf("test body too short: %d runes", len([]rune(msg)))
	}

	if err := New(srv.URL).Post(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	got := posts()
	if len(got) < 2 {
		t.Fatalf("expected the message split across several posts, got %d", len(got))
	}
	for _, c := range got {
		if n := len([]rune(c)); n > 2000 {
			t.Errorf("post exceeds 2000 runes: %d", n)
		}
	}
	for _, c := range got {
		for _, line := range strings.Split(c, "\n") {
			if !strings.Contains(msg, line) {
				t.Errorf("post contains a line not present in the original message: %q", line)
			}
		}
	}
	if joined := strings.Join(got, "\n"); joined != msg {
		t.Errorf("posts out of order or lines dropped:\ngot:  %q\nwant: %q", joined, msg)
	}
}

func TestPostHardSplitsOversizedLine(t *testing.T) {
	srv, posts := collectPosts(t)
	defer srv.Close()

	line := strings.Repeat("y", 2500)
	if err := New(srv.URL).Post(context.Background(), line); err != nil {
		t.Fatal(err)
	}

	got := posts()
	if len(got) != 2 {
		t.Fatalf("expected the oversized line split into 2 posts, got %d", len(got))
	}
	if n := len([]rune(got[0])); n != 2000 {
		t.Errorf("first post: got %d runes, want 2000", n)
	}
	if n := len([]rune(got[1])); n != 500 {
		t.Errorf("second post: got %d runes, want 500", n)
	}
	if got[0]+got[1] != line {
		t.Error("hard-split posts do not reassemble into the original line")
	}
}

func TestPostEmptyMessageNotPosted(t *testing.T) {
	srv, posts := collectPosts(t)
	defer srv.Close()

	if err := New(srv.URL).Post(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := posts(); len(got) != 0 {
		t.Errorf("expected no posts for an empty message, got %d", len(got))
	}
}

func TestPostStopsAtFirstChunkFailure(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, strings.Repeat("x", 49)+strconv.Itoa(i%10))
	}
	msg := strings.Join(lines, "\n")

	if err := New(srv.URL).Post(context.Background(), msg); err == nil {
		t.Fatal("expected an error from the failing first chunk")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("expected posting to stop after the first failing chunk, got %d calls", calls)
	}
}
