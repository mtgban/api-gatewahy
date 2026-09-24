# api-gatewahy

The MTGBAN API gateway. It fronts the per-game BAN price APIs behind one
bearer key per customer, valid across every game the customer's plan
covers, instead of the per-game signed links the website mints today.
Requests carry the customer's key; the gateway resolves it against
entitlements in Postgres, mints a short-lived backend signature for the
target game, and reverse-proxies to that game's own API host.

Accounts, keys, and entitlements are managed by operators through this
binary's admin subcommands in phase 1. Phase 2 adds Stripe billing so
entitlements are written directly from checkout and webhooks, and phase 3
adds the customer portal for self-service signup and account management.
The gateway also runs a background prober, a daily
usage summary posted to Discord, and cache invalidation over Postgres
LISTEN/NOTIFY so key and entitlement changes take effect without a
restart.

## Public API

Base URL: `https://api.mtgban.com/v1/{game}/`, forwarding to that game's
`/api/mtgban/...` paths (retail, buylist, all, sealed, sets, stores,
search). Only `GET` is accepted; any other method under `/v1/` is 405.

Authenticate with either:

1. `Authorization: Bearer mtgban_live_...`, the form to use in production.
2. `?key=mtgban_live_...`, a convenience for curl and for spreadsheet tools
   that cannot set a header. The key travels in the URL, so it lands in
   access logs, proxy logs, and browser history; prefer the header wherever
   the caller can send one.

A request with neither returns 401. Errors are JSON:

```json
{"error": "plan does not include game", "game": "pokemon"}
```

| Status | Meaning |
| --- | --- |
| 401 | Missing, malformed, revoked, or unknown key. |
| 403 | Key valid, but the plan lacks the game or the mode. Body names which. |
| 404 | Unknown game or path. |
| 405 | A method other than `GET` under `/v1/`. |
| 429 | Per-key limit exceeded. Carries `RateLimit-Limit` like the backend does. |
| 502 | Upstream unreachable, returned any status outside 2xx except 304 (redirects included), or is misconfigured. |
| 503 | Database unavailable and the key was not in cache. |
| 504 | Upstream exceeded the timeout. |

Meta endpoints, unauthenticated: `/healthz` (200 when the database pings
and at least one game is configured) and `/v1/games.json` (the configured
game names).

## Customer pages

When `GATEWAY_SESSION_SECRET` is set the gateway also serves the customer
portal on the same host. Game sites render the pricing page and hand off
here; every page links back to the site the customer came from. Portal
pages send `X-Frame-Options: DENY`, `Content-Security-Policy: frame-ancestors
'none'`, and `Referrer-Policy: no-referrer`; `/static/portal.css` is the
only static asset.

| Route | Auth | Purpose |
| --- | --- | --- |
| `GET /` | none | Redirects to `pricing_url`. |
| `GET /checkout` | none | Receives the plan from a game site's configurator; shows login or the confirm page. |
| `POST /checkout` | session + CSRF | Creates the Stripe Checkout Session. |
| `GET /checkout/success`, `/checkout/cancel` | session (success only) | Landing after Stripe. |
| `GET /login`, `POST /login`, `GET /login/{token}`, `POST /login/{token}` | none | Magic-link sign-in; links live 15 minutes. `GET /login/{token}` confirms and `POST /login/{token}` signs in; the link is consumed on the sign-in POST. |
| `POST /logout` | session + CSRF | Clears the cookie and bumps the account's session epoch, so every cookie issued before it stops working. |
| `GET /account`, `POST /account/keys`, `POST /account/keys/{id}/revoke`, `POST /account/plan`, `GET /portal` | session (+ CSRF on POST) | Keys, usage, entitlements, Stripe portal, plan change. |
| `GET /trial`, `POST /trial`, `GET /session`, `POST /session` | signed handoff token | `GET /trial` and `GET /session` show a confirm page; `POST /trial` grants the Patreon trial and `POST /session` signs in. Each handoff token is single-use (its nonce is burned on accept). |
| `GET /admin/...` | session, email in `admin_emails` | Accounts, entitlements, invites, usage, reconcile. Each account page shows an activity log of admin actions taken on it, from the web admin and from the CLI alike. |

