# AGENTS.md

Guidance for AI coding agents working in this repository. Humans may also
find it a useful quickstart. `SPECIFICATION.md` is the architecture: read it
for anything this file only summarizes. README.md is the operator's and
customer's document (public API, subcommands, config keys, Stripe setup):
change it whenever behaviour it describes changes.

## What this is

The MTGBAN API gateway: one Go binary that sells and serves access to the
per-game MTGBAN price backends. A customer's bearer key is resolved against
entitlements in Postgres; the gateway mints a short-lived backend signature
and reverse-proxies to that game's API. The same binary runs Stripe billing
(checkout, webhook, nightly reconcile), a server-rendered customer portal
with an admin area, and the operator CLI (`account`, `key`, `grant`,
`usage`, `catalog`, `checkout`, `invite`, `stripe`, `portal`, `plan`).

It imports six packages from `github.com/mtgban/mtgban-website` (`apisig`,
`apihandoff`, `apiproductlist`, `ratelimit`, `observability`, `timeseries`);
SPECIFICATION.md §1.2 says what each is for.

## Build, run, test

```bash
go build ./...
go vet ./...
gofmt -s -l .                # must print nothing; CI fails otherwise
go test ./...                # DB and Stripe tests skip

# What CI runs: the DB tests too, one package at a time (they share tables)
APIACCESS_TEST_DSN='postgres://u:p@localhost:5432/scratch?sslmode=disable' \
  go test -race -p 1 ./...

go run github.com/mgechev/revive@v1.13.0 -set_exit_status -config .revive.toml ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...

go run . serve -config config.json
```

- The DSN needs a password, any password for a trust-auth local server:
  `timeseries.SQLConfig.DSN()` is unquoted `key=value`, and an empty
  password swallows `dbname`, so tests connect to the wrong database.
- Use a scratch database. The tests delete rows from every apiaccess table.
- `STRIPE_TEST_KEY=sk_test_…` turns on the tests that call real Stripe.
- README "Running locally" walks through a full local setup against a
  website checkout.

This file does not track pass/fail status or coverage. Run the commands and
trust their output; todo/refactor.md records the last measurement.

## Configuration & secrets

- Config is JSON (`config/config.go`), read from `-config` or
  `BAN_CONFIG_PATH` (a path or `b2://`). `config.json` is gitignored: never
  commit one.
