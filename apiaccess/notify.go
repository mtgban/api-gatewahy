package apiaccess

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/lib/pq"
)

// ReloadChannel carries key hashes whose cached lookups must be dropped.
// An empty payload means drop everything.
const ReloadChannel = "apiaccess_reload"

// Notify tells every listening gateway to drop its cache entry for payload.
func (c *Client) Notify(ctx context.Context, payload string) error {
	_, err := c.db.ExecContext(ctx, "SELECT pg_notify($1, $2)", ReloadChannel, payload)
	return err
}

type listener struct {
	pl       *pq.Listener
	done     chan struct{}
	once     sync.Once
	closeErr error
}

// Listen subscribes to ReloadChannel on its own connection. onPayload runs
// per notification, onReconnect when notifications may have been lost.
func Listen(dsn string, onPayload func(string), onReconnect func(), onErr func(error)) (io.Closer, error) {
	pl := pq.NewListener(dsn, 10*time.Second, time.Minute, func(_ pq.ListenerEventType, err error) {
		if err != nil {
			onErr(err)
		}
	})
	if err := pl.Listen(ReloadChannel); err != nil {
		_ = pl.Close()
		return nil, err
	}
	l := &listener{pl: pl, done: make(chan struct{})}
	go func() {
		for {
			select {
			case <-l.done:
				return
			case n, ok := <-pl.Notify:
				if !ok {
					return
				}
				if n == nil {
					onReconnect()
					continue
				}
				onPayload(n.Extra)
			case <-time.After(90 * time.Second):
				go func() { _ = pl.Ping() }()
			}
		}
	}()
	return l, nil
}

func (l *listener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.closeErr = l.pl.Close()
	})
	return l.closeErr
}
