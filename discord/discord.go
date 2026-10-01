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
	"strings"
	"time"
)

// Poster sends messages to one webhook.
type Poster struct {
	hook   string
	client *http.Client
}

// messageLimit is Discord's per-message character cap.
const messageLimit = 2000

// New returns a Poster. An empty hook logs instead of posting.
func New(hook string) *Poster {
	return &Poster{hook: hook, client: &http.Client{Timeout: 10 * time.Second}}
}

// Post sends msg as the message content, split across several posts if it
// exceeds Discord's character limit. It stops at the first failed chunk.
func (p *Poster) Post(ctx context.Context, msg string) error {
	for _, chunk := range splitMessage(msg) {
		if err := p.postOne(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

// splitMessage breaks msg into chunks at most messageLimit runes long,
// preferring line boundaries; a single oversized line is hard-split.
func splitMessage(msg string) []string {
	if msg == "" {
		return nil
	}
	var chunks []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			chunks = append(chunks, string(cur))
			cur = nil
		}
	}
	for _, line := range strings.Split(msg, "\n") {
		lr := []rune(line)
		if len(lr) > messageLimit {
			flush()
			for len(lr) > messageLimit {
				chunks = append(chunks, string(lr[:messageLimit]))
				lr = lr[messageLimit:]
			}
			cur = lr
			continue
		}
		grow := len(lr)
		if len(cur) > 0 {
			grow++ // separating newline
		}
		if len(cur)+grow > messageLimit {
			flush()
		}
		if len(cur) > 0 {
			cur = append(cur, '\n')
		}
		cur = append(cur, lr...)
	}
	flush()
	return chunks
}

// postOne sends one chunk as the message content.
func (p *Poster) postOne(ctx context.Context, msg string) error {
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
