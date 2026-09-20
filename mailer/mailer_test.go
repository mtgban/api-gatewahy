package mailer

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestMessageHasBothParts(t *testing.T) {
	msg := string(Message("MTGBAN <no-reply@mtgban.com>", "ann@example.com", "Your link", "plain body", "<p>html body</p>"))
	for _, want := range []string{"From: MTGBAN <no-reply@mtgban.com>\r\n", "To: ann@example.com\r\n", "Subject: Your link\r\n", "MIME-Version: 1.0\r\n",
		"Content-Type: multipart/alternative; boundary=", "Content-Type: text/plain; charset=utf-8", "plain body", "Content-Type: text/html; charset=utf-8", "<p>html body</p>"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if !strings.HasSuffix(msg, "--\r\n") {
		t.Error("message does not end with the closing boundary")
	}
	textOnly := string(Message("a@b.c", "d@e.f", "s", "just text", ""))
	if strings.Contains(textOnly, "text/html") {
		t.Error("empty html still produced an html part")
	}
}

func TestLogMailerWritesTheText(t *testing.T) {
	var buf bytes.Buffer
	m := &Log{Out: &buf}
	if err := m.Send(context.Background(), "ann@example.com", "Your link", "visit https://x/y", "<p>x</p>"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "ann@example.com") || !strings.Contains(buf.String(), "https://x/y") {
		t.Errorf("log %q", buf.String())
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("MAIL_SMTP_HOST", "")
	if m, err := FromEnv("MTGBAN <no-reply@mtgban.com>"); m != nil || err != nil {
		t.Errorf("unset host: %+v %v", m, err)
	}
	t.Setenv("MAIL_SMTP_HOST", "smtp.example.com")
	t.Setenv("MAIL_SMTP_PORT", "")
	t.Setenv("MAIL_SMTP_USER", "u")
	t.Setenv("MAIL_SMTP_PASS", "p")
	m, err := FromEnv("MTGBAN <no-reply@mtgban.com>")
	if err != nil || m.Port != 587 || m.User != "u" || m.From != "MTGBAN <no-reply@mtgban.com>" {
		t.Errorf("%+v %v", m, err)
	}
	t.Setenv("MAIL_SMTP_PORT", "abc")
	if _, err := FromEnv("x <a@b.c>"); err == nil {
		t.Error("bad port accepted")
	}
	t.Setenv("MAIL_SMTP_PORT", "465")
	if _, err := FromEnv("not an address"); err == nil {
		t.Error("bad from accepted")
	}
}
