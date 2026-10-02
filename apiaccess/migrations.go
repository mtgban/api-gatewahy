package apiaccess

import (
	"context"
	"database/sql"
	"fmt"
)

// migration is one schema change, applied once and recorded in schema_migrations.
type migration struct {
	version    int
	name       string
	statements []string
}

// migrations run in version order. Append only: never edit one that has shipped.
var migrations = []migration{
	{1, "initial schema", initialSchema},
	{2, "check statuses, sources, key kinds and email form", checkConstraints},
}

// migrationLockKey is the advisory lock that serializes concurrent boots.
const migrationLockKey int64 = 0x6170696d696772 // "apimigr"

// checkConstraints pins the columns to the typed constants. Each drops its
// constraint first so a rerun is harmless; emails are normalized before the check.
var checkConstraints = []string{
	`UPDATE accounts SET email = lower(btrim(email)) WHERE email <> lower(btrim(email))`,
	`ALTER TABLE accounts
    DROP CONSTRAINT IF EXISTS accounts_status_check,
    ADD CONSTRAINT accounts_status_check CHECK (status IN ('active', 'suspended')),
    DROP CONSTRAINT IF EXISTS accounts_email_normalized,
    ADD CONSTRAINT accounts_email_normalized CHECK (email = lower(btrim(email)))`,
	`ALTER TABLE entitlements
    DROP CONSTRAINT IF EXISTS entitlements_status_check,
    ADD CONSTRAINT entitlements_status_check CHECK (status IN ('active', 'ended')),
    DROP CONSTRAINT IF EXISTS entitlements_source_check,
    ADD CONSTRAINT entitlements_source_check CHECK (source IN ('stripe', 'manual', 'trial'))`,
	`ALTER TABLE api_keys
    DROP CONSTRAINT IF EXISTS api_keys_kind_check,
    ADD CONSTRAINT api_keys_kind_check CHECK (kind IN ('ban_live', 'ban_demo'))`,
}

// initialSchema is the schema from before migrations, when every boot reran it.
// Its IF NOT EXISTS forms let a database that predates migrations record it.
var initialSchema = []string{
	`CREATE TABLE IF NOT EXISTS accounts (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    status     text NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    note       text NOT NULL DEFAULT ''
)`,
	`CREATE TABLE IF NOT EXISTS api_keys (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id   bigint NOT NULL REFERENCES accounts(id),
    key_hash     text NOT NULL UNIQUE,
    prefix       text NOT NULL,
    label        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
)`,
	`CREATE INDEX IF NOT EXISTS idx_api_keys_account ON api_keys (account_id)`,
	`CREATE TABLE IF NOT EXISTS entitlements (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id   bigint NOT NULL REFERENCES accounts(id),
    source       text NOT NULL,
    games        text[] NOT NULL,
    store_scope  text NOT NULL,
    modes        text[] NOT NULL,
    addons       text[] NOT NULL DEFAULT '{}',
    status       text NOT NULL DEFAULT 'active',
    valid_from   timestamptz NOT NULL DEFAULT now(),
    valid_until  timestamptz,
    external_ref text,
    note         text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
)`,
	`CREATE INDEX IF NOT EXISTS idx_entitlements_account ON entitlements (account_id) WHERE status = 'active'`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_entitlements_external_ref ON entitlements (external_ref) WHERE external_ref IS NOT NULL`,
	`CREATE TABLE IF NOT EXISTS usage (
    ts          timestamptz NOT NULL DEFAULT now(),
    key_id      bigint NOT NULL,
    account_id  bigint NOT NULL,
    game        text NOT NULL,
    path        text NOT NULL,
    status      int NOT NULL,
    bytes       bigint NOT NULL,
    duration_ms int NOT NULL,
    client_ip   inet
)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage (ts)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_account_ts ON usage (account_id, ts)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_key_ts ON usage (key_id, ts)`,
	// Only one live (non-revoked) key may use a given prefix.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_live_prefix ON api_keys (prefix) WHERE revoked_at IS NULL`,
	// Stripe: customer linkage, invite codes, and webhook event log.
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS stripe_customer_id text UNIQUE`,
	`CREATE TABLE IF NOT EXISTS invites (
    token_hash   text PRIMARY KEY,
    interval_key text NOT NULL,
    email        text NOT NULL DEFAULT '',
    expires_at   timestamptz NOT NULL,
    used_at      timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    note         text NOT NULL DEFAULT ''
)`,
	`CREATE TABLE IF NOT EXISTS stripe_events (
    id           text PRIMARY KEY,
    type         text NOT NULL,
    received_at  timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz
)`,
	// Portal: magic-link sign-in, trials, admin audit log, and session epoch.
	`CREATE TABLE IF NOT EXISTS magic_links (
    token_hash  text PRIMARY KEY,
    account_id  bigint NOT NULL REFERENCES accounts(id),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
)`,
	`CREATE TABLE IF NOT EXISTS trials (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    patreon_email    text NOT NULL,
    account_id       bigint NOT NULL REFERENCES accounts(id),
    granted_at       timestamptz NOT NULL DEFAULT now(),
    ends_at          timestamptz NOT NULL,
    reminder_sent_at timestamptz
)`,
	`CREATE INDEX IF NOT EXISTS idx_trials_email_granted ON trials (patreon_email, granted_at DESC)`,
	`CREATE TABLE IF NOT EXISTS handoff_nonces (
    nonce      text PRIMARY KEY,
    expires_at timestamptz NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS admin_actions (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at         timestamptz NOT NULL DEFAULT now(),
    actor      text NOT NULL,
    action     text NOT NULL,
    account_id bigint,
    target     text NOT NULL DEFAULT '',
    detail     text NOT NULL DEFAULT ''
)`,
	`CREATE INDEX IF NOT EXISTS idx_admin_actions_account ON admin_actions (account_id, at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_magic_links_expires ON magic_links (expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_handoff_nonces_expires ON handoff_nonces (expires_at)`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS session_epoch bigint NOT NULL DEFAULT 0`,
	// A key without a kind is live.
	`ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'ban_live'`,
}

// migrate applies every migration the database has not recorded yet.
func migrate(ctx context.Context, db *sql.DB) error {
	return applyMigrations(ctx, db, migrations)
}

// applyMigrations runs the unapplied entries of list in one transaction under
// the advisory lock, so a boot either applies all of them or none.
func applyMigrations(ctx context.Context, db *sql.DB, list []migration) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return fmt.Errorf("lock timeout: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	// CREATE ... IF NOT EXISTS checks the CREATE privilege first, so look before creating.
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("schema_migrations: %w", err)
	}
	if !exists {
		if _, err = tx.ExecContext(ctx, `CREATE TABLE schema_migrations (
    version    int PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
			return fmt.Errorf("schema_migrations: %w", err)
		}
	}
	applied, err := appliedVersions(ctx, tx)
	if err != nil {
		return err
	}
	for _, m := range list {
		if applied[m.version] {
			continue
		}
		for i, stmt := range m.statements {
			if _, err = tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %d (%s) statement %d: %w", m.version, m.name, i+1, err)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			return fmt.Errorf("migration %d (%s): record: %w", m.version, m.name, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func appliedVersions(ctx context.Context, tx *sql.Tx) (map[int]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("read versions: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read versions: %w", err)
	}
	return applied, nil
}
