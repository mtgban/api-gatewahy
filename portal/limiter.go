package portal

import (
	"sync"
	"time"
)

// limiter counts events per key over a sliding hour.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

// allow records one event for key unless max already happened in the last hour.
func (l *limiter) allow(key string, max int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	cutoff := now.Add(-time.Hour)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = kept
	}
	if len(kept) >= max {
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}
