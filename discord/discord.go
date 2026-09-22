// Package discord posts plain messages to a webhook.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Poster sends messages to one webhook.
type Poster struct {
	hook   string
	client *http.Client
}

// New returns a Poster. An empty hook logs instead of posting.
func New(hook string) *Poster {
	return &Poster{hook: hook, client: &http.Client{Timeout: 10 * time.Second}}
}

// Post sends msg as the message content.
func (p *Poster) Post(ctx context.Context, msg string) error {
	if p.hook == "" {
		log.Println("discord (no hook):", msg)
		return nil
	}
	// No mention parsing: message text can carry customer-supplied labels.
	body, err := json.Marshal(map[string]any{"content": msg, "allowed_mentions": map[string][]string{"parse": {}}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.hook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		// The hook URL is a credential, so report the error class only.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Err != nil {
			err = oe.Err
		}
		return fmt.Errorf("discord: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("discord: status %d", resp.StatusCode)
	}
	return nil
}