Sessions are a signed cookie (`ban_session`, 30 days, host-only); suspending
an account ends its sessions on the next request. The trial lasts
`trial_days` (15 by default) of all data for every configured game, once
per Patreon email every 180 days; a reminder mails three days before it
ends. Rotating `GATEWAY_SESSION_SECRET` signs everyone out, which is the
emergency logout. A Patreon handoff token names the game site that minted
it and is verified with that game's `secret` from `games`, the same value
the site keeps under `api_user_secrets["gateway@mtgban.com"]`, so the
handoff needs no secret of its own. Token-consuming posts (`/login/{token}`, `/trial`,
`/session`) are accepted only from the gateway's own origin
(`Sec-Fetch-Site` or `Origin`), so a foreign page cannot sign a visitor
into someone else's account; the sign-in email form (`POST /login`) has
the same requirement. An account can create at most 10 keys per hour.

## Admin subcommands

All admin subcommands take `-config <path>` (or `BAN_CONFIG_PATH`). The flag
may appear anywhere in the command line, before or after the verb:
`account add -config config.json -email x@example.com` and
`account -config config.json add -email x@example.com` are the same command.

`account add | suspend | reinstate | list`

- `add -email -note`: creates an account.
- `suspend -email`, `reinstate -email`: flips status between `active` and
  `suspended`.
- `list`: table of every account.

`key create | revoke | list`

- `create -email -label`: prints the plaintext key once.
- `revoke -prefix`: revokes by the prefix shown in `key list`.
- `list -email`: table of an account's keys.

`grant add | end | list`

- `add -email -games -stores -modes -until -note`: `-games` is a
  comma-separated game list, each name one of the games in the config,
  `-stores` is `ALL_ACCESS`, `BASE_ACCESS`, or a comma-separated store list
  drawn from `known_stores`, `-modes` is a comma-separated subset of
  `retail,buylist,sealed`, `-until` is `YYYY-MM-DD` (open-ended if omitted).
  Store shorthands are case-sensitive and must match the backend's spelling,
  for example `TCGLow` or `CK`.
- `end -id`: ends an entitlement by id as of now.
- `list -email`: table of an account's entitlements.

`usage`: `-since -until -email`, dates as `YYYY-MM-DD`. Defaults to the
last 30 days, all accounts. Prints requests, bytes, and errors by account
and game.

### Billing subcommands

These need `STRIPE_SECRET_KEY` in the environment.

`catalog seed`: creates or updates one Stripe Product per catalog package and
add-on and one Price per item and interval, looked up by `lookup_key`
(`starter_monthly`, `extra_game_quarterly`, ...). Rerunning is a no-op unless
the catalog changed; a changed amount creates a replacement Price and archives
the old one. Run it in test mode and again in live mode.

`checkout link -email -package [-games] [-stores] [-interval] [-invite]`:
prints a hosted Checkout URL for the account. `-games` defaults to `magic`,
which is always included. `-stores` applies to `starter` only and lists the
extra stores from the catalog's `selectable_stores` (TCGplayer is implied).
`-interval` defaults to `monthly`; `quarterly` needs `-invite` with a token
from `invite create`. The account must already exist (`account add`).

`invite create -interval quarterly [-email] [-days 14] [-note]`: prints a
one-time token, optionally bound to an email, that unlocks a non-public
interval for one checkout. The token is consumed when the Checkout Session is
created, and handed back if that creation fails. Landing on the cancel page
expires the session at Stripe first; the token comes back only once Stripe
confirms the expiry, so a session left open cannot be paid with a token that
was also reused. A session abandoned without the cancel page keeps its token
spent until Stripe expires it on its own.

`stripe reconcile [-sub sub_123]`: rebuilds the entitlement row for one
subscription, or for every subscription Stripe lists plus every active
`stripe` row Stripe no longer lists. `serve` runs the full pass nightly.

