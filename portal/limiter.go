package portal

import (
	"sync"
	"time"
)

// limiter counts events per key over a sliding hour.
type limiter struct {
	mu    sync.Mutex
	hits  map[string][]time.Time
	calls int
}

// allow records one event for key unless max already happened in the last hour.
func (l *limiter) allow(key string, max int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	l.calls++
	if l.calls%256 == 0 {
		l.sweep(now)
	}
	cutoff := now.Add(-time.Hour)
	old := l.hits[key]
	kept := make([]time.Time, 0, len(old))
	for _, t := range old {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// sweep drops keys whose newest hit is older than an hour, run every 256th call.
func (l *limiter) sweep(now time.Time) {
	cutoff := now.Add(-time.Hour)
	for k, hits := range l.hits {
		if len(hits) == 0 || hits[len(hits)-1].Before(cutoff) {
			delete(l.hits, k)
		}
	}
}
