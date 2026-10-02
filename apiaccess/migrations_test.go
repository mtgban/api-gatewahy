package apiaccess

import (
	"context"
	"database/sql"
	"maps"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMigrationVersionsAscend(t *testing.T) {
	for i, m := range migrations {
		if m.version != i+1 || m.name == "" || len(m.statements) == 0 {
			t.Errorf("migration %d: version %d, name %q, %d statements", i, m.version, m.name, len(m.statements))
		}
	}
}

// freshDB opens APIACCESS_TEST_DSN on a new empty schema, or skips; cleanup drops the schema.
func freshDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("APIACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("APIACCESS_TEST_DSN not set")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "mig_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Errorf("drop %s: %v", schema, err)
		}
		_ = admin.Close()
	})
	return db
}

// appliedAt reads schema_migrations as version to stamp.
func appliedAt(t *testing.T, db *sql.DB) map[int]time.Time {
	t.Helper()
	rows, err := db.Query(`SELECT version, applied_at FROM schema_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[int]time.Time{}
	for rows.Next() {
		var v int
		var at time.Time
		if err := rows.Scan(&v, &at); err != nil {
			t.Fatal(err)
		}
		got[v] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func versionsOf(list []migration) []int {
	var vs []int
	for _, m := range list {
		vs = append(vs, m.version)
	}
	return vs
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var reg sql.NullString
	if err := db.QueryRow(`SELECT to_regclass($1)::text`, name).Scan(&reg); err != nil {
		t.Fatal(err)
	}
	return reg.Valid
}

func TestMigrateFreshThenNothing(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	first := appliedAt(t, db)
	if got := slices.Sorted(maps.Keys(first)); !slices.Equal(got, versionsOf(migrations)) {
		t.Fatalf("versions %v, want %v", got, versionsOf(migrations))
	}
	if !tableExists(t, db, "admin_actions") {
		t.Error("admin_actions missing after migrate")
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if second := appliedAt(t, db); !maps.Equal(first, second) {
		t.Errorf("second boot changed schema_migrations: %v then %v", first, second)
	}
}

// A database the old ensureSchema built has every table and no schema_migrations.
func TestMigrateAdoptsExistingSchema(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	for _, stmt := range initialSchema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO accounts (email) VALUES ('old@example.com')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(appliedAt(t, db))); !slices.Equal(got, versionsOf(migrations)) {
		t.Errorf("versions %v, want %v", got, versionsOf(migrations))
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM accounts WHERE email = 'old@example.com'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("existing row: %d %v", n, err)
	}
}

func TestApplyMigrationsRunsOnlyNew(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	before := appliedAt(t, db)
	next := len(migrations) + 1
	list := append(slices.Clone(migrations), migration{next, "probe", []string{`CREATE TABLE IF NOT EXISTS zz_probe (id int)`}})
	if err := applyMigrations(ctx, db, list); err != nil {
		t.Fatal(err)
	}
	after := appliedAt(t, db)
	if len(after) != len(before)+1 || !tableExists(t, db, "zz_probe") {
		t.Fatalf("after the probe: %v, zz_probe %v", after, tableExists(t, db, "zz_probe"))
	}
	delete(after, next)
	if !maps.Equal(before, after) {
		t.Errorf("earlier versions re-stamped: %v then %v", before, after)
	}
	withProbe := appliedAt(t, db)
	if err := applyMigrations(ctx, db, list); err != nil {
		t.Fatal(err)
	}
	if third := appliedAt(t, db); !maps.Equal(withProbe, third) {
		t.Errorf("third run changed schema_migrations: %v then %v", withProbe, third)
	}
}

func TestApplyMigrationsFailureRollsBack(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	before := appliedAt(t, db)
	n := len(migrations)
	list := append(slices.Clone(migrations),
		migration{n + 1, "probe", []string{`CREATE TABLE zz_probe (id int)`}},
		migration{n + 2, "broken", []string{`SELECT nope FROM nowhere`}})
	err := applyMigrations(ctx, db, list)
	if err == nil || !strings.Contains(err.Error(), "migration "+strconv.Itoa(n+2)+" (broken)") {
		t.Fatalf("err %v, want it to name migration %d", err, n+2)
	}
	if after := appliedAt(t, db); !maps.Equal(before, after) || tableExists(t, db, "zz_probe") {
		t.Errorf("failed run left %v, zz_probe %v", after, tableExists(t, db, "zz_probe"))
	}
}

func TestMigrateGivesUpOnHeldLock(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = holder.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, migrationLockKey) }()
	start := time.Now()
	err = migrate(ctx, db)
	if err == nil || !strings.Contains(err.Error(), "advisory lock") || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("err %v, want an advisory lock timeout", err)
	}
	if d := time.Since(start); d < 4*time.Second || d > 30*time.Second {
		t.Errorf("gave up after %v, want about 5s", d)
	}
}

// Migrations run once, but every statement still tolerates a rerun.
func TestMigrationStatementsRerun(t *testing.T) {
	db := freshDB(t)
	if err := migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		for i, stmt := range m.statements {
			if _, err := db.Exec(stmt); err != nil {
				t.Errorf("migration %d statement %d: %v", m.version, i+1, err)
			}
		}
	}
}

// After the first boot, a role without CREATE on the schema still boots.
func TestSecondBootNeedsNoCreate(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	var canCreateRole bool
	if err := db.QueryRow(`SELECT rolcreaterole OR rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&canCreateRole); err != nil || !canCreateRole {
		t.Skip("the test connection cannot create roles")
	}
	role := "mig_nocreate_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	for _, stmt := range []string{
		"CREATE ROLE " + role + " NOLOGIN",
		"GRANT USAGE ON SCHEMA " + schema + " TO " + role,
		"GRANT SELECT, INSERT ON schema_migrations TO " + role,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if _, err := db.Exec("DROP OWNED BY " + role + "; DROP ROLE " + role); err != nil {
			t.Errorf("drop %s: %v", role, err)
		}
	})
	u, err := url.Parse(os.Getenv("APIACCESS_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	q.Set("role", role)
	u.RawQuery = q.Encode()
	as, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = as.Close() }()
	var user string
	var canCreate bool
	if err := as.QueryRow(`SELECT current_user, has_schema_privilege(current_schema(), 'CREATE')`).Scan(&user, &canCreate); err != nil || user != role || canCreate {
		t.Fatalf("connected as %q, create %v, err %v; want %s without CREATE", user, canCreate, err, role)
	}
	if err := migrate(ctx, as); err != nil {
		t.Errorf("second boot without CREATE: %v", err)
	}
	probe := append(slices.Clone(migrations), migration{len(migrations) + 1, "probe", []string{`CREATE TABLE zz_probe (id int)`}})
	if err := applyMigrations(ctx, as, probe); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("a new migration without CREATE: err %v, want permission denied", err)
	}
}