`portal link -email [-return URL]`: prints a Customer Portal URL where the
customer can update their card, see invoices, and cancel at period end.

`plan change -email -package [-games] [-stores] [-sub]`: rewrites the
subscription's items and metadata to the new plan with proration, keeping the
current interval, then reconciles. `-sub` is only needed when the account has
more than one active Stripe subscription.

`serve -config`: runs the gateway HTTP server.

`version`: prints the build version.

## Configuration

JSON, named by `-config` or `BAN_CONFIG_PATH` (a `b2://` path needs
`BAN_CONFIG_KEY` and `BAN_CONFIG_SECRET`). See `config/config.go`.

| Key | Default |
| --- | --- |
| `port` | `8080` |
| `instance_name` | `api-gatewahy` |
| `link` | `http://www.mtgban.com` |
| `client_ip_header` | `DO-Connecting-IP` |
| `public_url` | `https://api.mtgban.com` |
| `gateway_email` | required, no default |
| `apiaccess_config` | required, no default |
| `observability_config` | (none, observability disabled) |
| `discord_api_notif_hook` | (none, alerts disabled) |
| `games` | required, no default |
| `known_stores` | (none) |
| `cache_ttl_seconds` | `60` |
| `stale_grace_seconds` | `600` |
| `per_key_requests_per_sec` | `10` |
| `per_key_burst` | `5` |
| `per_ip_requests_per_sec` | `50` |
| `per_ip_burst` | `100` |
| `upstream_timeout_seconds` | `300` |
| `shutdown_grace_seconds` | `60` |
| `usage_retention_days` | `395` |
| `stripe.grace_days` | `10` |
| `stripe.success_path` | `/checkout/success` |
| `stripe.cancel_path` | `/checkout/cancel` |
| `pricing_url` | `https://mtgban.com/api-plans` |
| `admin_emails` | (none) |
| `mail.from` | `MTGBAN <no-reply@mtgban.com>` |
| `trial_days` | `15` |
| `login_links_per_hour` | `5` |

`cache_ttl_seconds` is how long a resolved key stays cached, and
`stale_grace_seconds` is how much longer the gateway keeps serving that
cached answer while the database is failing; past the two summed, a lookup
returns 503 instead. A revoked key may therefore keep working for at most
`cache_ttl_seconds + stale_grace_seconds` during a database outage, 11
minutes on the defaults. LISTEN/NOTIFY drops the cached entry immediately
when the database is reachable.

`client_ip_header` names the one header the gateway and the portal trust for
the client address. It defaults to `DO-Connecting-IP`, which DigitalOcean App
Platform's ingress sets to the real peer and which a client cannot forge
because the ingress overwrites it. The header must be one the trusted edge
sets or overwrites on every request; the listener is only reachable through
that edge. If a multi-valued header such as `X-Forwarded-For` is ever
configured, the last element is used, the one a proxy that appends would have
added, never the first, which the client controls. An inbound
`X-Forwarded-For` is otherwise never read: any client can send one, and the
value ends up in `usage.client_ip` and in what the game backends rate-limit
on. A value that is not an IP literal, a zoned IPv6 literal included, falls
back to the connection's peer address, and so does an empty
`client_ip_header`, which trusts nothing but the peer. Whatever address is
resolved is the `X-Forwarded-For` the gateway sends upstream, and the
configured header itself is stripped before forwarding.

Requests are throttled per address (`per_ip_requests_per_sec`, `per_ip_burst`)
before any key is read, so a stream of forged keys cannot turn into database
lookups. The per-key limit applies after the key resolves. Usage rows are
buffered and dropped rather than blocking a request when the buffer is full;
the drop count is logged each flush interval.

