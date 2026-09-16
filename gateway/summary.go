package gateway

import (
	"fmt"
	"strings"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// SummaryText renders the daily Discord message.
func SummaryText(day time.Time, rows []apiaccess.UsageRow, newKeys []apiaccess.Key, dropped int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "API usage for %s\n", day.Format("2006-01-02"))
	if len(rows) == 0 {
		b.WriteString("no API traffic\n")
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "%s / %s: %d requests, %s, %d errors\n", r.Email, r.Game, r.Requests, humanBytes(r.Bytes), r.Errors)
	}
	for _, k := range newKeys {
		fmt.Fprintf(&b, "new key %s (%s) for account %d\n", k.Prefix, k.Label, k.AccountID)
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "%d usage rows dropped\n", dropped)
	}
	return b.String()
}
