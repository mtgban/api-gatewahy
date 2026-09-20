# api-gatewahy

The MTGBAN API gateway. It fronts the per-game BAN price APIs behind one
bearer key per customer, valid across every game the customer's plan
covers, instead of the per-game signed links the website mints today.
Requests carry the customer's key; the gateway resolves it against
entitlements in Postgres, mints a short-lived backend signature for the
target game, and reverse-proxies to that game's own API host.

Accounts, keys, and entitlements are managed by operators through this
binary's admin subcommands in phase 1. A later phase lets Stripe write
entitlements directly. The gateway also runs a background prober, a daily
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
| 502 | Upstream unreachable, returned a non-2xx, or is misconfigured. |
| 503 | Database unavailable and the key was not in cache. |
| 504 | Upstream exceeded the timeout. |

Meta endpoints, unauthenticated: `/healthz` (200 when the database pings
and at least one game is configured) and `/v1/games.json` (the configured
game names).

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
created, and handed back if that creation fails.

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
| `upstream_timeout_seconds` | `300` |
| `shutdown_grace_seconds` | `60` |
| `usage_retention_days` | `395` |
| `stripe.grace_days` | `10` |
| `stripe.success_path` | `/checkout/success` |
| `stripe.cancel_path` | `/checkout/cancel` |

`cache_ttl_seconds` is how long a resolved key stays cached, and
`stale_grace_seconds` is how much longer the gateway keeps serving that
cached answer while the database is failing; past the two summed, a lookup
returns 503 instead. A revoked key may therefore keep working for at most
`cache_ttl_seconds + stale_grace_seconds` during a database outage, 11
minutes on the defaults. LISTEN/NOTIFY drops the cached entry immediately
when the database is reachable.

`client_ip_header` names the one header the gateway trusts for the client
address. It defaults to `DO-Connecting-IP`, which DigitalOcean App Platform's
ingress sets to the real peer and which a client cannot forge because the
ingress overwrites it. An inbound `X-Forwarded-For` is never read: any client
can send one, and the value ends up in `usage.client_ip` and in what the game
backends rate-limit on. A value that is not an IP literal, a zoned IPv6
literal included, falls back to the connection's peer address, and so does an
empty `client_ip_header`, which trusts nothing but the peer. Whatever address
is resolved is the `X-Forwarded-For` the gateway sends upstream, and the
configured header itself is stripped before forwarding.

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
value under that game's `api_user_secrets["gateway@mtgban.com"]`.

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
