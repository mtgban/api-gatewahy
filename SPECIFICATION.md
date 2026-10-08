# api-gatewahy — Architecture & Development Specification

> Written from a full read of every package, tests included, at master
> `ff61598` (2026-10-01). It covers how the gateway is built, how a request,
> a payment and a sign-in flow through it, and how it is developed and
> deployed. README.md is the operator's and customer's document (API,
> subcommands, config keys, Stripe dashboard setup); this file is the
> contributor's, and does not repeat README's tables. Function names are
> given rather than line numbers, which drift: `grep -n` for them.

## 1. Overview

One Go binary (`go 1.26.0`, `toolchain go1.26.9`, module
`github.com/mtgban/api-gatewahy`) that sells and serves API access to the
per-game MTGBAN price backends:

- **Gateway** (`/v1/{game}/...` and `/v2/{game}/...`): authenticates a
  customer's bearer key, checks the account's entitlements for that game
  and mode, mints a short-lived backend signature, and reverse-proxies to
  the game's own `/api/mtgban/...` (v1) or `/api/v2/...` (v2) host.
- **Billing**: Stripe Checkout, webhooks and a nightly reconcile write the
  entitlements a paid plan buys.
- **Portal**: server-rendered customer pages (magic-link sign-in, keys,
  usage, plan change, Patreon trial) and an admin area, on the same host.
- **Operator CLI**: subcommands for accounts, keys, grants, usage, the
  Stripe catalog, checkout links, invites, reconcile and plan changes.

~8,000 lines of production Go and ~8,300 of tests across a root
`package main` and eight packages. Postgres is the only datastore; there is
no Redis, no queue, and no state outside Postgres except per-process caches
and rate limiters.

### 1.1 Package map and dependency direction

```
main (main.go, serve.go, admin.go, billing_cmd.go)
 ├── config      JSON config: load (file or b2://), defaults, validation
 ├── gateway     /v1 and /v2 handler, key resolver cache, usage meter, prober
 │    └── apiaccess
 ├── billing     catalog plans, store families, Stripe, checkout, webhook, reconcile
 │    └── apiaccess
 ├── portal      customer + admin pages
 │    ├── apiaccess, billing, session, mailer
 │    └── gateway  (ClientIP only)
 ├── session     signed cookies, CSRF tokens
 ├── mailer      SMTP (STARTTLS) or log
 ├── discord     webhook poster
 └── apiaccess   Postgres store: schema, accounts, keys, entitlements, usage,
                 trials, invites, magic links, nonces, Stripe event ledger,
                 audit log, LISTEN/NOTIFY
```

`apiaccess` imports no sibling package; `gateway` never imports `billing`,
`portal` or `config`. Only `main` knows about config and wires everything.

### 1.2 What comes from mtgban-website

`github.com/mtgban/mtgban-website` is pinned by commit (always a commit on
the website's `master`, because the website rebase-merges and PR SHAs do not
survive). The gateway imports:

| Package | Used for |
|---|---|
| `apisig` | `Mint` the backend signature (gateway handler and prober) |
| `apihandoff` | Verify the Patreon handoff tokens game sites mint (`/trial`, `/session`) |
| `apiproductlist` | The embedded price list (`products.json`): packages, add-ons, intervals |
| `ratelimit` | Per-IP and per-account token buckets in the gateway |
| `observability` | Optional request event recorder |
| `timeseries` | `SQLConfig` and `OpenDB` for both Postgres pools |

A change to any of these is a change to the gateway. The website's
`docs/api-gateway-dependency.md` says which side deploys first. For work
spanning both repos, `go work init . ../mtgban-website` (`go.work` is
gitignored) and re-pin once the website PR merges.

## 2. Process lifecycle

### 2.1 Command dispatch (`main.go`)

