// Package mailer sends the portal's mail: one interface, an SMTP
// implementation with STARTTLS, and a logging one for development and tests.
package mailer

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"time"
)

// Mailer sends one message with a text body and an optional HTML twin.
type Mailer interface {
	Send(ctx context.Context, to, subject, text, html string) error
}

// SMTP sends through one server with STARTTLS and PLAIN auth.
type SMTP struct {
	Host string
	Port int
	User string
	Pass string
	// From is the header value, for example "MTGBAN <no-reply@mtgban.com>";
	// the header keeps the display name but MAIL FROM uses the bare address.
	From string
}

// Send implements Mailer.
func (s *SMTP) Send(ctx context.Context, to, subject, text, html string) error {
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("mailer: from: %w", err)
	}
	rcpt, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("mailer: to: %w", err)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)))
	if err != nil {
		return fmt.Errorf("mailer: dial: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mailer: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
		return fmt.Errorf("mailer: starttls: %w", err)
	}
	if s.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Pass, s.Host)); err != nil {
			return fmt.Errorf("mailer: auth: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("mailer: mail from: %w", err)
	}
	if err := c.Rcpt(rcpt.Address); err != nil {
		return fmt.Errorf("mailer: rcpt: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mailer: data: %w", err)
	}
	if _, err := w.Write(Message(from.String(), rcpt.Address, subject, text, html)); err != nil {
		return fmt.Errorf("mailer: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: send: %w", err)
	}
	return c.Quit()
}

// Message renders an RFC 5322 message with text and, when html is set, an HTML alternative.
func Message(from, to, subject, text, html string) []byte {
	var b bytes.Buffer
	boundary := "ban-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\n",
		from, to, mime.QEncoding.Encode("utf-8", subject), time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	part := func(ctype, body string) {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", boundary, ctype)
		qp := quotedprintable.NewWriter(&b)
		_, _ = qp.Write([]byte(body))
		_ = qp.Close()
		b.WriteString("\r\n")
	}
	part("text/plain", text)
	if html != "" {
		part("text/html", html)
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

// Log writes mail to Out instead of sending it, for development and tests.
type Log struct {
	Out io.Writer
}

// Send implements Mailer.
func (l *Log) Send(_ context.Context, to, subject, text, _ string) error {
	_, err := fmt.Fprintf(l.Out, "mail to %s: %s\n%s\n", to, subject, text)
	return err
}

// FromEnv builds the SMTP mailer from MAIL_SMTP_HOST, MAIL_SMTP_PORT (587),
// MAIL_SMTP_USER, and MAIL_SMTP_PASS. No host means nil, nil: use Log.
func FromEnv(from string) (*SMTP, error) {
	host := os.Getenv("MAIL_SMTP_HOST")
	if host == "" {
		return nil, nil
	}
	port := 587
	if p := os.Getenv("MAIL_SMTP_PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return nil, errors.New("MAIL_SMTP_PORT must be a port number")
		}
		port = n
	}
	if _, err := mail.ParseAddress(from); err != nil {
		return nil, fmt.Errorf("mail.from: %w", err)
	}
	return &SMTP{Host: host, Port: port, User: os.Getenv("MAIL_SMTP_USER"), Pass: os.Getenv("MAIL_SMTP_PASS"), From: from}, nil
}
