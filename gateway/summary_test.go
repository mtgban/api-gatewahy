package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

func TestSummaryText(t *testing.T) {
	day := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	rows := []apiaccess.UsageRow{
		{Email: "ck@example.com", Game: "magic", Requests: 120, Bytes: 5 << 20, Errors: 2},
		{Email: "ck@example.com", Game: "pokemon", Requests: 3, Bytes: 1024, Errors: 0},
	}
	keys := []apiaccess.Key{{Prefix: "abcd1234", Label: "prod"}}
	got := SummaryText(day, rows, keys, 7)
	for _, want := range []string{"2026-09-14", "ck@example.com", "magic", "120", "5.0 MB", "2 errors", "pokemon", "abcd1234", "prod", "7 usage rows dropped"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
	empty := SummaryText(day, nil, nil, 0)
	if !strings.Contains(empty, "no API traffic") {
		t.Errorf("empty summary:\n%s", empty)
	}
}