`commands` is a map filled by `init()` in the file that owns each command
(`serve.go`, `admin.go`, `billing_cmd.go`, `main.go` for `version`). `run`
installs a `signal.NotifyContext` for SIGINT/SIGTERM and calls the command
with `(ctx, args, stdout, stderr)`. Exit codes everywhere: 0 ok, 1 error,
2 usage. `version` is set by `-ldflags "-X main.version=..."` in the
Dockerfile.

Admin and billing commands go through `withStore` (`admin.go`): it pulls
`-config` out of the arguments wherever it appears (`extractConfigFlag`),
loads the config, opens `apiaccess.Client`, and runs the verb. Each command
family then parses **one shared `flag.FlagSet` holding every flag of every
verb** and dispatches on `cmd + " " + verb` in a single switch (`runAdmin`,
`runBilling`). Flags a verb does not use are accepted silently, and a flag's
default applies to every verb (see todo/refactor.md).

### 2.2 `serve` (`serve.go`)

1. Load config; open `apiaccess.Client` (pings, then runs pending migrations).
2. Optional observability recorder (`observability_config`); failure to
   open it only logs.
3. Feature switches, all read from the environment:
   - `STRIPE_SECRET_KEY` set → billing on; `STRIPE_WEBHOOK_SECRET` becomes
     required (`stripeDepsFromEnv`).
   - `GATEWAY_SESSION_SECRET` set (≥32 chars) → portal on; SMTP from
     `MAIL_SMTP_*`, or `mailer.Log` to stderr when no host is set, which
     prints sign-in links to the log (`portalDepsFromEnv`).
4. `newServer` builds, in order: the per-game `gateway.Upstream`s, the
   `Resolver` (TTL + stale grace), the `UsageMeter` (5 s / 200 rows), the
   gateway `Handler`, the LISTEN/NOTIFY listener, the background jobs, then
   billing and portal objects when enabled, then the mux.
5. `serveUntilDone` serves until the context ends, then `Shutdown` with
   `shutdown_grace_seconds`; a request still running at the deadline is cut
   off and logged.
6. Deferred cleanup (LIFO): stop jobs and wait for them, close the listener,
   close the meter (final flush), close the observability recorder and its
   pool, close the store. The meter flushes while the store is still open.

HTTP server timeouts: ReadHeader 10 s, Read 30 s, Write
`upstream_timeout_seconds` + 30 s, Idle 120 s.

### 2.3 Background jobs

All run under one `jobsCtx` and a `sync.WaitGroup`. The app runs a single
instance, so each job runs once:

| Job | When | What |
|---|---|---|
| Prober | at start, then hourly | Mints a BASE_ACCESS retail signature per game, GETs `/api/mtgban/stores.json` and `/api/v2/stores.json`, each of which must answer its version's stores list (an array on v1, an object with `sellers` and `vendors` on v2; a game fails on either, its error naming the version), alerts Discord on a state change (failing ↔ recovered) |
| Daily summary | 00:05 UTC | Yesterday's usage by account and game, keys created, rows dropped by the meter → Discord; then `PruneUsage` older than `usage_retention_days`, `PruneStripeEvents` and `PruneInvites` (30 days, constants in `serve.go`), `PruneAdminActions` older than `admin_actions_retention_days` (skipped when 0) |
| Stripe reconcile | 03:00 UTC (billing on) | `Reconciler.All`, summary → Discord |
| Trial reminders | 09:00 UTC (portal on) | Mails trials ending within 3 days, marks each reminded |

`runDaily(ctx, hour, minute, fn)` sleeps to the next hh:mm UTC. Jobs do not
recover panics, and nothing would stop a second instance running the same
job.

### 2.4 Mux (`newMux`)

| Path | Handler |
|---|---|
| `/healthz` | 200 when `PingContext` succeeds within 2 s and ≥1 game is configured, else 503 |
| `/v1/games.json`, `/v2/games.json` | configured game names, GET only |
| `/stripe/webhook` | `billing.Webhook` (billing on) |
| portal routes | `portal.Server.Register` (portal on); else plain-text success/cancel pages when billing is on |
| `/v1/`, `/v2/` | `gateway.Handler` |
| `/` | JSON 404 |

