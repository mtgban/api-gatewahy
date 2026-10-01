# Refactor plan: the measured list and the checklist

Measured 2026-10-01 against master `ff61598`, from a full read of every
package with the DB tests on. Each item scores (Impact + Risk) × (6 − Effort),
all three on 1–5; effort S is under half a day, M half a day to two, L
longer. Line numbers drift with every merge, so items name files and
functions. Re-measure (commands at the end) before acting on one.

Where it stands: 271 tests pass and 2 skip (real Stripe), 80.0% statement
coverage (root package 53.7%, mailer 44.6%, the rest 83–94%), 0 reachable
vulnerabilities, CI runs the DB tests on every PR. The code is young (first
commit 2026-09-15) and in good shape. The debt is concentrated in three
places:
- billing edge cases where money or access goes wrong quietly;
- two operator CLIs built on one shared flag set;
- test fakes that have drifted from the store they stand in for.

## Decisions

Recorded 2026-10-01. The items that depended on them are in Phase 2.

- [x] **D1 Grace runs from the last paid period.** A `past_due`
  subscription keeps access until the start of the unpaid period (the
  items' `current_period_start`) plus `stripe.grace_days`, so
  `grace_days = 0` ends access at the failed renewal. README's
  `stripe.grace_days` paragraph changes with it.
- [x] **D2 End refuses Stripe rows.** A Stripe entitlement mirrors its
  subscription, so the admin page hides End for `source = stripe` and the
  handler refuses it with "cancel it in Stripe". Suspending the account
  stays the immediate cut-off; the gateway rejects every key of a
  suspended account and reconcile does not touch account status.
- [x] **D3 One instance.** The App Platform app runs a single instance.
  Daily jobs and in-process limits need no cross-instance locks; a
  per-subscription mutex is enough to serialize reconciles. README says so.
- [x] **D4 `/healthz` is liveness only.** It answers 200 while the process
  serves and stops depending on the DB ping, so a DB blip cannot get the
  container restarted and its resolver cache wiped. DB health is reported
  by the prober's Discord alerts instead.
- [x] **D5 Upgrades invoice immediately.** `ChangePlan` uses
  `always_invoice` when the new plan's `Total` is higher, and keeps
  `create_prorations` (a credit on the next invoice) for a downgrade.
- [x] **D6 Block a second subscription.** `Checkout.Create` refuses an
  account that already has an active Stripe plan (covering the CLI, which
  never checks), using one shared "active plan" predicate. It also expires
  the customer's other open Checkout Sessions, so two tabs cannot both be
  paid. Reconcile alerts if an account still ends up with two.
- [x] **D7a No `reflect`.** Banned in production code and tests, as in
  go-mtgban and mtgban-website: revive's `imports-blocklist` through
  `.revive.toml`, after the 6 billing test files that import it are
  rewritten.
- [x] **D7b No rule on calls folded into an `if`.** Follow the
  surrounding code; not a review point and not a sweep target.

## Phase 1: correctness fixes

Small and independent; one PR per checklist line. Each line has a
GitHub issue, assigned to the-muppet2. Each lands with a test that
fails before the fix.

| # | Item | Type | I/R/E | Score | Evidence |
|---|---|---|---|---|---|
| 1 | CLI `plan change` silently drops games | Bug | 4/3/1 | 35 | `runBilling`: `-games` defaults to `magic` on the shared flag set and `plan()` always passes it, so `plan change -email x -package all_data` on a magic+pokemon customer removes pokemon and its `extra_game` item, with proration |
| 2 | Revocation can lose to an in-flight lookup | Security | 3/4/1 | 35 | `Resolver.Resolve` stores the DB result after unlocking. An `Invalidate` that lands during the query is overwritten, so a revoked key works for up to `cache_ttl_seconds`, contrary to README "drops the cached entry immediately" |
| 3 | A hung DB hangs requests, and stale grace never applies | Correctness | 4/3/1 | 35 | `Resolver.Resolve` calls `LookupKey` on the request context with no deadline; the DSN has no `connect_timeout`. Stale entries are served only on *error* |
| 4 | Plan change re-prices grandfathered items | Billing | 3/3/1 | 30 | `ChangePlan` keys items by raw `Price.LookupKey`; reconcile uses `priceLookupKey`, which falls back to metadata after `Seed` moved the key. Any change deletes and re-adds those items at today's price |
| 5 | `DEV_ACCESS,` is stored as `DEV_ACCESS` | Security | 2/3/1 | 25 | `canonicalStoreScope` matches presets on the whole string only. `"DEV_ACCESS,"` canonicalizes to `DEV_ACCESS`, which it rejects on input. Only the handler's exact-match check stops it. Presets must stand alone |
| 6 | Daily summary silently dropped past 2,000 chars | Ops | 2/3/1 | 25 | `discord.Post` sends one message; `SummaryText` writes a line per account×game and per new key. Discord answers 400 and only a log line remains |
| 7 | Permanent reconcile errors retried for 3 days, alerting each time | Billing/Ops | 3/3/2 | 24 | `Webhook` answers 500 for every `apply` error, including ones that cannot succeed: no metadata, unknown package, `ErrNoAccount`, `ValidationError`. `All` alerts the same subscriptions nightly, twice |
| 8 | Daily jobs have no panic recovery | Ops | 3/3/1 | 30 | `runDaily` runs `fn` bare in a background goroutine, so one panic in reconcile or reminders ends the process |
| 9 | `checkout link` prints a struct | Bug | 2/2/1 | 20 | `runBilling` prints `billing.Session` with `%s`: `{cs_… https://…}` |
| 10 | Client disconnects counted as upstream 502s | Metering | 2/2/1 | 20 | `ErrorHandler` has no `context.Canceled` case: it logs "upstream unavailable", writes 502, and usage counts an error. The 499 path is unreachable |
| 11 | Upstream transport has no dial or TLS timeout | Correctness | 2/2/1 | 20 | `newProxy` builds a bare `http.Transport`; a blackholed upstream waits the full 300 s |
| 12 | Plan change reports failure after Stripe changed | Billing/UX | 2/2/1 | 20 | `ChangePlan` returns an error when the follow-up reconcile fails; the portal shows "Could not start checkout" for a change that went through |
| 13 | Invite stays burned when the client disconnects | Billing | 2/2/1 | 20 | `Checkout.Create`'s deferred `ReleaseInvite` uses the request context and drops its error |
| 14 | Ended Stripe rows get a new end date on every touch | Data | 2/2/1 | 20 | `MapStatus` uses `now` for ended subscriptions; use `sub.EndedAt` |
| 15 | Trial reminder sent to customers who already bought | UX | 2/2/1 | 20 | `SendTrialReminders` checks only account status, not a Stripe plan or an ended trial entitlement. It also sends before marking |
| 16 | Account page says "No active access" when the DB errors | UX | 2/2/1 | 20 | `renderAccount` logs `ListEntitlements`/`ListKeys` errors and renders empty lists |
| 17 | SMTP on port 465 hangs every send | Ops | 2/2/1 | 20 | `SMTP.Send` dials plain TCP then requires STARTTLS; `FromEnv` accepts any port |
| 18 | Trial grant can lock a patron out for 180 days | Correctness | 3/2/2 | 20 | `CreateTrial` commits, then `AddEntitlement` runs on its own and is compensated by `DeleteTrial`. A crash between them leaves a trial with no access. Do both in `CreateTrial`'s transaction |
| 19 | Duplicate `RateLimit-Limit` header | API | 2/1/1 | 15 | The handler sets it, then ReverseProxy adds the backend's own (a per-IP value). Delete it in `ModifyResponse` |
| 20 | First key on the success page fails | UX | 2/1/1 | 15 | `success.html` labels it "(optional)", but `createKey` requires a label |
| 21 | CLI `grant end` and `account add` missing from the activity log | Audit | 2/1/1 | 15 | `grant end` audits with account 0 because `EndEntitlement` returns nothing; `account add` is not audited. README says CLI and web alike |
| 22 | Meter: one 30 s budget for 3 attempts; rows recorded after Close vanish | Metering | 1/2/1 | 15 | `UsageMeter.flush`, `Record` |
| 23 | `ClientIP` reads the first line of a repeated header | Security (latent) | 1/2/1 | 15 | `r.Header.Get` instead of the last of `Values`. Only matters if a multi-line header is configured |
| 24 | Wrong statuses and unlogged errors in the portal | Ops | 1/2/1 | 15 | `currentIntervalFor` returns 400 for store and Stripe failures. `adminTarget` turns any store error into 404. `changePlan` and `checkoutPost` drop errors |
| 25 | Bearer scheme case-sensitive; any `Authorization` shadows `?key=` | API | 1/1/1 | 10 | `bearerKey` |
| 26 | Daily summary lists today's keys as yesterday's | Ops | 1/1/1 | 10 | `KeysCreatedSince` has no upper bound |
| 27 | `RevokeKeyByPrefix` is SELECT then an unguarded UPDATE | Data | 1/1/1 | 10 | Collapse into one `UPDATE … WHERE revoked_at IS NULL RETURNING` |

### Phase 1 checklist

- [ ] CLI billing: #1, #9 (land with Phase 3's per-verb flag sets, or fix
  the defaults in place first). Issue [#33](https://github.com/mtgban/api-gatewahy/issues/33)
- [x] Resolver: #2 (generation counter), #3 (lookup deadline, then
  stale-on-timeout); add a `-race` test with a blocking source. Issue [#34](https://github.com/mtgban/api-gatewahy/issues/34)
- [x] Gateway proxy: #10, #11, #19, #23, #25. Issue [#35](https://github.com/mtgban/api-gatewahy/issues/35)
- [x] Billing reconcile: #4, #7, #14; then #12, #13. Issue [#36](https://github.com/mtgban/api-gatewahy/issues/36)
- [x] apiaccess: #5, #18, #21 (`EndEntitlement` returns the row), #27. Issue [#37](https://github.com/mtgban/api-gatewahy/issues/37)
- [x] Jobs: #8 (recover per run, log, alert). Issue [#38](https://github.com/mtgban/api-gatewahy/issues/38)
- [x] Discord: #6 (split on line boundaries). Issue [#39](https://github.com/mtgban/api-gatewahy/issues/39)
- [x] Portal: #15, #16, #20, #24. Issue [#40](https://github.com/mtgban/api-gatewahy/issues/40)
- [x] Mailer: #17. Issue [#41](https://github.com/mtgban/api-gatewahy/issues/41)
- [x] Meter and summary: #22, #26. Issue [#42](https://github.com/mtgban/api-gatewahy/issues/42)

## Phase 2: decision-dependent

| Item | Decision | Evidence |
|---|---|---|
| `past_due` access ends at the unpaid period's start + `grace_days` | D1 | `subPeriodEnd` uses the current period's end, which a failed renewal has already advanced |
| Hide End on Stripe rows and refuse it in `adminEndEntitlement` (and CLI `grant end`), pointing to Stripe and to Suspend | D2 | `admin_account.html`; `UpsertStripeEntitlement` rewrites status from Stripe |
| Serialize reconciles per subscription with a mutex; `All` re-fetches before writing a status change | D3 | `Reconciler.apply`; `UpsertStripeEntitlement` is last-writer-wins |
| README: one instance; the per-hour limits are per process | D3 | README "Customer pages", Configuration |
| `/healthz` answers from the process alone; the prober pings the DB and alerts on a state change | D4 | `newMux`, `gateway.Prober` |
| `always_invoice` when the plan's total goes up | D5 | `ChangePlan` |
| `Checkout.Create` refuses an active plan and expires other open sessions; one "active plan" predicate; reconcile alerts on two | D6 | `SubscriptionFor` counts `Status == "active"`, `hasActiveStripePlan` uses `ActiveAt`; the CLI never checks |
| Ban `reflect`: revive `imports-blocklist` in `.revive.toml`, rewrite the 6 test files with `slices`/`maps` or typed comparisons | D7a | `billing/{seed,entitlement_db,stores,plan,planchange,reconcile}_test.go` |

- [x] D3: reconcile mutex, README note. Issue [#45](https://github.com/mtgban/api-gatewahy/issues/45)
- [x] D4: liveness `/healthz`, DB check in the prober. Issue [#46](https://github.com/mtgban/api-gatewahy/issues/46)
- [ ] D5: `always_invoice` on upgrade, with a fake-Stripe test for each
  direction. Issue [#47](https://github.com/mtgban/api-gatewahy/issues/47)
- [ ] D6: block in `Checkout.Create`, expire sibling sessions, alert in
  reconcile. Issue [#48](https://github.com/mtgban/api-gatewahy/issues/48)
- [x] D7a: rewrite the 6 test files, then turn the lint rule on in the same
  PR. Issue [#49](https://github.com/mtgban/api-gatewahy/issues/49)
- [x] D2: refuse End on Stripe rows in the web admin and the CLI, with
  tests. Issue [#44](https://github.com/mtgban/api-gatewahy/issues/44)
- [ ] D1: grace from `current_period_start`, a test per interval, README.
  Issue [#43](https://github.com/mtgban/api-gatewahy/issues/43)

## Phase 3: structure

| # | Item | Type | I/R/E | Score | Evidence |
|---|---|---|---|---|---|
| S1 | One validated config owns the gateway's defaults | Code | 2/2/1 | 20 | `gateway.New` defaults some options and not others: a zero `UpstreamTimeout` makes every request 504, and a zero `PerKeyBurst` makes every request 429. The per-IP defaults are a second copy of `config.applyDefaults`. Config ignores unknown keys |
| S2 | One signature minter for handler and prober | Code | 2/2/1 | 20 | `probe.go` builds its own `apisig.Claims` and path, so a change to the handler's claims would pass the probe while live traffic fails |
| S3 | Test fakes drift from the store | Test | 3/3/3 | 18 | `portal/mem_test.go` diverges from apiaccess in 8 ways: no scope/mode canonicalization; Upsert drops `ValidFrom`; `SetStripeCustomerID` invents accounts; `SummarizeUsage` ungrouped; different ordering; `GrantedAt` hard-coded; wall clock. There are three memStores and two `fakeStores` |
| S4 | Migrations run every boot with no version, lock or timeout | Schema | 3/3/3 | 18 | `ensureSchema` re-runs 3 `ALTER TABLE` and ~10 `CREATE INDEX` per start. These take table locks with no `lock_timeout`, so a boot can queue behind a long reader and stall lookups behind it. README's schema note lists 4 of 11 tables |
| S5 | Two operator CLIs on one shared flag set | Code | 3/2/3 | 15 | `runAdmin` 213 lines, `runBilling` 172. Every verb accepts every flag, and defaults leak across verbs (#1). The happy paths are untestable because `billingDeps.store` is concrete; that is how #9 shipped |
| S6 | CLI and web admin re-implement the same rules | Dup | 2/2/2 | 16 | Manual grant validation (they already differ: web rejects a past `until`), key kind (`keyKindFor` vs `hasActiveStripePlan`), usage window defaults, `splitList`/`listValues`, the grant audit string |
| S7 | Portal routes are three hand-kept lists | Code | 2/3/3 | 15 | `registerX` ×5, `Reserved`, and nothing asserting every POST is guarded. A new unguarded route or a missing reserved prefix passes CI |
| S8 | Portal handler plumbing | Code | 2/2/2 | 16 | `r.Form` is populated only because `withSession` reads `csrf`; the `sameOrigin` preamble ×4; "issue a session" ×3; "no subscription to change" ×2; `renderConfirm` takes 10 positional arguments |
| S9 | Schema constraints and named constants | Schema | 2/2/2 | 16 | No CHECK on status, source, kind or email normalization; 34 bare `"active"`/`"stripe"`/… literals outside apiaccess. Needs S4 |
| S10 | Resolver hardening | Perf | 2/2/2 | 16 | Negative entries share the 10,000 budget, so forged keys evict real ones and their stale grace. No coalescing of concurrent misses (`singleflight`) |
| S11 | Session token purpose binding | Security | 1/2/1 | 15 | `ban_session` and `ban_pending` share key and format; not exploitable today (pending keys are whitelisted). `Codec` accepts an empty secret |
| S12 | Prune unbounded tables | Schema | 1/2/1 | 15 | `stripe_events`, expired `invites`, `admin_actions` (decide retention) |
| S13 | Layering | Arch | 1/1/2 | 8 | portal imports `gateway` for `ClientIP` only; `SummaryText` lives in gateway; the JSON error shape is hand-written in `serve.go` ×3; apiaccess depends on the website's `timeseries.SQLConfig`, whose unquoted DSN breaks on an empty password |
| S14 | apiaccess row-scanning boilerplate | Dup | 1/1/2 | 8 | 10 hand-written rows loops, 4 copies of the "0 means all" filter; a generic `queryAll` |

### Phase 3 checklist

- [ ] S5 first: a verb table, one `FlagSet` per verb, and narrow store
  interfaces so the happy paths get tests. This also fixes #1 and #9 at the
  root. Issue [#50](https://github.com/mtgban/api-gatewahy/issues/50)
- [ ] S6 in the same series: `apiaccess.ManualGrant`, `KeyKindFor`, and a
  shared usage window, used by both admins. Issue [#51](https://github.com/mtgban/api-gatewahy/issues/51)
- [ ] S3: an `apiaccess/apiaccesstest` in-memory store plus a contract test
  that runs one table against it and against Postgres; delete the three
  memStores. Fold the two `fakeStores` and Stripe fakes into
  `billing/billingtest`. Issue [#52](https://github.com/mtgban/api-gatewahy/issues/52)
- [x] S1, S2 (one PR). Issue [#53](https://github.com/mtgban/api-gatewahy/issues/53)
- [ ] S4, then S9 and S12 through it. Issue [#54](https://github.com/mtgban/api-gatewahy/issues/54)
- [x] S7, then S8. Issue [#55](https://github.com/mtgban/api-gatewahy/issues/55)
- [x] S10. Issue [#56](https://github.com/mtgban/api-gatewahy/issues/56)
- [x] S11. Issue [#57](https://github.com/mtgban/api-gatewahy/issues/57)
- [ ] S13, S14 opportunistically, when touching those files. Issue [#58](https://github.com/mtgban/api-gatewahy/issues/58)

## Phase 4: docs

- [ ] README drift (issue [#59](https://github.com/mtgban/api-gatewahy/issues/59)):
  - error table: 401 also covers a suspended account; 403 covers no store
    scope; 429 has three sources; 502 also passes 429 through; an upstream
    429's body is not JSON
  - the origin check also reads `Referer`
  - "`admin key create`" is `key create`
  - one instance (D3), so the per-hour limits are per process
  - the schema note lists 4 of 11 tables
  - the `-games` default is `magic` but not forced
  - an abandoned session's invite stays spent
- [ ] Code comments that describe history: schema.go's "Phase 2"/"Phase 3"
  and "Appended so…"; `reconcile.go`'s "the spec's table"; the
  `AddEntitlement` comment that overclaims (#5); trial.go's "15-day";
  `name := name` loop copies, dead since Go 1.22. Issue [#60](https://github.com/mtgban/api-gatewahy/issues/60)

## Not worth doing

- **Structured logging or levels**: one binary and one log stream; the
  rule that matters (never log a secret) is already followed.
- **Swapping `lib/pq` for pgx**: pq is maintained, and LISTEN/NOTIFY, COPY
  and arrays all work. A swap touches every query for no measured gain.
- **Partitioning `usage`**: revisit when the daily prune is measurably
  slow; row counts today don't call for it.
- **Splitting `package main`**: 1,225 lines over four files. S5 shrinks it
  further.
- **A frontend build step or JS**: the portal is 17 templates and one
  stylesheet.

## Re-measuring

```bash
APIACCESS_TEST_DSN='postgres://u:p@localhost:5432/scratch?sslmode=disable' \
  go test -race -count=1 -p 1 -coverprofile=c.out ./... && go tool cover -func=c.out | tail -1
go test -count=1 -p 1 -json ./... | jq -r 'select(.Test and .Action!="run" and .Action!="output") | .Action' | sort | uniq -c
govulncheck ./...
go list -m -u -f '{{if and .Update (not .Indirect)}}{{.Path}} {{.Version}} -> {{.Update.Version}}{{end}}' all
```
