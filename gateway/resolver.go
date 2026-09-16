package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
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

// DefaultMaxEntries bounds the cache so unknown keys cannot grow it forever.
const DefaultMaxEntries = 10000

// DefaultStaleGrace bounds how long a store outage keeps stale entries alive.
const DefaultStaleGrace = 10 * time.Minute

type cacheEntry struct {
	lookup  apiaccess.Lookup
	unknown bool
	fetched time.Time
}

// Resolver caches key lookups for a TTL and serves stale entries for a
// bounded grace period when the store is down.
type Resolver struct {
	src        LookupSource
	ttl        time.Duration
	now        func() time.Time
	maxEntries int

	mu         sync.Mutex
	staleGrace time.Duration
	cache      map[string]cacheEntry
}

// NewResolver wraps src with a ttl cache. now is injectable for tests.
func NewResolver(src LookupSource, ttl time.Duration, now func() time.Time) *Resolver {
	if now == nil {
		now = time.Now
	}
	return &Resolver{src: src, ttl: ttl, now: now, maxEntries: DefaultMaxEntries,
		staleGrace: DefaultStaleGrace, cache: map[string]cacheEntry{}}
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

// SetMaxEntries bounds the cache; n below one restores DefaultMaxEntries.
func (r *Resolver) SetMaxEntries(n int) {
	if n <= 0 {
		n = DefaultMaxEntries
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxEntries = n
}

// Resolve returns the lookup for hash, from cache when fresh.
func (r *Resolver) Resolve(ctx context.Context, hash string) (apiaccess.Lookup, error) {
	now := r.now()
	r.mu.Lock()
	e, ok := r.cache[hash]
	grace := r.staleGrace
	r.mu.Unlock()
	if ok && now.Sub(e.fetched) < r.ttl {
		return e.result()
	}

	lk, err := r.src.LookupKey(ctx, hash)
	switch {
	case err == nil:
		e = cacheEntry{lookup: lk, fetched: now}
	case errors.Is(err, apiaccess.ErrNotFound):
		e = cacheEntry{unknown: true, fetched: now}
	default:
		// A stale entry outlives its ttl by the grace period, no longer.
		if ok && now.Sub(e.fetched) <= r.ttl+grace {
			return e.result()
		}
		return apiaccess.Lookup{}, ErrUnavailable
	}
	r.mu.Lock()
	r.makeRoomLocked(now)
	r.cache[hash] = e
	r.mu.Unlock()
	return e.result()
}

// makeRoomLocked drops expired entries, then arbitrary ones; each is reconstructible.
func (r *Resolver) makeRoomLocked(now time.Time) {
	if len(r.cache) < r.maxEntries {
		return
	}
	for h, e := range r.cache {
		if now.Sub(e.fetched) >= r.ttl+r.staleGrace {
			delete(r.cache, h)
		}
	}
	for h := range r.cache {
		if len(r.cache) < r.maxEntries {
			return
		}
		delete(r.cache, h)
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
	if hash == "" {
		r.cache = map[string]cacheEntry{}
		return
	}
	delete(r.cache, hash)
}
