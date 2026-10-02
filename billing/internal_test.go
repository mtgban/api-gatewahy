package billing

// Tests of unexported helpers, too small to export for billing_test.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v84"
)

func TestIsPermanent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no plan metadata", fmt.Errorf("subscription sub_1: %w", ErrNoPlan), true},
		{"unknown package", invalid("unknown package %q", "gold"), true},
		{"no account", fmt.Errorf("subscription sub_1: %w", ErrNoAccount), true},
		{"validation", &ValidationError{Msg: "Store x is not available for the games you picked."}, true},
		{"stores down", fmt.Errorf("%w: magic: refused", ErrStoresUnavailable), false},
		{"stores down wrapping a validation", fmt.Errorf("%w: %w", ErrStoresUnavailable, invalid("odd")), false},
		{"stripe", &stripe.Error{Msg: "api error", HTTPStatusCode: 500}, false},
		{"db", errors.New("billing: upsert sub_1: db down"), false},
		{"cancelled", context.Canceled, false},
	}
	for _, c := range cases {
		if got := isPermanent(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestSubLocksAreKeyedAndFreed checks another id does not wait and a released id leaves no entry.
func TestSubLocksAreKeyedAndFreed(t *testing.T) {
	var l subLocks
	unlockA := l.lock("sub_a")
	done := make(chan struct{})
	go func() {
		l.lock("sub_b")()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sub_b waited on sub_a's lock")
	}
	unlockA()
	if len(l.locks) != 0 {
		t.Errorf("%d entries left after every unlock", len(l.locks))
	}
}

// TestSubLocksKeepAnAwaitedEntry checks the holder's unlock leaves the entry
// a waiter holds, so a newcomer queues behind the waiter.
func TestSubLocksKeepAnAwaitedEntry(t *testing.T) {
	var l subLocks
	var mu sync.Mutex
	inFlight, most := 0, 0
	enter := func() {
		mu.Lock()
		defer mu.Unlock()
		inFlight++
		most = max(most, inFlight)
	}
	leave := func() {
		mu.Lock()
		defer mu.Unlock()
		inFlight--
	}
	refs := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		if e := l.locks["sub_a"]; e != nil {
			return e.refs
		}
		return 0
	}

	unlockHolder := l.lock("sub_a")
	enter()
	waiterIn, releaseWaiter, waiterDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(waiterDone)
		unlock := l.lock("sub_a")
		enter()
		close(waiterIn)
		<-releaseWaiter
		leave()
		unlock()
	}()
	deadline := time.After(5 * time.Second)
	for refs() != 2 {
		select {
		case <-deadline:
			t.Fatal("the waiter never queued on sub_a")
		default:
			runtime.Gosched()
		}
	}
	leave()
	unlockHolder()
	select {
	case <-waiterIn:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never got sub_a")
	}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		unlock := l.lock("sub_a")
		enter()
		leave()
		unlock()
	}()
	select {
	case <-newDone:
		t.Error("the newcomer got sub_a while the waiter held it")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseWaiter)
	for _, done := range []chan struct{}{waiterDone, newDone} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a contender never finished")
		}
	}
	if most != 1 || len(l.locks) != 0 {
		t.Errorf("%d held sub_a at once, %d entries left", most, len(l.locks))
	}
}