The whole mux is wrapped in `recoverPanics`: a panic becomes a JSON 500 if
nothing was written yet; `http.ErrAbortHandler` is re-panicked so the
connection is torn down and the meter's deferred record still runs.

## 3. Configuration (`config/`)

`config.Load(ctx, path)` reads `-config`, else `$BAN_CONFIG_PATH`, through
`simplecloud.Open` (a local path or `b2://`, with
`BAN_CONFIG_KEY`/`BAN_CONFIG_SECRET`). `Parse` decodes, applies defaults,
then `Validate` returns the first error (games in name order).

- Defaults are applied when a value is zero or negative, so most knobs
  cannot be set to 0. Two keys are presence-checked by a second decode:
  `client_ip_header` (explicit `""` means trust only the peer address) and
  `stripe.grace_days` (explicit `0` means no grace).
- Validation: `gateway_email` and `apiaccess_config` required; `public_url`
  and `pricing_url` absolute; game names `^[a-z0-9]+$` (the router's
  pattern), each with a secret and an absolute upstream; Stripe landing
  paths start with `/`, differ, contain no braces and avoid `/`,
  `/healthz`, `/stripe/webhook`, `/v1/` and `/v2/`. `serve` additionally checks
  them against portal routes (`checkReservedPaths` → `portal.Reserved`).
- Unknown keys are ignored (no `DisallowUnknownFields`), so a misspelt key
  silently takes its default.
- Secrets never live in the config file except per-game `secret`s and the
  database passwords; Stripe, session and SMTP secrets are environment only.

The full key list with defaults is README's Configuration table.

## 4. Data model (`apiaccess/`)

### 4.1 Access and migrations

- `database/sql` with `github.com/lib/pq`. `NewClient(ctx, apiaccess.SQLConfig)`
  opens a pool (25 open by default), pings, and runs `migrate`.
  `SQLConfig.DSN()` single-quotes every value, so an empty password or one
  with a space or a quote works.
- `migrate` applies the append-only `migrations` list (`migrations.go`):
  in one transaction it sets `lock_timeout = '5s'`, takes
  `pg_advisory_xact_lock`, creates `schema_migrations` if missing, and runs
  each migration whose version is not recorded there, then records it. Any
  error, a lock timeout included, rolls the whole boot back and names the
  migration. Migration 1 is the pre-migration schema in its `IF NOT EXISTS`
  form, so a database built before versioning just records it. A schema
  change is a new migration; shipped ones are never edited. The role needs
  `CREATE` only while `schema_migrations` is missing or a migration is
  pending.
- Every method takes `ctx` first. Two transactions exist: `CreateTrial`
  (advisory lock on `hashtext('trial:'||email)`, then a conditional insert)
  and `InsertUsage` (a COPY). Everything else is one atomic statement.
- Single-use tokens (invites, magic links, handoff nonces, Stripe events)
  are claimed by one conditional write, never SELECT-then-UPDATE.
- `ErrNotFound` is the lookup-miss sentinel; domain errors
  (`ErrInviteInvalid`, `ErrTrialTooSoon`, `ErrNonceUsed`) where callers
  branch on them. Errors are wrapped with an `apiaccess:` prefix.

### 4.2 Tables

