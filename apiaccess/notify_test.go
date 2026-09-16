package apiaccess

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotifyListenRoundTrip(t *testing.T) {
	c := testClient(t)
	dsn := os.Getenv("APIACCESS_TEST_DSN")
	got := make(chan string, 1)
	l, err := Listen(dsn, func(p string) { got <- p }, func() {}, func(err error) { t.Log(err) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	// LISTEN takes a moment to register; retry the NOTIFY a few times.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := c.Notify(context.Background(), "abc123"); err != nil {
			t.Fatal(err)
		}
		select {
		case p := <-got:
			if p != "abc123" {
				t.Errorf("payload %q", p)
			}
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Fatal("notification never arrived")
}

func TestNotifyListenCloseIdempotent(t *testing.T) {
	dsn := os.Getenv("APIACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("APIACCESS_TEST_DSN not set")
	}
	var reconnects atomic.Int32
	l, err := Listen(dsn, func(string) {}, func() { reconnects.Add(1) }, func(err error) { t.Log(err) })
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := reconnects.Load(); n != 0 {
		t.Errorf("onReconnect called %d times after close", n)
	}
}
