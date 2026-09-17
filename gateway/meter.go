package gateway

import (
	"context"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/observability"
)

// UsageSink stores usage rows and last-used marks.
type UsageSink interface {
	InsertUsage(ctx context.Context, rows []apiaccess.Usage) error
	TouchKeys(ctx context.Context, seen map[int64]time.Time) error
}

// EventSink receives one observability event per request.
type EventSink interface {
	Record(ev observability.Event)
}

// UsageMeter batches usage rows so metering never blocks a request.
type UsageMeter struct {
	sink       UsageSink
	events     EventSink
	instance   string
	flushEvery time.Duration
	batch      int
	retryPause time.Duration

	in      chan apiaccess.Usage
	done    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
	dropped atomic.Int64
}

// NewUsageMeter starts the flushing goroutine.
func NewUsageMeter(sink UsageSink, events EventSink, instance string, flushEvery time.Duration, batch int) *UsageMeter {
	if batch <= 0 {
		batch = 100
	}
	if flushEvery <= 0 {
		flushEvery = time.Minute
	}
	m := &UsageMeter{
		sink:       sink,
		events:     events,
		instance:   instance,
		flushEvery: flushEvery,
		batch:      batch,
		retryPause: time.Second,
		in:         make(chan apiaccess.Usage, 4*batch),
		done:       make(chan struct{}),
	}
	m.wg.Add(1)
	go m.run()
	return m
}

// Record queues u and emits its observability event. Never blocks.
func (m *UsageMeter) Record(u apiaccess.Usage) {
	if m.events != nil {
		m.events.Record(observability.Event{
			Ts:       u.Ts,
			Path:     u.Path,
			Tier:     "api",
			Device:   "api",
			Visitor:  strconv.FormatInt(u.AccountID, 10),
			Instance: m.instance,
		})
	}
	select {
	case m.in <- u:
	default:
		m.dropped.Add(1)
	}
}

// Dropped counts rows lost to a full buffer or a failed flush.
func (m *UsageMeter) Dropped() int64 {
	return m.dropped.Load()
}

func (m *UsageMeter) run() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.flushEvery)
	defer ticker.Stop()
	var pending []apiaccess.Usage
	for {
		select {
		case u := <-m.in:
			pending = append(pending, u)
			if len(pending) >= m.batch {
				m.flush(pending)
				pending = nil
			}
		case <-ticker.C:
			if len(pending) > 0 {
				m.flush(pending)
				pending = nil
			}
		case <-m.done:
			for {
				select {
				case u := <-m.in:
					pending = append(pending, u)
				default:
					if len(pending) > 0 {
						m.flush(pending)
					}
					return
				}
			}
		}
	}
}

func (m *UsageMeter) flush(rows []apiaccess.Usage) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = m.sink.InsertUsage(ctx, rows); err == nil {
			break
		}
		if attempt < 2 {
			time.Sleep(m.retryPause)
		}
	}
	if err != nil {
		m.dropped.Add(int64(len(rows)))
		log.Printf("meter: dropped %d usage rows: %v", len(rows), err)
		return
	}
	seen := map[int64]time.Time{}
	for _, u := range rows {
		if u.Ts.After(seen[u.KeyID]) {
			seen[u.KeyID] = u.Ts
		}
	}
	if err := m.sink.TouchKeys(ctx, seen); err != nil {
		log.Printf("meter: touch keys: %v", err)
	}
}

// Close flushes pending rows and stops the goroutine. Safe to call more than once.
func (m *UsageMeter) Close() error {
	m.once.Do(func() {
		close(m.done)
		m.wg.Wait()
	})
	return nil
}
