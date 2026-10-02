package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	for i := 0; i < 50; i++ {
		src.res[fmt.Sprintf("key-%d", i)] = apiaccess.Lookup{}
	}
	r := NewResolver(src, time.Minute, func() time.Time { return now })
	r.SetMaxEntries(8)
	for i := 0; i < 50; i++ {
		_, _ = r.Resolve(context.Background(), fmt.Sprintf("key-%d", i))
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
	r.SetMaxUnknown(8)

	// Four entries are past the ttl but still inside the grace window; the
	// other four are past ttl+grace and should be swept regardless.
	r.mu.Lock()
	for i := 0; i < 4; i++ {
		r.unknown[fmt.Sprintf("aged-%d", i)] = cacheEntry{unknown: true, fetched: clock.Add(-5 * time.Minute)}
	}
	for i := 0; i < 4; i++ {
		r.unknown[fmt.Sprintf("stale-%d", i)] = cacheEntry{unknown: true, fetched: clock.Add(-20 * time.Minute)}
	}
	r.mu.Unlock()

	// This insert fills the cache to its bound, forcing makeRoomLocked to run.
	if _, err := r.Resolve(context.Background(), "new-key"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("resolve new-key: %v", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i < 4; i++ {
		if _, ok := r.unknown[fmt.Sprintf("aged-%d", i)]; !ok {
			t.Errorf("aged-%d evicted by the sweep, want it kept inside the grace window", i)
		}
	}
	for i := 0; i < 4; i++ {
		if _, ok := r.unknown[fmt.Sprintf("stale-%d", i)]; ok {
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

// TestResolverUnknownKeysCannotEvictRealKeys is #56 item 1: a flood of
// forged keys must stay inside its own budget and leave real keys cached.
func TestResolverUnknownKeysCannotEvictRealKeys(t *testing.T) {
	src := &fakeSource{res: map[string]apiaccess.Lookup{"real": {Key: apiaccess.Key{ID: 1}}}}
	r := NewResolver(src, time.Minute, func() time.Time { return now })
	r.SetMaxEntries(8)
	r.SetMaxUnknown(4)
	if _, err := r.Resolve(context.Background(), "real"); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}

	for i := 0; i < 200; i++ {
		_, _ = r.Resolve(context.Background(), fmt.Sprintf("forged-%d", i))
		r.mu.Lock()
		n := len(r.unknown)
		r.mu.Unlock()
		if n > 4 {
			t.Fatalf("unknown map holds %d entries after %d forged keys, want at most 4", n, i+1)
		}
	}

	calls := src.calls
	lk, err := r.Resolve(context.Background(), "real")
	if err != nil || lk.Key.ID != 1 {
		t.Fatalf("real key after the flood: %+v %v", lk, err)
	}
	if src.calls != calls {
		t.Fatal("the forged flood evicted the real key: resolving it called the source again")
	}
}

// TestResolverEvictsExpiredUnknownFirst checks a full unknown map drops
// entries past ttl+grace before any fresh one.
func TestResolverEvictsExpiredUnknownFirst(t *testing.T) {
	src := &fakeSource{res: map[string]apiaccess.Lookup{}}
	clock := now
	r := NewResolver(src, time.Minute, func() time.Time { return clock })
	r.SetStaleGrace(10 * time.Minute)
	r.SetMaxUnknown(4)

	for _, h := range []string{"old-0", "old-1"} {
		_, _ = r.Resolve(context.Background(), h)
	}
	clock = clock.Add(20 * time.Minute) // past ttl+grace for the old entries
	for _, h := range []string{"new-0", "new-1", "new-2"} {
		_, _ = r.Resolve(context.Background(), h)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range []string{"new-0", "new-1", "new-2"} {
		if _, ok := r.unknown[h]; !ok {
			t.Errorf("%s missing from the unknown map, want fresh entries kept over expired ones", h)
		}
	}
	for _, h := range []string{"old-0", "old-1"} {
		if _, ok := r.unknown[h]; ok {
			t.Errorf("%s survived, want expired entries evicted first", h)
		}
	}
}

// gatedSource holds every call until release closes; entered closes when
// the first call starts. A call ends early if its ctx does.
type gatedSource struct {
	entered chan struct{}
	release chan struct{}
	lookup  apiaccess.Lookup
	once    sync.Once
	calls   atomic.Int32
}

func newGatedSource(id int64) *gatedSource {
	return &gatedSource{entered: make(chan struct{}), release: make(chan struct{}),
		lookup: apiaccess.Lookup{Key: apiaccess.Key{ID: id}}}
}

func (s *gatedSource) LookupKey(ctx context.Context, _ string) (apiaccess.Lookup, error) {
	s.calls.Add(1)
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return s.lookup, nil
	case <-ctx.Done():
		return apiaccess.Lookup{}, ctx.Err()
	}
}

// settle gives callers that have read the clock time to reach the shared fetch.
const settle = 100 * time.Millisecond

// TestResolverCoalescesConcurrentMisses is #56 item 2: concurrent misses
// for one hash make one store query and all get its answer.
func TestResolverCoalescesConcurrentMisses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newGatedSource(5)
		r := NewResolver(src, time.Minute, func() time.Time { return now })

		const n = 8
		lks := make([]apiaccess.Lookup, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Go(func() {
				lks[i], errs[i] = r.Resolve(context.Background(), "h1")
			})
		}
		synctest.Wait() // every caller is parked on a fetch
		close(src.release)
		wg.Wait()

		if c := src.calls.Load(); c != 1 {
			t.Errorf("LookupKey called %d times for %d concurrent misses, want 1", c, n)
		}
		for i := 0; i < n; i++ {
			if errs[i] != nil || lks[i].Key.ID != 5 {
				t.Errorf("caller %d: %+v %v", i, lks[i], errs[i])
			}
		}
	})
}

// TestResolverCoalescedFetchSurvivesLeaderCancel checks the caller that
// started the shared fetch giving up does not cancel it for the others.
func TestResolverCoalescedFetchSurvivesLeaderCancel(t *testing.T) {
	src := newGatedSource(6)
	var arrived atomic.Int32
	r := NewResolver(src, time.Minute, func() time.Time {
		arrived.Add(1)
		return now
	})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leaderErr := make(chan error, 1)
	go func() {
		_, err := r.Resolve(leaderCtx, "h1")
		leaderErr <- err
	}()
	waitFor(t, func() bool { return src.calls.Load() >= 1 })

	var lk apiaccess.Lookup
	var err error
	followerDone := make(chan struct{})
	go func() {
		lk, err = r.Resolve(context.Background(), "h1")
		close(followerDone)
	}()
	waitFor(t, func() bool { return arrived.Load() >= 2 })
	time.Sleep(settle)

	cancelLeader()
	if lerr := <-leaderErr; !errors.Is(lerr, ErrUnavailable) {
		t.Fatalf("leader got %v, want ErrUnavailable after its ctx ended", lerr)
	}
	close(src.release)
	<-followerDone

	if err != nil || lk.Key.ID != 6 {
		t.Fatalf("follower lost the shared fetch when the leader gave up: %+v %v", lk, err)
	}
	if c := src.calls.Load(); c != 1 {
		t.Errorf("LookupKey called %d times, want 1 shared call", c)
	}
}

// TestResolverDoesNotCoalesceAcrossInvalidate keeps #34's guarantee under
// coalescing: a resolve after Invalidate must not join a fetch from before it.
func TestResolverDoesNotCoalesceAcrossInvalidate(t *testing.T) {
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

	var lk apiaccess.Lookup
	var err error
	done := make(chan struct{})
	go func() {
		lk, err = r.Resolve(context.Background(), "h1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(settle): // stuck on the old fetch: release it below
	}
	close(src.release)
	wg.Wait()
	<-done

	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("a resolve after Invalidate joined the pre-revocation fetch: %+v %v", lk, err)
	}
	if firstErr != nil || firstLK.Key.ID != 1 {
		t.Fatalf("in-flight resolve: %+v %v", firstLK, firstErr)
	}
	if lk, err := r.Resolve(context.Background(), "h1"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("revoked key served from cache: %+v %v", lk, err)
	}
	if src.calls != 2 {
		t.Errorf("calls %d want 2", src.calls)
	}
}

// hungFirstSource ignores ctx on its first call and holds it until release
// closes; every later call answers at once.
type hungFirstSource struct {
	release chan struct{}
	lookup  apiaccess.Lookup
	calls   atomic.Int32
}

func (s *hungFirstSource) LookupKey(_ context.Context, _ string) (apiaccess.Lookup, error) {
	if s.calls.Add(1) == 1 {
		<-s.release
	}
	return s.lookup, nil
}

// TestResolverForgetsHungFetch checks a fetch the driver never cancels does
// not pin its key: once it times out, the next miss starts a fresh lookup.
func TestResolverForgetsHungFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &hungFirstSource{release: make(chan struct{}), lookup: apiaccess.Lookup{Key: apiaccess.Key{ID: 2}}}
		defer close(src.release)
		r := NewResolver(src, time.Minute, func() time.Time { return now })
		r.SetLookupTimeout(100 * time.Millisecond)

		if _, err := r.Resolve(context.Background(), "h1"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("hung first resolve: %v, want ErrUnavailable", err)
		}
		synctest.Wait() // let the timed-out lookup finish its cleanup

		lk, err := r.Resolve(context.Background(), "h1")
		if c := src.calls.Load(); err != nil || lk.Key.ID != 2 || c != 2 {
			t.Fatalf("a hung fetch pinned the key after the store recovered: second=%d %v calls=%d, want second=2 <nil> calls=2", lk.Key.ID, err, c)
		}
	})
}