| Table | Key columns | Notes |
|---|---|---|
| `accounts` | `id`, `email` UNIQUE, `status` (active/suspended), `note`, `stripe_customer_id` UNIQUE, `session_epoch` | Emails stored through `NormalizeEmail` (lowercase, trimmed) |
| `api_keys` | `id`, `account_id`, `key_hash` UNIQUE, `prefix`, `label`, `kind`, `last_used_at`, `revoked_at` | Partial UNIQUE on `prefix` where not revoked |
| `entitlements` | `id`, `account_id`, `source` (manual/stripe/trial), `games[]`, `store_scope`, `modes[]`, `addons[]`, `status` (active/ended), `valid_from`, `valid_until`, `external_ref` | Partial UNIQUE on `external_ref` = the Stripe subscription id |
| `usage` | `ts`, `key_id`, `account_id`, `game`, `path`, `status`, `bytes`, `duration_ms`, `client_ip` | No PK/FK; indexes on `ts`, `(account_id, ts)`, `(key_id, ts)`; pruned daily |
| `invites` | `token_hash`, `interval_key`, `email` ('' = unbound), `expires_at`, `used_at` | Unlock a non-public interval for one checkout |
| `stripe_events` | `id`, `type`, `received_at`, `processed_at` | Webhook idempotency ledger |
| `magic_links` | `token_hash`, `account_id`, `expires_at`, `used_at` | 15-minute sign-in links |
| `trials` | `patreon_email`, `account_id`, `granted_at`, `ends_at`, `reminder_sent_at` | One trial per email per 180 days |
| `handoff_nonces` | `nonce`, `expires_at` | Burns a handoff token on accept |
| `admin_actions` | `at`, `actor`, `action`, `account_id`, `target`, `detail` | Audit log; actor is the admin's email or `cli:<user>` |
| `schema_migrations` | `version`, `applied_at` | The migrations that have run |

Migration 2 CHECKs account status, entitlement status and source, key
kind and normalized emails against the typed Go constants; modes are
enforced in Go only. The daily job prunes `stripe_events` and expired
`invites` after 30 days and `admin_actions` after
`admin_actions_retention_days` (`0` keeps them forever); `magic_links` and
`handoff_nonces` are swept on insert.

### 4.3 Keys

`<kind>_<32 × [a-z0-9]>` from `crypto/rand`; kind is `ban_live` (account had
an active Stripe plan when minted) or `ban_demo` (trial or manual), and
`mtgban_live_` keys from before still validate (`LooksLikeKey`). Only
`HashKey` (unsalted SHA-256 hex) is stored, and the same hash is used for
invite and magic-link tokens. The prefix shown to operators is the first 8
random characters. Kind is informational: the gateway does not branch on it.
`CreateKey` retries a prefix collision up to 4 times.

### 4.4 Entitlements

An account's access is the union of its active rows (`ActiveAt(now)`:
status active and `valid_from ≤ now < valid_until`). Store scope is
`ALL_ACCESS`, `BASE_ACCESS`, or a sorted comma list of backend shorthands
typed exactly as the backend spells them; `DEV_ACCESS` is refused.
`canonicalStoreScope` checks syntax only, not existence: a typo grants
nothing rather than failing. Modes are a subset of `retail, buylist,
sealed` in that order. Every write goes through `prepare`
(`AddEntitlement`, `UpsertStripeEntitlement`), which canonicalizes scope and
modes. `UpsertStripeEntitlement` keys on `external_ref`, keeps the first
`valid_from`, and overwrites everything else.

### 4.5 Cache invalidation (`notify.go`)

`Notify(payload)` is `pg_notify('apiaccess_reload', payload)`: a key hash
drops that key, `""` drops every cached key. `Listen` wraps `pq.Listener`
(10 s → 1 m reconnect, 90 s idle ping); a reconnect is treated as "drop
everything". Senders: CLI mutations, portal key revoke and admin actions,
trial grant, reconcile. Entitlement *expiry* needs no notify, because the
gateway re-checks `ActiveAt(now)` on cached rows every request; a status
flip or a new row does.

## 5. Gateway request path (`gateway/`)

### 5.1 Files

