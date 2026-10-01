package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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

// blockingSource blocks its first LookupKey until release is closed, so a
// test can call Invalidate while the fetch is in flight.
type blockingSource struct {
	block   chan struct{}
	release chan struct{}

	mu      sync.Mutex
	calls   int
	results []apiaccess.Lookup
	errs    []error
}

func (s *blockingSource) LookupKey(_ context.Context, _ string) (apiaccess.Lookup, error) {
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.mu.Unlock()
	if i == 0 {
		close(s.block)
		<-s.release
	}
	return s.results[i], s.errs[i]
}

// TestResolverSkipsStoreWhenInvalidatedDuringFetch is the race from #34
// item 2: an Invalidate landing mid-fetch must not be overwritten by that
// fetch's own store.
func TestResolverSkipsStoreWhenInvalidatedDuringFetch(t *testing.T) {
	src := &blockingSource{
		block:   make(chan struct{}),
		release: make(chan struct{}),
		results: []apiaccess.Lookup{{Key: apiaccess.Key{ID: 1}}, {}},
		errs:    []error{nil, apiaccess.ErrNotFound},
	}
	r := NewResolver(src, time.Minute, func() time.Time { return now })

	var wg sync.WaitGroup
	var firstLK apiaccess.Lookup
	var firstErr error
	wg.Go(func() {
		firstLK, firstErr = r.Resolve(context.Background(), "h1")
	})

	<-src.block
	r.Invalidate("h1")
	close(src.release)
	wg.Wait()

	if firstErr != nil || firstLK.Key.ID != 1 {
		t.Fatalf("in-flight resolve: %+v %v", firstLK, firstErr)
	}

	// Nothing should have been cached by the invalidated fetch, so this
	// call must go to the source again and see the now-revoked key.
	if lk, err := r.Resolve(context.Background(), "h1"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("second resolve served the revoked entry: %+v %v", lk, err)
	}
	if src.calls != 2 {
		t.Errorf("calls %d want 2, the invalidated fetch should not have been cached", src.calls)
	}
}

// hangingSource never returns on its own; it only unblocks when ctx ends,
// standing in for a blackholed store.
type hangingSource struct {
	calls int32
}

func (s *hangingSource) LookupKey(ctx context.Context, _ string) (apiaccess.Lookup, error) {
	atomic.AddInt32(&s.calls, 1)
	<-ctx.Done()
	return apiaccess.Lookup{}, ctx.Err()
}

// TestResolverServesStaleWithinDeadlineOnHang is #34 item 3: a blackholed
// store must not hang a request past the lookup timeout.
func TestResolverServesStaleWithinDeadlineOnHang(t *testing.T) {
	seed := &fakeSource{res: map[string]apiaccess.Lookup{"h1": {Key: apiaccess.Key{ID: 7}}}}
	clock := now
	r := NewResolver(seed, time.Minute, func() time.Time { return clock })
	r.SetStaleGrace(10 * time.Minute)
	r.SetLookupTimeout(100 * time.Millisecond)

	if _, err := r.Resolve(context.Background(), "h1"); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}

	hang := &hangingSource{}
	r.src = hang
	clock = clock.Add(2 * time.Minute) // past ttl, inside the grace window

	start := time.Now()
	lk, err := r.Resolve(context.Background(), "h1")
	elapsed := time.Since(start)
	if err != nil || lk.Key.ID != 7 {
		t.Fatalf("stale not served on hang: %+v %v", lk, err)
	}
	if elapsed > time.Second {
		t.Fatalf("resolve took %s, want it bounded by the lookup deadline", elapsed)
	}
}

// TestResolverTimesOutWithoutCachedEntry is #34 item 3's other half: with
// nothing cached, a hang must still end in an error, not a hang.
func TestResolverTimesOutWithoutCachedEntry(t *testing.T) {
	hang := &hangingSource{}
	r := NewResolver(hang, time.Minute, func() time.Time { return now })
	r.SetLookupTimeout(100 * time.Millisecond)

	start := time.Now()
	_, err := r.Resolve(context.Background(), "h1")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v want ErrUnavailable", err)
	}
	if elapsed > time.Second {
		t.Fatalf("resolve took %s, want it bounded by the lookup deadline", elapsed)
	}
}

// ignoringSource ignores ctx entirely and only returns once release closes,
// standing in for lib/pq's refusal to honor ctx on an already-blocked read.
type ignoringSource struct {
	release chan struct{}
	lookup  apiaccess.Lookup
	err     error
}