// TestResolverKeepsOneMapPerHash pins the two-map invariants: Invalidate
// clears an unknown entry, and each store removes the hash from the other map.
func TestResolverKeepsOneMapPerHash(t *testing.T) {
	src := &fakeSource{res: map[string]apiaccess.Lookup{}}
	clock := now
	r := NewResolver(src, time.Minute, func() time.Time { return clock })
	r.SetStaleGrace(10 * time.Minute)

	// A key created after a negative answer works once its NOTIFY lands.
	if _, err := r.Resolve(context.Background(), "h1"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("h1 before creation: %v, want ErrUnknownKey", err)
	}
	src.res["h1"] = apiaccess.Lookup{Key: apiaccess.Key{ID: 4}}
	r.Invalidate("h1")
	if lk, err := r.Resolve(context.Background(), "h1"); err != nil || lk.Key.ID != 4 {
		t.Fatalf("h1 after creation and Invalidate: %+v %v, want the key", lk, err)
	}

	// A key revoked without NOTIFY turns unknown past the ttl, and stays so
	// through a store error rather than serving the revoked key stale.
	delete(src.res, "h1")
	clock = clock.Add(2 * time.Minute)
	if lk, err := r.Resolve(context.Background(), "h1"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("h1 after revocation: %+v %v, want ErrUnknownKey", lk, err)
	}
	src.err = errors.New("db down")
	if lk, err := r.Resolve(context.Background(), "h1"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("h1 on a store error after revocation: %+v %v, want ErrUnknownKey", lk, err)
	}

	// A key created without NOTIFY drops its old unknown entry, so evicting
	// the key later cannot resurface that entry.
	src.err = nil
	if _, err := r.Resolve(context.Background(), "h2"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("h2 before creation: %v, want ErrUnknownKey", err)
	}
	src.res["h2"] = apiaccess.Lookup{Key: apiaccess.Key{ID: 5}}
	src.res["h3"] = apiaccess.Lookup{Key: apiaccess.Key{ID: 6}}
	clock = clock.Add(2 * time.Minute)
	if lk, err := r.Resolve(context.Background(), "h2"); err != nil || lk.Key.ID != 5 {
		t.Fatalf("h2 after creation: %+v %v, want the key", lk, err)
	}
	r.SetMaxEntries(1)
	if _, err := r.Resolve(context.Background(), "h3"); err != nil {
		t.Fatalf("h3: %v", err) // evicts h2 from the known map
	}
	src.err = errors.New("db down")
	if lk, err := r.Resolve(context.Background(), "h2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("evicted h2 on a store error: %+v %v, want ErrUnavailable, not a leftover unknown entry", lk, err)
	}
}