`shutdown_grace_seconds` is how long a shutdown waits for in-flight requests
after SIGTERM, and any request still running when it expires is logged and
cut off. A download may run for `upstream_timeout_seconds`, so with the
defaults, a 60s grace against a 300s timeout, a slow download started just
before a deploy is dropped and the customer has to retry. An operator who
needs 300s downloads to survive a deploy has to raise the platform's own
termination grace to cover them (DigitalOcean App Platform allows up to
120s, which still leaves a gap) or lower `upstream_timeout_seconds` to fit
inside the grace. The defaults keep long downloads working outside deploys
and accept the loss during one.

`apiaccess_config` and `observability_config` are Postgres connections:
`host`, `port`, `user`, `password`, `dbname`, `sslmode`, plus optional pool
tuning (`readonly`, `max_open_conns`, `max_idle_conns`,
`conn_max_lifetime_seconds`). `games` maps a game name to
`{"upstream": "https://...", "secret": "..."}`; the secret must match the
value under that game's `api_user_secrets["gateway@mtgban.com"]`. The
gateway creates `magic_links`, `trials`, `handoff_nonces`, and
`admin_actions` at startup through `ensureSchema`, so the database role
needs `CREATE` on the schema on the first boot after this deploy (the
`apiaccess_app` role already has it).

`public_url` is where customers land after Stripe Checkout:
`stripe.success_path` and `stripe.cancel_path` are joined onto it.
`stripe.grace_days` is how long a `past_due` subscription keeps its access
past the end of the period it failed to pay for; `0` ends access at the
period end. Stripe's own dunning emails and Smart Retries cover the
customer-facing reminders during that window.

## Environment variables

- `BAN_CONFIG_PATH`, `BAN_CONFIG_KEY`, `BAN_CONFIG_SECRET`: config location
  and B2 credentials when `-config` is not given.
- `APIACCESS_TEST_DSN`: Postgres DSN for the `apiaccess` and `billing`
  packages' DB tests; tests skip when it is unset. These packages share one
  scratch database, so run `go test -p 1 ./...` when the DSN is set. CI sets
  it against a `postgres:16` service and runs those tests on every push and
  pull request.
- `STRIPE_SECRET_KEY`: enables billing. `serve` mounts `/stripe/webhook` and
  runs the nightly reconcile only when it is set; the billing subcommands
  require it. Never put it in the config file.
- `STRIPE_WEBHOOK_SECRET`: the signing secret of the dashboard webhook
  endpoint. Required by `serve` when `STRIPE_SECRET_KEY` is set.
- `STRIPE_TEST_KEY`: a `sk_test_` key for the `billing` package's Stripe
  tests; tests skip when it is unset.
- `GATEWAY_SESSION_SECRET`: turns the customer pages on; at least 32
  characters. Signs the session, pending-checkout, and CSRF tokens.
- `MAIL_SMTP_HOST`, `MAIL_SMTP_PORT` (587), `MAIL_SMTP_USER`,
  `MAIL_SMTP_PASS`: STARTTLS SMTP for sign-in links and notices. With no
  host, mail is written to the log instead, sign-in links included, which
  is only acceptable while no customer can reach the host.

## Running locally

The gateway needs a running game backend to proxy to. Against a local
website checkout:

1. Add `"gateway@mtgban.com": "<a secret>"` to that game's
   `api_user_secrets` and start the website with `-sig`.
2. Write a local `config.json` (gitignored) with `gateway_email`,
   `apiaccess_config` pointing at a scratch Postgres database, one entry
   under `games` whose `upstream` is `http://localhost:<website port>`
   and whose `secret` matches the one added above, and `link` set to that
   same `http://localhost:<website port>` (the website substitutes
   localhost for its own link when not serving from an mtgban.com host, so
   the gateway must mint signatures against the same link).
3. Create an account, grant it access, and mint a key:

   ```bash
   go run . account add -config config.json -email smoke@example.com
   go run . grant add -config config.json -email smoke@example.com \
     -games magic -stores ALL_ACCESS -modes retail,buylist,sealed
   go run . key create -config config.json -email smoke@example.com
   ```

4. Run the gateway and call it:

   ```bash
   go run . serve -config config.json &
   curl -s -H "Authorization: Bearer <key>" localhost:8080/v1/magic/mtgban/stores.json
   ```

