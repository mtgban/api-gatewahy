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
}

func ensureSchema(db *sql.DB) error {
	for _, stmt := range schemaStatements {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}