func (s *ignoringSource) LookupKey(_ context.Context, _ string) (apiaccess.Lookup, error) {
	<-s.release
	return s.lookup, s.err
}

// TestResolverAbandonsLateFetchAfterTimeout is #34 item 3's critical fix: a
// fetch the driver won't cancel must still return, and never get cached late.
func TestResolverAbandonsLateFetchAfterTimeout(t *testing.T) {
	seed := &fakeSource{res: map[string]apiaccess.Lookup{"h1": {Key: apiaccess.Key{ID: 9}}}}
	clock := now
	r := NewResolver(seed, time.Minute, func() time.Time { return clock })
	r.SetStaleGrace(10 * time.Minute)
	r.SetLookupTimeout(100 * time.Millisecond)
	if _, err := r.Resolve(context.Background(), "h1"); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}

	ign := &ignoringSource{release: make(chan struct{}), lookup: apiaccess.Lookup{Key: apiaccess.Key{ID: 99}}}
	r.src = ign
	clock = clock.Add(2 * time.Minute) // past ttl, inside the grace window

	done := make(chan struct{})
	var lk apiaccess.Lookup
	var err error
	go func() {
		lk, err = r.Resolve(context.Background(), "h1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolve did not return within the lookup timeout")
	}
	if err != nil || lk.Key.ID != 9 {
		t.Fatalf("stale not served on an abandoned fetch: %+v %v", lk, err)
	}

	close(ign.release)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		cur := r.cache["h1"]
		r.mu.Unlock()
		if cur.lookup.Key.ID == 99 {
			t.Fatalf("the abandoned fetch's late result was stored: %+v", cur)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// errorAfterInvalidateSource blocks its first call, then answers with err.
type errorAfterInvalidateSource struct {
	block   chan struct{}
	release chan struct{}
	err     error
}

func (s *errorAfterInvalidateSource) LookupKey(_ context.Context, _ string) (apiaccess.Lookup, error) {
	close(s.block)
	<-s.release
	return apiaccess.Lookup{}, s.err
}

// TestResolverDropsStaleWhenInvalidatedDuringErroredFetch is #34 item 3's
// minor fix: a NOTIFY mid-fetch must win over serving a stale, maybe-revoked copy.
func TestResolverDropsStaleWhenInvalidatedDuringErroredFetch(t *testing.T) {
	seed := &fakeSource{res: map[string]apiaccess.Lookup{"h1": {Key: apiaccess.Key{ID: 3}}}}
	clock := now
	r := NewResolver(seed, time.Minute, func() time.Time { return clock })
	r.SetStaleGrace(10 * time.Minute)
	if _, err := r.Resolve(context.Background(), "h1"); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}

	src := &errorAfterInvalidateSource{block: make(chan struct{}), release: make(chan struct{}), err: errors.New("db down")}
	r.src = src
	clock = clock.Add(2 * time.Minute) // past ttl, inside the grace window

	var wg sync.WaitGroup
	var lk apiaccess.Lookup
	var err error
	wg.Go(func() {
		lk, err = r.Resolve(context.Background(), "h1")
	})
	<-src.block
	r.Invalidate("h1")
	close(src.release)
	wg.Wait()

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %+v %v, want ErrUnavailable: a NOTIFY landed mid-fetch", lk, err)
	}
}

// deadlineCapturingSource records the deadline ctx carried, if any.
type deadlineCapturingSource struct {
	deadline chan time.Time
}

func (s *deadlineCapturingSource) LookupKey(ctx context.Context, _ string) (apiaccess.Lookup, error) {
	dl, ok := ctx.Deadline()
	if ok {
		s.deadline <- dl
	}
	close(s.deadline)
	return apiaccess.Lookup{}, apiaccess.ErrNotFound
}

// TestResolverFetchContextCarriesConfiguredTimeout checks a zero config
// value cannot silently drop the deadline: the default must still apply.
func TestResolverFetchContextCarriesConfiguredTimeout(t *testing.T) {
	src := &deadlineCapturingSource{deadline: make(chan time.Time, 1)}
	r := NewResolver(src, time.Minute, func() time.Time { return now })
	r.SetLookupTimeout(0) // zero means "use the default"

	start := time.Now()
	_, _ = r.Resolve(context.Background(), "h1")

	dl, ok := <-src.deadline
	if !ok {
		t.Fatal("fetch ctx carried no deadline")
	}
	got := dl.Sub(start)
	if got < DefaultLookupTimeout-time.Second || got > DefaultLookupTimeout+time.Second {
		t.Fatalf("fetch ctx deadline %s from start, want within 1s of the default %s", got, DefaultLookupTimeout)
	}
}
