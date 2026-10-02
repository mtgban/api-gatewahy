// Package apiaccess is the gateway's Postgres store: accounts, keys,
// entitlements, usage, and the reload notification channel.
package apiaccess

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	// registers the postgres driver database/sql opens by name
	_ "github.com/lib/pq"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("apiaccess: not found")

// ErrStripeEntitlement is returned when EndEntitlement is asked to end a
// Stripe-sourced row: the next reconcile would just restore it from Stripe.
var ErrStripeEntitlement = errors.New("apiaccess: a Stripe entitlement is cancelled in Stripe; suspend the account for an immediate cut-off")

// Client wraps a connection pool and the schema it expects.
type Client struct {
	db *sql.DB
}

// NewClient opens a pool, pings, and applies pending migrations.
func NewClient(ctx context.Context, cfg SQLConfig) (*Client, error) {
	db, err := cfg.OpenDB()
	if err != nil {
		return nil, fmt.Errorf("apiaccess: open: %w", err)
	}
	return wrap(ctx, db)
}

// OpenDSN opens a pool from a Postgres URL, pings, and applies pending migrations.
func OpenDSN(ctx context.Context, dsn string) (*Client, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	return wrap(ctx, db)
}

func wrap(ctx context.Context, db *sql.DB) (*Client, error) {
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apiaccess: ping: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apiaccess: migrate: %w", err)
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
