package gateway

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mtgban/mtgban-website/apisig"
)

// mintSig builds the one set of claims the handler and the prober both sign
// with, so a claims change cannot pass the probe while live traffic fails.
// A positive lookbackDays adds APIlookback; zero mints the fields price
// keys always carried.
func mintSig(up Upstream, link, email, scope string, modes []string, lookbackDays int, expires time.Time) string {
	fields := url.Values{
		"APImode":   {strings.Join(modes, ",")},
		"UserEmail": {email},
	}
	if lookbackDays > 0 {
		fields.Set("APIlookback", strconv.Itoa(lookbackDays))
	}
	return apisig.Mint(up.Secret, link, apisig.Claims{API: scope, Fields: fields, Expires: expires.Unix()})
}
