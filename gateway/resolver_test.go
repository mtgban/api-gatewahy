package gateway

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

type fakeSource struct {
	calls int
	res   map[string]apiaccess.Lookup
	err   error
}

func (f *fakeSource) LookupKey(_ context.Context, hash string) (apiaccess.Lookup, error) {
	f.calls++
	if f.err != nil {
		return apiaccess.Lookup{}, f.err
	}
	lk, ok := f.res[hash]
	if !ok {
		return apiaccess.Lookup{}, apiaccess.ErrNotFound
	}
	return lk, nil
}

func TestResolverCachesHits(t *testing.T) {
	src := &fakeSource{res: map[string]apiaccess.Lookup{"h1": {Key: apiaccess.Key{ID: 1}}}}
	clock := now
	r := NewResolver(src, time.Minute, func() time.Time { return clock })

	for i := 0; i < 3; i++ {
		lk, err := r.Resolve(context.Background(), "h1")
		if err != nil || lk.Key.ID != 1 {
			t.Fatalf("resolve %d: %+v %v", i, lk, err)
		}
	}
	if src.calls != 1 {
		t.Errorf("source called %d times", src.calls)
	}

	clock = clock.Add(2 * time.Minute)
	_, _ = r.Resolve(context.Background(), "h1")
	if src.calls != 2 {
		t.Errorf("ttl not honored: %d calls", src.calls)
	}
}

func TestResolverCachesUnknown(t *testing.T) {
	src := &fakeSource{res: map[string]apiaccess.Lookup{}}
	r := NewResolver(src, time.Minute, func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if _, err := r.Resolve(context.Background(), "nope"); !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("got %v", err)
		}
	}
	if src.calls != 1 {
		t.Errorf("unknown key not cached: %d calls", src.calls)
	}
}

func TestResolverServesStaleOnError(t *testing.T) {
	src := &fakeSource{res: map[string]apiaccess.Lookup{"h1": {Key: apiaccess.Key{ID: 1}}}}
	clock := now
	r := NewResolver(src, time.Minute, func() time.Time { return clock })
	r.SetStaleGrace(10 * time.Minute)
	_, _ = r.Resolve(context.Background(), "h1")

	src.err = errors.New("db down")
	clock = clock.Add(11*time.Minute - time.Second)
	lk, err := r.Resolve(context.Background(), "h1")
	if err != nil || lk.Key.ID != 1 {
		t.Errorf("stale not served inside the grace window: %+v %v", lk, err)
	}
	if _, err := r.Resolve(context.Background(), "never-seen"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("got %v want ErrUnavailable", err)
	}
}

func TestResolverDropsStaleAfterGrace(t *testing.T) {
	src := &fakeSource{res: map[string]apiaccess.Lookup{"h1": {Key: apiaccess.Key{ID: 1}}}}
	clock := now
	r := NewResolver(src, time.Minute, func() time.Time { return clock })
	r.SetStaleGrace(10 * time.Minute)
	_, _ = r.Resolve(context.Background(), "h1")

	src.err = errors.New("db down")
	clock = clock.Add(11*time.Minute + time.Second)
	if _, err := r.Resolve(context.Background(), "h1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("got %v want ErrUnavailable past the grace window", err)
	}
}

func TestResolverBoundsCache(t *testing.T) {
	src := &fakeSource{res: map[string]apiaccess.Lookup{}}
	r := NewResolver(src, time.Minute, func() time.Time { return now })
	r.SetMaxEntries(8)
	for i := 0; i < 50; i++ {
		_, _ = r.Resolve(context.Background(), fmt.Sprintf("unknown-%d", i))
		r.mu.Lock()
		n := len(r.cache)
		r.mu.Unlock()
		if n > 8 {
			t.Fatalf("cache holds %d entries after %d lookups, want at most 8", n, i+1)
		}
	}
}

func TestResolverSweepKeepsGraceWindowEntries(t *testing.T) {
	src := &fakeSource{res: map[string]apiaccess.Lookup{}}
	clock := now
	r := NewResolver(src, time.Minute, func() time.Time { return clock })
	r.SetStaleGrace(10 * time.Minute)
	r.SetMaxEntries(8)

	// Four entries are past the ttl but still inside the grace window; the
	// other four are past ttl+grace and should be swept regardless.
	r.mu.Lock()
	for i := 0; i < 4; i++ {
		r.cache[fmt.Sprintf("aged-%d", i)] = cacheEntry{unknown: true, fetched: clock.Add(-5 * time.Minute)}
	}
	for i := 0; i < 4; i++ {
		r.cache[fmt.Sprintf("stale-%d", i)] = cacheEntry{unknown: true, fetched: clock.Add(-20 * time.Minute)}
	}
	r.mu.Unlock()

	// This insert fills the cache to its bound, forcing makeRoomLocked to run.
	if _, err := r.Resolve(context.Background(), "new-key"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("resolve new-key: %v", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i < 4; i++ {
		if _, ok := r.cache[fmt.Sprintf("aged-%d", i)]; !ok {
			t.Errorf("aged-%d evicted by the sweep, want it kept inside the grace window", i)
		}
	}
	for i := 0; i < 4; i++ {
		if _, ok := r.cache[fmt.Sprintf("stale-%d", i)]; ok {
			t.Errorf("stale-%d survived the sweep, want it evicted past the grace window", i)
		}
	}
}

func TestResolverInvalidate(t *testing.T) {
	src := &fakeSource{res: map[string]apiaccess.Lookup{"h1": {}, "h2": {}}}
	r := NewResolver(src, time.Minute, func() time.Time { return now })
	_, _ = r.Resolve(context.Background(), "h1")
	_, _ = r.Resolve(context.Background(), "h2")
	r.Invalidate("h1")
	_, _ = r.Resolve(context.Background(), "h1")
	_, _ = r.Resolve(context.Background(), "h2")
	if src.calls != 3 {
		t.Errorf("calls %d want 3", src.calls)
	}
	r.Invalidate("")
	_, _ = r.Resolve(context.Background(), "h2")
	if src.calls != 4 {
		t.Errorf("calls %d want 4", src.calls)
	}
}
