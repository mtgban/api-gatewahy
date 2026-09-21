package portal

import (
	"fmt"
	"testing"
	"time"
)

func TestLimiterSlidingHour(t *testing.T) {
	var l limiter
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !l.allow("k", 3, now.Add(time.Duration(i)*time.Minute)) {
			t.Fatalf("hit %d refused", i)
		}
	}
	if l.allow("k", 3, now.Add(10*time.Minute)) {
		t.Error("fourth hit allowed")
	}
	if !l.allow("other", 3, now) {
		t.Error("other key refused")
	}
	if !l.allow("k", 3, now.Add(61*time.Minute)) {
		t.Error("hit after the window refused")
	}
	if l.allow("empty", 0, now) {
		t.Error("max 0 allowed")
	}
	if _, ok := l.hits["empty"]; ok {
		t.Error("refused key left an entry")
	}
}

func TestLimiterSweepShrinksMap(t *testing.T) {
	var l limiter
	now := time.Now()
	for i := 0; i < 300; i++ {
		l.allow(fmt.Sprintf("k%d", i), 1000, now)
	}
	if len(l.hits) != 300 {
		t.Fatalf("setup: %d keys", len(l.hits))
	}
	later := now.Add(2 * time.Hour)
	for i := 0; i < 256; i++ {
		l.allow(fmt.Sprintf("burst%d", i), 1000, later)
	}
	if len(l.hits) >= 300 {
		t.Errorf("sweep did not shrink the map: %d keys", len(l.hits))
	}
}
