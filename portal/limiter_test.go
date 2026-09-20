package portal

import (
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
