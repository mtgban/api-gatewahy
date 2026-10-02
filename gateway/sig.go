package gateway

import (
	"net/url"
	"strings"
	"time"

	"github.com/mtgban/mtgban-website/apisig"
)

// mintSig builds the one set of claims the handler and the prober both sign
// with, so a claims change cannot pass the probe while live traffic fails.
func mintSig(up Upstream, link, email, scope string, modes []string, expires time.Time) string {
	return apisig.Mint(up.Secret, link, apisig.Claims{
		API: scope,
		// The field names are apisig.APIFields.
		Fields: url.Values{
			"APImode":   {strings.Join(modes, ",")},
			"UserEmail": {email},
		},
		Expires: expires.Unix(),
	})
}
