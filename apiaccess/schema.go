package apiaccess

import "database/sql"

// schemaStatements are applied in order on every start. Each is idempotent.
var schemaStatements = []string{
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
	// Appended so an existing database picks it up on the next start.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_live_prefix ON api_keys (prefix) WHERE revoked_at IS NULL`,
	// Phase 2: Stripe.
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
	// Phase 3: portal.
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
}

func ensureSchema(db *sql.DB) error {
	for _, stmt := range schemaStatements {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}
