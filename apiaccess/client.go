// Package apiaccess is the gateway's Postgres store: accounts, keys,
// entitlements, usage, and the reload notification channel.
package apiaccess

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mtgban/mtgban-website/timeseries"

	// registers the postgres driver database/sql opens by name
	_ "github.com/lib/pq"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("apiaccess: not found")

// Client wraps a connection pool and the schema it expects.
type Client struct {
	db *sql.DB

	// KnownStores restricts the store tokens AddEntitlement will store.
	// Empty accepts any token, but never DEV_ACCESS or an empty scope.
	KnownStores []string
}

// SetKnownStores sets the store tokens AddEntitlement accepts.
func (c *Client) SetKnownStores(stores []string) {
	c.KnownStores = stores
}

// NewClient opens a pool, pings, and ensures the schema.
func NewClient(cfg timeseries.SQLConfig) (*Client, error) {
	db, err := cfg.OpenDB()
	if err != nil {
		return nil, fmt.Errorf("apiaccess: open: %w", err)
	}
	return wrap(db)
}

func newClientDSN(dsn string) (*Client, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	return wrap(db)
}

func wrap(db *sql.DB) (*Client, error) {
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apiaccess: ping: %w", err)
	}
	if err := ensureSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apiaccess: ensure schema: %w", err)
	}
	return &Client{db: db}, nil
}

// Close shuts down the pool.
func (c *Client) Close() error {
	return c.db.Close()
}

// Ping reports whether the database answers.
func (c *Client) Ping() error {
	return c.db.Ping()
}

// PingContext reports whether the database answers before ctx ends.
func (c *Client) PingContext(ctx context.Context) error {
	return c.db.PingContext(ctx)
}
