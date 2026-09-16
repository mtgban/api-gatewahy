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
- `end -id`: ends an entitlement by id as of now.
- `list -email`: table of an account's entitlements.

`usage`: `-since -until -email`, dates as `YYYY-MM-DD`. Defaults to the
last 30 days, all accounts. Prints requests, bytes, and errors by account
and game.

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

## Environment variables

- `BAN_CONFIG_PATH`, `BAN_CONFIG_KEY`, `BAN_CONFIG_SECRET`: config location
  and B2 credentials when `-config` is not given.
- `APIACCESS_TEST_DSN`: Postgres DSN for the `apiaccess` package's DB
  tests; tests skip when it is unset. CI sets it against a `postgres:16`
  service and runs those tests on every push and pull request.

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

## Onboarding a game

For each game the gateway forwards to, an operator adds
`"gateway@mtgban.com": "<secret>"` to that game's `api_user_secrets` and
puts the matching upstream URL and secret under `games` in the gateway's
config. The gateway mints its own signature per request; it never reuses
a customer key against a game's `api_user_secrets` directly.
