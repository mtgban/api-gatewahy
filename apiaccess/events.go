package apiaccess

import (
	"context"
	"time"
)

// eventReclaimAfter is how long an unfinished claim blocks a retry.
const eventReclaimAfter = 5 * time.Minute

// BeginStripeEvent claims the event. False means it was handled already or
// is being handled right now.
func (c *Client) BeginStripeEvent(ctx context.Context, id, typ string) (bool, error) {
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO stripe_events (id, type) VALUES ($1, $2)
		 ON CONFLICT (id) DO UPDATE SET received_at = now()
		 WHERE stripe_events.processed_at IS NULL AND stripe_events.received_at < now() - make_interval(secs => $3)`,
		id, typ, eventReclaimAfter.Seconds())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishStripeEvent marks the event processed.
func (c *Client) FinishStripeEvent(ctx context.Context, id string) error {
	_, err := c.db.ExecContext(ctx, `UPDATE stripe_events SET processed_at = now() WHERE id = $1`, id)
	return err
}

// DeleteStripeEvent drops the claim so Stripe's retry is handled afresh.
func (c *Client) DeleteStripeEvent(ctx context.Context, id string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM stripe_events WHERE id = $1`, id)
	return err
}