| File | Responsibility |
|---|---|
| `route.go` | `ParseRoute`: only `/v1/{game}/{sub}` (to `/api/mtgban/{sub}`) and `/v2/{game}/{sub}` (to `/api/v2/{sub}`); kinds search, meta (sets/stores, and finishes in v2), retail/buylist/all/sealed; `Rest` is the backend path under `/api/` (`mtgban/...` or `v2/...`); rejects `..`; `NeedsModes` |
| `access.go` | `Resolve(ents, game, now) (Access, bool)`: union of active rows naming the game; ALL > BASE > explicit union |
| `resolver.go` | `Resolver`: key hash → `apiaccess.Lookup` cache with TTL, negative caching, stale-on-error, 10,000-entry bound plus 1,000 for unknown keys |
| `handler.go` | `Handler.ServeHTTP`, one `httputil.ReverseProxy` + `http.Transport` per game, `ClientIP`, `statusWriter` |
| `meter.go` | `UsageMeter`: non-blocking `Record`, batched COPY every 5 s or 200 rows, 3 attempts, then drop and count |
| `probe.go` | `Prober`: hourly signature health check per game |
| `errors.go` | `writeError` JSON `{"error", "game"}` |
| `summary.go` | `SummaryText` for the daily Discord post |

### 5.2 One request

1. Non-GET → 405. Bad route → 404. Unknown game → 404.
2. `ClientIP`: the last comma element of the configured header if it is an
   IP literal without a zone, else the peer. Per-IP limiter **before** any
   key is read → 429 with `Retry-After: 1`.
3. Key from `Authorization: Bearer …`, else `?key=`. Missing → 401;
   `!LooksLikeKey` → 401.
4. `Resolver.Resolve(HashKey(key))`: unknown → 401; store error with
   nothing usable cached → 503 `Retry-After: 30`.
5. Revoked key or non-active account → 401.
6. `Resolve(entitlements, game, now)`: no row for the game → 403; empty or
   `DEV_ACCESS` scope → 403; each mode the route needs and the plan lacks →
   403 naming it.
7. Per-account limiter (`account:<id>`) → 429; sets `RateLimit-Limit`.
8. `apisig.Mint(game secret, link, {API: scope, mode: modes, email:
   gateway_email}, now + 5 m)`.
9. Proxy with `upstream_timeout_seconds` covering the whole exchange.
   `Rewrite` sets the backend path, replaces `sig`, drops `key`, strips
   Cookie/Authorization/forwarding headers, sets `X-Forwarded-For` to the
   resolved IP. `ModifyResponse` passes 2xx, 304 and 429; any other status
   is an error; a 200 JSON body's first 300 bytes are sniffed for the
   backend's `"error": "invalid…` signature refusal (the backend answers a
   bad signature with 200; see README "Backend contract").
   `ErrorHandler`: signature refused or upstream status → 502, timeout →
   504, anything else → 502. It logs the error's type only, never the
   error, because a transport error can carry the minted signature.
10. The deferred `meter.Record` writes one usage row (status 0 → 499).
    Requests refused in steps 1–7 are not metered.

### 5.3 Caching and failure behaviour

- A cached lookup is fresh for `cache_ttl_seconds` (60). On a store error an
  entry younger than TTL + `stale_grace_seconds` (600) is served, so a
  revoked key can work for at most 11 minutes during a database outage.
  Unknown keys are cached negatively in a separate 1,000-entry map, so a
  flood of forged keys evicts only other unknown keys, never a real one.
- The resolver releases its mutex during the DB call; concurrent misses for
  one hash in one invalidation generation share a single query, and a
  result an `Invalidate` overtook is not stored.
- The DB call runs detached from the request, bounded by the 5 s lookup timeout.

## 6. Billing (`billing/`)

### 6.1 Catalog and plans

The catalog is `apiproductlist.MustLoad()` from the website module:
packages `starter` (explicit stores, one included), `all_stores`
(BASE_ACCESS), `all_data` (ALL_ACCESS); add-ons `extra_store` (starter only)
and `extra_game`; intervals `monthly` (public) and `quarterly` (invite
only). Amounts are monthly cents; an interval's amount is monthly × months.
Stripe lookup keys are `<item>_<interval>`.

`Plan{Package, Interval, Games, Stores}`:
- `Normalize` → `Validate` (games configured; non-public interval needs an
  invite) → `Resolve` (store families → `ResolvedPlan{Scope, Modes, …}`).
