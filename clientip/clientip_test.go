package clientip

import (
	"net/http/httptest"
	"testing"
)

func TestFromRequestTakesTheLastHeaderValue(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 203.0.113.5")
	if got := FromRequest(req, "X-Forwarded-For"); got != "203.0.113.5" {
		t.Errorf("last hop: %q", got)
	}
	req.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := FromRequest(req, "X-Forwarded-For"); got != "198.51.100.9" {
		t.Errorf("fallback: %q", got)
	}
}

func TestFromRequestReadsLastOfRepeatedHeaderLines(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Add("X-Forwarded-For", "10.0.0.1")
	req.Header.Add("X-Forwarded-For", "203.0.113.5")
	if got := FromRequest(req, "X-Forwarded-For"); got != "203.0.113.5" {
		t.Errorf("last header line: got %q", got)
	}
}
