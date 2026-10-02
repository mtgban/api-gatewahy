package gateway

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"golang.org/x/sync/singleflight"
)

// LookupSource is what the resolver caches in front of.
type LookupSource interface {
	LookupKey(ctx context.Context, hash string) (apiaccess.Lookup, error)
}

var (
	// ErrUnknownKey means no key row has this hash.
	ErrUnknownKey = errors.New("unknown key")
	// ErrUnavailable means the store failed and nothing is cached.
	ErrUnavailable = errors.New("lookup unavailable")
)

// DefaultMaxEntries bounds the cache of known keys.
const DefaultMaxEntries = 10000

// DefaultMaxUnknown bounds unknown-key entries separately, so forged keys
// cannot evict real ones.
const DefaultMaxUnknown = 1000

// DefaultStaleGrace bounds how long a store outage keeps stale entries alive.
const DefaultStaleGrace = 10 * time.Minute

// DefaultLookupTimeout bounds a single store lookup, so a blackholed store
// cannot hang the request; past it the lookup is treated as an error.
const DefaultLookupTimeout = 5 * time.Second

type cacheEntry struct {
	lookup  apiaccess.Lookup
	unknown bool
	fetched time.Time
}

// fetchResult is what a LookupKey call hands back to each waiting caller.
type fetchResult struct {
	lookup apiaccess.Lookup
	err    error
}

// Resolver caches key lookups for a TTL and serves stale entries for a
// bounded grace period when the store is down.
type Resolver struct {
	src        LookupSource
	ttl        time.Duration
	now        func() time.Time
	maxEntries int
	maxUnknown int
	group      singleflight.Group

	mu            sync.Mutex
	staleGrace    time.Duration
	lookupTimeout time.Duration
	cache         map[string]cacheEntry
	unknown       map[string]cacheEntry
	generation    uint64
}

// NewResolver wraps src with a ttl cache. now is injectable for tests.
func NewResolver(src LookupSource, ttl time.Duration, now func() time.Time) *Resolver {
	if now == nil {
		now = time.Now
	}
	return &Resolver{src: src, ttl: ttl, now: now, maxEntries: DefaultMaxEntries,
		maxUnknown: DefaultMaxUnknown, staleGrace: DefaultStaleGrace,
		lookupTimeout: DefaultLookupTimeout,
		cache:         map[string]cacheEntry{}, unknown: map[string]cacheEntry{}}
}

// SetStaleGrace bounds stale serving; d below zero restores DefaultStaleGrace.
func (r *Resolver) SetStaleGrace(d time.Duration) {
	if d < 0 {
		d = DefaultStaleGrace
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.staleGrace = d
}

// SetLookupTimeout bounds a store lookup; d at or below zero restores
// DefaultLookupTimeout.
func (r *Resolver) SetLookupTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultLookupTimeout
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookupTimeout = d
}