- `LineItems`/`Total` price it; `Metadata`/`PlanFromMetadata` round-trip it
  through the Stripe subscription's metadata, **which is the source of
  truth** for what a subscription grants.
- `ValidationError` carries customer-safe text (`IsValidation`); sentinels
  `ErrInviteRequired`, `ErrPriceNotSeeded`, `ErrStoresUnavailable`,
  `ErrNoCustomer`, `ErrNoAccount`, `ErrManySubscriptions`.

### 6.2 Store families (`stores.go`)

Each game site publishes `GET <upstream origin>/api-plans/stores.json`:
`{game, implied[], stores[{key, name, shorthands[]}]}`. `SiteStoreClient`
caches each game's list for 5 minutes (5 s fetch timeout, one fetch per
burst, last good list served after a failure with a 30 s backoff, one
Discord alert once a list is an hour stale). `Plan.Resolve` turns picked
family keys into the union of backend shorthands across the plan's games,
plus every implied family; a key no site offers is a `ValidationError`.

### 6.3 Stripe access (`stripe.go`)

`stripe-go/v84` through the `billing.API` interface (13 methods); `*Client`
is the real one, `fakeAPI` (`fake_test.go`) the test double with per-method
failure injection and call counters. Tests that hit real Stripe need
`STRIPE_TEST_KEY=sk_test_…` and skip otherwise.

### 6.4 Flows

- **Checkout** (`Checkout.Create`): account active → validate → resolve →
  consume invite for a non-public interval (released on any later error) →
  price ids by lookup key → ensure Stripe customer → Checkout Session in
  subscription mode with the plan as subscription metadata. `Abandon`
  expires the session and releases the invite only once Stripe confirms.
- **Webhook** (`Webhook.ServeHTTP`): POST, 1 MB cap, signature verified
  (API version mismatch ignored), six handled event types; others get 200.
  `BeginStripeEvent` claims the event id (reclaiming an unfinished claim
  after 5 minutes), the subscription id is pulled from whichever object the
  event carries, and `Reconcile(subID)` rebuilds the row from Stripe's
  current state. Success → `FinishStripeEvent`, 200. Failure →
  `DeleteStripeEvent`, 500, Stripe retries. Event bodies are never trusted
  for plan data, so delivery order does not matter.
- **Reconcile** (`Reconciler.apply`): plan from metadata → account (metadata
  `account_id`, then Stripe customer) → status map (active/trialing →
  open-ended; past_due → until period end + `stripe.grace_days`; anything
  else → ended now) → resolve stores → `UpsertStripeEntitlement` →
  `Notify("")`. An item/metadata mismatch alerts but metadata wins. `All`
  applies every listed subscription and re-fetches every active `stripe`
  row Stripe no longer lists.
- **Plan change** (`ChangePlan`): ownership check, interval pinned to the
  current one, validate and resolve, diff items, `UpdateSubscription` with
  `create_prorations`, reconcile.
- **Seed** (`Seed`): fixed product ids `mtgban_api_<key>`; prices matched by
  lookup key; a changed amount creates a replacement price, transfers the
  lookup key and archives the old one. Idempotent.

### 6.5 Who calls what

| Flow | Portal | CLI | serve |
|---|---|---|---|
| Checkout | `POST /checkout` | `checkout link` | — |
| Abandon | cancel page | — | — |
| Plan change | `POST /account/plan` | `plan change` | — |
| Reconcile one | after plan change | `stripe reconcile -sub` | webhook |
| Reconcile all | `POST /admin/reconcile` | `stripe reconcile` | 03:00 job |
| Seed | — | `catalog seed` | — |

## 7. Portal (`portal/`)

### 7.1 Routing and guards

Go 1.22 method patterns on `http.ServeMux`. `Server.Register` calls one
`registerX` per file (login, checkout, account, trial, admin) plus `/{$}`
(redirect to `pricing_url`) and `/static/portal.css`. `Reserved` is a
hand-kept mirror of those routes, used to refuse colliding Stripe landing
paths.

