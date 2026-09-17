package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/observability"
)

type fakeSink struct {
	mu      sync.Mutex
	batches [][]apiaccess.Usage
	touched []map[int64]time.Time
	fail    int
}

func (f *fakeSink) InsertUsage(_ context.Context, rows []apiaccess.Usage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return errors.New("db down")
	}
	cp := append([]apiaccess.Usage(nil), rows...)
	f.batches = append(f.batches, cp)
	return nil
}

func (f *fakeSink) TouchKeys(_ context.Context, seen map[int64]time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, seen)
	return nil
}

type blockingSink struct {
	mu      sync.Mutex
	batches [][]apiaccess.Usage
	release chan struct{}
}

func (b *blockingSink) InsertUsage(_ context.Context, rows []apiaccess.Usage) error {
	<-b.release
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := append([]apiaccess.Usage(nil), rows...)
	b.batches = append(b.batches, cp)
	return nil
}

func (b *blockingSink) TouchKeys(_ context.Context, _ map[int64]time.Time) error {
	return nil
}

type fakeEvents struct {
	mu  sync.Mutex
	evs []observability.Event
}

func (f *fakeEvents) Record(ev observability.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evs = append(f.evs, ev)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never met")
}

func TestMeterFlushesByBatch(t *testing.T) {
	sink := &fakeSink{}
	events := &fakeEvents{}
	m := NewUsageMeter(sink, events, "test", time.Hour, 2)
	defer func() { _ = m.Close() }()

	m.Record(apiaccess.Usage{KeyID: 1, AccountID: 5, Path: "/api/mtgban/retail.json", Ts: now})
	m.Record(apiaccess.Usage{KeyID: 1, AccountID: 5, Path: "/api/mtgban/buylist.json", Ts: now.Add(time.Second)})

	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.batches) == 1 && len(sink.touched) == 1
	})
	sink.mu.Lock()
	if len(sink.batches[0]) != 2 || len(sink.touched) != 1 || !sink.touched[0][1].Equal(now.Add(time.Second)) {
		t.Errorf("batches %+v touched %+v", sink.batches, sink.touched)
	}
	sink.mu.Unlock()

	events.mu.Lock()
	if len(events.evs) != 2 || events.evs[0].Tier != "api" || events.evs[0].Device != "api" ||
		events.evs[0].Visitor != "5" || events.evs[0].Instance != "test" || events.evs[0].Path != "/api/mtgban/retail.json" {
		t.Errorf("events %+v", events.evs)
	}
	events.mu.Unlock()
}

func TestMeterFlushesByTime(t *testing.T) {
	sink := &fakeSink{}
	m := NewUsageMeter(sink, nil, "test", 50*time.Millisecond, 100)
	defer func() { _ = m.Close() }()
	m.Record(apiaccess.Usage{KeyID: 1, Ts: now})
	waitFor(t, func() bool { sink.mu.Lock(); defer sink.mu.Unlock(); return len(sink.batches) == 1 })
}

func TestMeterRetriesThenDrops(t *testing.T) {
	sink := &fakeSink{fail: 2}
	m := NewUsageMeter(sink, nil, "test", time.Hour, 1)
	m.retryPause = time.Millisecond
	m.Record(apiaccess.Usage{KeyID: 1, Ts: now})
	waitFor(t, func() bool { sink.mu.Lock(); defer sink.mu.Unlock(); return len(sink.batches) == 1 })
	if m.Dropped() != 0 {
		t.Errorf("dropped %d after successful retry", m.Dropped())
	}

	sink.mu.Lock()
	sink.fail = 3
	sink.mu.Unlock()
	m.Record(apiaccess.Usage{KeyID: 2, Ts: now})
	waitFor(t, func() bool { return m.Dropped() == 1 })
	_ = m.Close()
}

func TestMeterCloseFlushes(t *testing.T) {
	sink := &fakeSink{}
	m := NewUsageMeter(sink, nil, "test", time.Hour, 100)
	m.Record(apiaccess.Usage{KeyID: 1, Ts: now})
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if len(sink.batches) != 1 {
		t.Errorf("close did not flush: %+v", sink.batches)
	}
}

func TestMeterCloseTwice(t *testing.T) {
	sink := &fakeSink{}
	m := NewUsageMeter(sink, nil, "test", time.Hour, 100)
	m.Record(apiaccess.Usage{KeyID: 1, Ts: now})

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	if len(sink.batches) != 1 {
		t.Errorf("expected exactly one flushed batch, got: %+v", sink.batches)
	}
}

func TestMeterDropsWhenBufferFull(t *testing.T) {
	sink := &blockingSink{release: make(chan struct{})}
	m := NewUsageMeter(sink, nil, "test", time.Hour, 1)

	for i := 0; i < 10; i++ {
		m.Record(apiaccess.Usage{KeyID: int64(i), Ts: now})
	}

	waitFor(t, func() bool { return m.Dropped() >= 1 })

	close(sink.release)
	_ = m.Close()

	sink.mu.Lock()
	flushed := 0
	for _, b := range sink.batches {
		flushed += len(b)
	}
	sink.mu.Unlock()

	if got := int64(flushed) + m.Dropped(); got != 10 {
		t.Errorf("flushed %d + dropped %d = %d, want 10", flushed, m.Dropped(), got)
	}
}