## Docker

```bash
docker build -t api-gatewahy:dev .
docker run --rm api-gatewahy:dev version
```

`CMD` is `serve`; pass `-config` or set `BAN_CONFIG_PATH` at runtime.

## Deploying

The DigitalOcean App Platform app builds this repo's `Dockerfile` from
`master`, region SF3, HTTP port 8080, health check `/healthz`, domain
`api.mtgban.com`, with `BAN_CONFIG_PATH`, `BAN_CONFIG_KEY`, and
`BAN_CONFIG_SECRET` set as encrypted env vars pointing at the production
config. Pushing a tag `vX.Y.Z` (or running the workflow manually) triggers
`.github/workflows/deploy.yml`, which installs `doctl` and runs
`doctl apps create-deployment` against the app recorded in the
`DO_APIGATEWAY_APP_ID` repo secret.

The website module is pinned by commit. The site tags the commits it
wants the gateway to build against as `apisig-vX.Y.Z`; Go cannot use those
tags as versions (the site is on major version 13 with no `/v13` module
path), so `go get github.com/mtgban/mtgban-website@apisig-vX.Y.Z` resolves
the tag to the commit and records its pseudo-version in `go.mod`. The pin
is currently `apisig-v0.0.1` (b4be4eed). Bump it the same way when the
site changes `apisig`, `apiproductlist`, or `apihandoff`.

### Backend contract

A game backend answers a bad gateway signature with HTTP 200 and a JSON body
of `{"error": "invalid signature"}` or
`{"error": "invalid or expired signature"}`, so the gateway sniffs the first
bytes of a 200 JSON response for `"error": "invalid` and turns that case into
a 502 rather than passing a success status with an error body to the
customer. The intended follow-up is a website-side change that returns a
status code for a refused signature, after which the gateway can drop the
sniffer and read the status.

## Stripe

Billing is on when `STRIPE_SECRET_KEY` and `STRIPE_WEBHOOK_SECRET` are set.
`serve` then mounts `POST /stripe/webhook`, serves plain confirmation pages
at `stripe.success_path` and `stripe.cancel_path`, and runs
`stripe reconcile` for every subscription at 03:00 UTC, posting the summary
to Discord.

The webhook verifies the `Stripe-Signature` header, records the event id in
`stripe_events` so a redelivery is a no-op, finds the subscription the event
is about, and rebuilds that subscription's entitlement row from Stripe's
current state. It never reads plan data from the event body, so it accepts
events of any API version. A failed reconcile answers 500 and drops the
event's ledger row, and Stripe retries.

Dashboard setup, once per mode (test, then live):

1. `api-gatewahy catalog seed` to create the products and prices.
2. Developers, Webhooks: add an endpoint for
   `https://api.mtgban.com/stripe/webhook` subscribed to
   `checkout.session.completed`, `customer.subscription.created`,
   `customer.subscription.updated`, `customer.subscription.deleted`,
   `invoice.paid`, and `invoice.payment_failed`. Copy its signing secret
   into `STRIPE_WEBHOOK_SECRET`.
3. Settings, Billing, Customer portal: allow payment method updates, invoice
   history, and cancellation at period end. Do not allow plan switching; the
   plan lives in subscription metadata that the portal would not update.
   Plan changes go through `plan change`.
4. Settings, Billing, Subscriptions and emails: turn on Smart Retries and the
   failed-payment emails, which are the customer-facing reminders during the
   `stripe.grace_days` window.

Every entitlement Stripe writes has `source = stripe` and `external_ref` set
to the subscription id; `grant list` shows them beside manual grants.

## Onboarding a game

For each game the gateway forwards to, an operator adds
`"gateway@mtgban.com": "<secret>"` to that game's `api_user_secrets` and
puts the matching upstream URL and secret under `games` in the gateway's
config. The gateway mints its own signature per request; it never reuses
a customer key against a game's `api_user_secrets` directly.