There is no middleware chain; each route is wrapped explicitly:

| Guard | Does | Failure |
|---|---|---|
| `withSession` | Reads `ban_session`, loads the account, requires `sess.Epoch == account.SessionEpoch` and status active; on POST checks the `csrf` form value | GET → 302 `/login`; POST → 401; suspended → 403; bad CSRF → 403 |
| `withAdmin` | `withSession` + email in `admin_emails` | 404 |
| `sameOrigin` (called inline) | `Sec-Fetch-Site: same-origin`, else `Origin`, else `Referer` matches `public_url` | 403 |

Token-consuming POSTs without a session (`/login`, `/login/{token}`,
`/trial`, `/session`) call `sameOrigin` first. Every state-changing route is
a POST; GETs change nothing local (`GET /portal` creates a Stripe portal
session).

### 7.2 Sessions and CSRF (`session/`)

Stateless: `base64url(url.Values) + "." + base64url(HMAC-SHA256)` with the
session secret; `_exp` inside. Session fields: account id, email, issued-at,
epoch. Logout bumps `accounts.session_epoch`, invalidating every cookie
issued before; suspending an account ends sessions on the next request;
rotating `GATEWAY_SESSION_SECRET` signs everyone out. `ban_pending` (1 h)
carries an in-flight checkout (plan, return_to, invite, `cs`). CSRF token =
HMAC(`csrf|<account id>|<issued-at>`), so it needs no storage. Cookies are
host-only, `Path=/`, HttpOnly, SameSite=Lax, Secure when `public_url` is
https.

### 7.3 Pages

- **Sign-in**: `POST /login` → rate limit per IP and per email
  (`login_links_per_hour`) → `GetOrCreateAccount` → 15-minute magic link by
  mail (deleted if the mail fails). `GET /login/{token}` only shows a
  confirm button (mail scanners prefetch links); `POST /login/{token}`
  consumes it and issues the session.
- **Checkout**: `GET /checkout` takes the plan from the game site's
  configurator query or the pending cookie, validates and resolves it, then
  shows sign-in or the confirm page. `POST /checkout` refuses an account
  that already has an active Stripe plan, creates the session, and
  redirects to Stripe.
- **Account**: entitlements, keys (label required, ≤5 unrevoked, 10 mints
  per hour per account), this month's usage, plan change, Stripe portal.
  A new key's plaintext is rendered once, never redirected.
- **Trial / session handoff**: game sites mint an `apihandoff` token signed
  with that game's `secret`; GET shows a confirm page, POST burns the nonce.
  `/trial` grants `trial_days` of ALL_ACCESS on every game once per Patreon
  email per 180 days (`CreateTrial`, then `AddEntitlement`, compensating
  with `DeleteTrial`).
- **Admin**: account list/search, demo access, recent actions, per-account
  page (status, note, keys, entitlements, manual grant, invites, activity,
  per-key usage), usage page, "reconcile now". Every mutation calls
  `audit` then `notify`.

### 7.4 Rendering

`//go:embed templates/*.html static/*.css`, parsed once into one
`html/template` set. No layout: each page includes `header` and `footer`.
Every page gets `page{Title, Session, CSRF, Error, Notice, …, Data}`, with
one `xxxData` payload type per template. `render` buffers, sets the
security headers (X-Frame-Options, CSP frame-ancestors, Referrer-Policy,
Cache-Control no-store) and turns template errors into a 500. Mutations
redirect with `?notice=<key>` mapped through `notices`. Mail bodies are
built in Go (`mail.go`), not templates.

### 7.5 In-process limiters

`limiter.go`: per-key sliding-hour timestamps in one mutex map (`ip:`,
`email:`, `keys:<id>`, `keyattempts:<id>`). They reset on restart; with
one instance they are global.

## 8. Support packages