- Stripe, session and SMTP secrets come from the environment only
  (`STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `GATEWAY_SESSION_SECRET`,
  `MAIL_SMTP_*`). Do not add a config key for a secret.
- `STRIPE_SECRET_KEY` turns billing on and `GATEWAY_SESSION_SECRET` turns
  the portal on. Code that needs either must handle it being off.
- A new config key needs a default in `applyDefaults` or a check in
  `Validate`, a row in README's Configuration table, and a test in
  `config_test.go`. Config ignores unknown keys, so a typo in a key name
  fails silently.

## Repository conventions

- **Commits**: `area: imperative summary`, lowercase, where area is the
  package or the CLI command (`portal:`, `billing:`, `apiaccess:`,
  `gateway:`, `admin:`, `keys:`, `serve:`, `config:`, `docs:`, `deps:`).
  Keep the subject short and the body to a few lines: the why, and the
  number that proves it. Longer write-ups go in `docs/`, committed with the
  change, and the body points at them.
- **No `Co-Authored-By` trailer.**
- **Every PR targets `master`.** Deploys are tag-driven from master.
- **Comments say what the code is, not what it used to be.** History goes
  in the commit message. A comment inside a function is two or three
  lines. Every exported identifier has a doc comment, usually one line,
  starting with its name.
- Errors are wrapped with the package name: `fmt.Errorf("billing: …: %w",
  err)`. Lookup misses are `apiaccess.ErrNotFound`. Sentinels are checked
  with `errors.Is`.
- Consumers declare the narrow interface they need next to their use and
  assert `var _ Store = (*apiaccess.Client)(nil)`. Concrete wiring happens
  only in `package main`.
- Time is injected (`Now func() time.Time` fields, a `now()` fallback) so
  tests can freeze it. Don't call `time.Now()` in code a test drives.
- Plain stdlib tests: internal (`package x`), table-driven where it fits,
  no assertion library, no `t.Parallel`.
- **No `reflect`**, tests included, as in go-mtgban and mtgban-website:
  compare with `slices`/`maps` or a comparison written for the type. Six
  billing test files still import it until todo/refactor.md D7a lands the
  lint rule; don't add a seventh.
- Calls folded into an `if` (`if err := f(); err != nil`) are neither
  required nor banned here, unlike go-mtgban: follow the surrounding code.

## Where things live

| Path | Responsibility |
|---|---|
| `main.go` | Command registry and dispatch; signal context |
| `serve.go` | `serve`: wiring, background jobs, mux, panic recovery, graceful shutdown |
| `admin.go` | `account`, `key`, `grant`, `usage` subcommands |
| `billing_cmd.go` | `catalog`, `checkout`, `invite`, `stripe`, `portal`, `plan` subcommands |
| `config/` | Load (file or b2://), defaults, validation |
| `apiaccess/` | The Postgres store: `migrations.go` (append-only migrations), one file per table or aggregate, `notify.go` (cache reload over LISTEN/NOTIFY) |
| `gateway/` | `/v1` handler and reverse proxy, key resolver cache, usage meter, prober, daily summary text |
| `billing/` | Plans from the catalog, store families from the game sites, Stripe client, checkout, webhook, reconcile, plan change, seed |
| `portal/` | Customer and admin pages; `templates/` and `static/` are embedded |
| `session/` | Signed session and pending-checkout cookies, CSRF tokens |
| `mailer/`, `discord/` | SMTP or log mail; Discord webhook alerts |
| `todo/refactor.md` | The measured tech-debt list, open decisions, and the plan |

## Critical invariants — do not break these

1. **Secrets never reach a log line, an error or a response.** That covers
   plaintext keys, minted signatures, handoff and magic-link tokens, the
   Discord hook URL and SMTP credentials. Proxy errors are logged by type
   (`%T`) because a transport error can carry the signed URL. Follow the
   existing scrubbing in `gateway/probe.go` and `discord/discord.go`.
2. **Only hashes are stored.** Keys, invite tokens and magic-link tokens go
   through `apiaccess.HashKey`. A plaintext key is shown once and never
   redirected.
3. **Every entitlement write goes through `apiaccess`'s `prepare`**
   (`AddEntitlement`, `UpsertStripeEntitlement`), which canonicalizes the
   store scope and modes. Never insert or update entitlements with SQL from
   another package.
4. **Notify after changing what a key can do.** Revoking a key, suspending
   an account, adding or ending an entitlement: call `Notify(hash)` or
   `Notify("")` after the write, or gateways keep the old answer for
   `cache_ttl_seconds`. Expiry by time needs nothing, since the gateway
   re-checks `ActiveAt(now)` on every request.
5. **Billing trusts Stripe's current state, never the event body.** The
   webhook only learns which subscription changed, then reconcile re-reads
   it. A subscription's metadata (`Plan.Metadata`) is the source of truth
   for what it grants. Prices and packages come from `apiproductlist` in
   the website module; a price edit is a dependency bump plus
   `catalog seed`.
6. **Single-use means one conditional write.** Invites, magic links,
   handoff nonces and Stripe events are claimed with one
   `UPDATE … WHERE unused` or `INSERT … ON CONFLICT`. Never use
   SELECT-then-UPDATE.
7. **Schema changes are new migrations.** Append a numbered entry to
   `migrations` in `apiaccess/migrations.go`. Never edit or reorder one
   that has shipped: production has recorded it and will not rerun it.
8. **Every portal POST is guarded.** Use `withSession` (session + CSRF) or
   `withAdmin`, or call `sameOrigin` first for the sessionless
   token-consuming posts. A new route goes into `Reserved` too, and its
   form carries the hidden `csrf` input.
9. **Refuse at the edge before touching the database.** The gateway
   rate-limits per IP and checks `LooksLikeKey` before any lookup. Keep new
   checks in that order.
10. **The website module is pinned to a master commit.** The website
    rebase-merges, so a commit pinned from a PR branch never lands. While a
    change spans both repos, use `go work` (gitignored) and re-pin once the
    website PR merges. `apisig` and `apihandoff` bytes are frozen by golden
    tests on the website side, so never re-implement them here.
11. **Background work runs under `jobsCtx` in `newServer`**, is added to
    its `WaitGroup`, and stops on shutdown. The app runs a single
    instance, so jobs and the in-process limiters assume one process; a
    second instance would duplicate every daily job.

## Testing patterns

- `apiaccess` tests run against real Postgres via `testClient(t)`. Other
  packages use hand-written fakes: `portal/mem_test.go`,
  `billing/store_test.go`, `billing/fake_test.go`, root `admin_test.go`.
  **A change to an `apiaccess` method's behaviour must be mirrored in every
  fake that implements it.** The fakes already diverge in places
  (todo/refactor.md S3), so prove behaviour that depends on the store's
  rules in `apiaccess` tests, not through a fake.
- Portal tests: `newTestServer(t)` freezes time at 2026-09-20 12:00 UTC;
  `ts.do`, `ts.signIn`, `ts.handoff`, and `cookieFor` cover requests,
  sessions and handoffs. POSTs carry `Origin: https://api.test`.
- Gateway tests proxy to `fakeBackend`, which verifies the minted
  signature with the real `apisig.Verify`.
- A bug fix lands with a test that fails before it.

## Known issues / refactors pending

`todo/refactor.md` holds the measured, scored list, the decisions that block
some of it, and the phased checklist. This file does not copy it, since a
copy goes stale the moment an item lands. Tick its boxes in the PR that does
the work.

## Gotchas

- The 200-with-error sniff in `gateway/handler.go` exists because the
  backends answer a bad signature with HTTP 200 (README "Backend
  contract"). Don't remove it until the website returns a status.
- `stale_grace_seconds` means a revoked key can keep working for TTL +
  grace during a database outage. That is deliberate; don't shorten the
  path that serves stale entries without reading SPECIFICATION.md §5.3.
- `.dockerignore` excludes `docs/` and `*.md`, so nothing at runtime may
  read them.
- README is user-facing: the error table, routes, subcommands and config
  keys must match the code. Update it in the same PR.
- Mail without `MAIL_SMTP_HOST` goes to the log, sign-in links included.
  That is fine locally and never acceptable where customers can reach the
  host.