// SetMaxEntries bounds the known-key cache; n below one restores DefaultMaxEntries.
func (r *Resolver) SetMaxEntries(n int) {
	if n <= 0 {
		n = DefaultMaxEntries
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxEntries = n
}

// SetMaxUnknown bounds unknown-key entries; n below one restores DefaultMaxUnknown.
func (r *Resolver) SetMaxUnknown(n int) {
	if n <= 0 {
		n = DefaultMaxUnknown
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxUnknown = n
}

// Resolve returns the lookup for hash, from cache when fresh. Concurrent
// misses for one hash in one generation share a single store lookup.
func (r *Resolver) Resolve(ctx context.Context, hash string) (apiaccess.Lookup, error) {
	now := r.now()
	r.mu.Lock()
	e, ok := r.entryLocked(hash)
	grace := r.staleGrace
	timeout := r.lookupTimeout
	gen := r.generation
	r.mu.Unlock()
	if ok && now.Sub(e.fetched) < r.ttl {
		return e.result()
	}

	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Keying on gen keeps a caller from joining a fetch an Invalidate overtook.
	key := strconv.FormatUint(gen, 10) + ":" + hash
	resCh := r.group.DoChan(key, func() (any, error) {
		// Detached from the starting caller, so its giving up cannot fail the rest.
		lookupCtx, lookupCancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer lookupCancel()
		// A driver that ignores ctx must not pin the key: drop it once the lookup times out.
		defer context.AfterFunc(lookupCtx, func() { r.group.Forget(key) })()
		lk, err := r.src.LookupKey(lookupCtx, hash)
		return fetchResult{lookup: lk, err: err}, nil
	})

	select {
	case res := <-resCh:
		return r.storeFetch(hash, gen, grace, res.Val.(fetchResult))
	case <-fetchCtx.Done():
		// lib/pq won't honor ctx on a blocked read; the late result lands in
		// resCh, which is buffered, so an abandoned fetch never stores.
		return r.staleOrUnavailable(hash, gen, grace)
	}
}

// entryLocked finds hash in either map; a hash lives in at most one.
func (r *Resolver) entryLocked(hash string) (cacheEntry, bool) {
	if e, ok := r.cache[hash]; ok {
		return e, true
	}
	e, ok := r.unknown[hash]
	return e, ok
}

// storeFetch caches a completed fetch's result, unless an Invalidate for
// hash landed while the fetch was in flight.
func (r *Resolver) storeFetch(hash string, gen uint64, grace time.Duration, res fetchResult) (apiaccess.Lookup, error) {
	now := r.now()
	var e cacheEntry
	switch {
	case res.err == nil:
		e = cacheEntry{lookup: res.lookup, fetched: now}
	case errors.Is(res.err, apiaccess.ErrNotFound):
		e = cacheEntry{unknown: true, fetched: now}
	default:
		return r.staleOrUnavailable(hash, gen, grace)
	}
	r.mu.Lock()
	// An Invalidate that landed while the fetch above was in flight must
	// win: storing this result would resurrect a key it just revoked.
	if r.generation != gen {
		r.mu.Unlock()
		return e.result()
	}
	if e.unknown {
		delete(r.cache, hash)
		r.makeRoomLocked(r.unknown, r.maxUnknown, now)
		r.unknown[hash] = e
	} else {
		delete(r.unknown, hash)
		r.makeRoomLocked(r.cache, r.maxEntries, now)
		r.cache[hash] = e
	}
	r.mu.Unlock()
	return e.result()
}

// staleOrUnavailable serves hash's entry if a fresh now keeps it within
// ttl+grace and gen still matches: a NOTIFY mid-fetch wins over a stale copy.
func (r *Resolver) staleOrUnavailable(hash string, gen uint64, grace time.Duration) (apiaccess.Lookup, error) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != gen {
		return apiaccess.Lookup{}, ErrUnavailable
	}
	if e, ok := r.entryLocked(hash); ok && now.Sub(e.fetched) <= r.ttl+grace {
		return e.result()
	}
	return apiaccess.Lookup{}, ErrUnavailable
}

// makeRoomLocked drops m's expired entries, then arbitrary ones, until it
// is under limit; each is reconstructible.
func (r *Resolver) makeRoomLocked(m map[string]cacheEntry, limit int, now time.Time) {
	if len(m) < limit {
		return
	}
	for h, e := range m {
		if now.Sub(e.fetched) >= r.ttl+r.staleGrace {
			delete(m, h)
		}
	}
	for h := range m {
		if len(m) < limit {
			return
		}
		delete(m, h)
	}
}

func (e cacheEntry) result() (apiaccess.Lookup, error) {
	if e.unknown {
		return apiaccess.Lookup{}, ErrUnknownKey
	}
	return e.lookup, nil
}

// Invalidate drops one hash, or every entry when hash is empty.
func (r *Resolver) Invalidate(hash string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation++
	if hash == "" {
		r.cache = map[string]cacheEntry{}
		r.unknown = map[string]cacheEntry{}
		return
	}
	delete(r.cache, hash)
	delete(r.unknown, hash)
}