- **mailer**: `Mailer.Send(ctx, to, subject, text, html)`. `SMTP` dials
  plain TCP and requires STARTTLS (no implicit TLS on 465); PLAIN auth when
  a user is set; multipart/alternative, quoted-printable. `Log` writes the
  mail to a writer (production fallback and test capture).
- **discord**: `Poster.Post(ctx, msg)` with `allowed_mentions` off, 10 s
  timeout; errors are scrubbed so the hook URL never leaks; an empty hook
  only logs. No splitting at Discord's 2,000-character limit.

## 9. Development

### 9.1 Build and test

```bash
go build ./...
go vet ./...
gofmt -s -l .                                # must print nothing
go test ./...                                # DB and Stripe tests skip
APIACCESS_TEST_DSN='postgres://u:p@localhost:5432/scratch?sslmode=disable' \
  go test -race -p 1 ./...                   # what CI runs
go run github.com/mgechev/revive@v1.13.0 -set_exit_status -config .revive.toml ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

- `APIACCESS_TEST_DSN` turns on the `apiaccess` and `billing` DB tests.
  They share tables, hence `-p 1`. The DSN needs a password (any, for a
  trust-auth local server): an empty one breaks `SQLConfig.DSN()`.
- `STRIPE_TEST_KEY=sk_test_…` turns on the real-Stripe tests.
- As measured at `ff61598`: 271 tests pass, 2 skip (Stripe), 80.0%
  statement coverage, 0 reachable vulnerabilities (govulncheck).

### 9.2 Test style

- Internal tests (`package x`), stdlib only, no assertion library; table
  tests over anonymous structs; status code + body substring assertions,
  no golden files; no `t.Parallel`.
- Fakes are hand-written per consumer: `portal/mem_test.go` (the 31-method
  `portal.Store`), `billing/store_test.go`, root `admin_test.go`,
  `gateway/resolver_test.go`, `billing/fake_test.go` (`fakeAPI`, `fakeStores`),
  `portal/fake_stripe_test.go`. They are kept in step with `apiaccess` by
  hand; several already diverge (todo/refactor.md).
- Consumers declare narrow interfaces next to their use and assert
  `var _ Store = (*apiaccess.Client)(nil)`.
- Clocks are injected as `Now func() time.Time` / `now` fields; the portal
  test server freezes time in three places.
- The gateway's `fakeBackend` verifies the minted signature with the real
  `apisig.Verify`.

### 9.3 CI and deploy

- `.github/workflows/ci.yml` on every PR and push to master: `build`
  (gofmt -s, vet, revive v1.13.0 with `.revive.toml`, staticcheck 2026.2.1,
  govulncheck, build, `go test -race -p 1` against `postgres:16`).
- `.github/dependabot.yml`: weekly gomod, GitHub Actions and Docker.
- Deploy: a `v*` tag (or a manual run) of `.github/workflows/deploy.yml`
  runs `doctl apps create-deployment` for the DigitalOcean App Platform app,
  which builds the `Dockerfile` (golang:1.26 → distroless static, nonroot)
  from master. The app spec (instances, health check, env) lives in
  DigitalOcean, not in this repo. A tag push does not wait for CI on that
  commit.

## 10. Design principles in the code

1. **Postgres is the only shared state.** Caches and limiters are per
   process and disposable; correctness never depends on them, only latency
   and the outage window does.
2. **Never trust a client or an event for what it grants.** Keys resolve
   against the store, Stripe events trigger a re-read of the subscription,
   handoff tokens are verified with the issuing game's secret.
3. **Secrets never reach logs or errors.** Plaintext keys and tokens are
   never stored; minted signatures, hook URLs and SMTP passwords are kept
   out of every log line (log the error type, not the error).
4. **Single-use means one conditional write.**
5. **Consumer-side interfaces, concrete wiring in `main`.**
6. **Fail closed at the edge, fail stale in the middle**: unknown or
   malformed input is refused before it reaches the database; a database
   outage serves the last known answer for a bounded time.
