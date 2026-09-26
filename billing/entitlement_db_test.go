package billing

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/mtgban/mtgban-website/timeseries"
)

// sqlConfigFromDSN turns a postgres:// DSN into the SQLConfig apiaccess.NewClient wants.
func sqlConfigFromDSN(dsn string) (timeseries.SQLConfig, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return timeseries.SQLConfig{}, err
	}
	port, _ := strconv.Atoi(u.Port())
	password, _ := u.User.Password()
	return timeseries.SQLConfig{
		Host:     u.Hostname(),
		Port:     port,
		User:     u.User.Username(),
		Password: password,
		DBName:   strings.TrimPrefix(u.Path, "/"),
		SSLMode:  u.Query().Get("sslmode"),
	}, nil
}

// TestCatalogEntitlementRoundtrip runs Plan.Resolve for every catalog
// package through real apiaccess validation, so a scope or mode change that
// apiaccess would reject is caught here rather than in production.
func TestCatalogEntitlementRoundtrip(t *testing.T) {
	dsn := os.Getenv("APIACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("APIACCESS_TEST_DSN not set")
	}
	cfg, err := sqlConfigFromDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	client, err := apiaccess.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	// apiaccess's own DB tests wipe these tables between runs, so leave no
	// rows behind for them to trip over.
	t.Cleanup(func() {
		raw, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Errorf("cleanup: open: %v", err)
			return
		}
		defer func() { _ = raw.Close() }()
		if _, err := raw.Exec("DELETE FROM entitlements WHERE external_ref LIKE 'sub_catalog_%'"); err != nil {
			t.Errorf("cleanup entitlements: %v", err)
		}
		if _, err := raw.Exec("DELETE FROM accounts WHERE email = 'catalog-roundtrip@example.com'"); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	})

	ctx := context.Background()
	const email = "catalog-roundtrip@example.com"
	acct, err := client.GetAccountByEmail(ctx, email)
	if errors.Is(err, apiaccess.ErrNotFound) {
		acct, err = client.CreateAccount(ctx, email, "")
	}
	if err != nil {
		t.Fatal(err)
	}

	for _, pkg := range testCatalog.Packages {
		plan := Plan{Package: pkg.Key, Interval: "monthly", Games: []string{"magic"}}
		if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
			plan.Stores = []string{"cardkingdom", "starcitygames"}
		}
		plan, err := plan.Normalize(testCatalog)
		if err != nil {
			t.Fatalf("%s: normalize: %v", pkg.Key, err)
		}
		resolved, err := plan.Resolve(ctx, testCatalog, newFakeStores())
		if err != nil {
			t.Fatalf("%s: resolve: %v", pkg.Key, err)
		}
		scope, modes := resolved.Scope, resolved.Modes
		e := apiaccess.Entitlement{
			AccountID:   acct.ID,
			Source:      "stripe",
			Games:       plan.Games,
			StoreScope:  scope,
			Modes:       modes,
			ExternalRef: "sub_catalog_" + pkg.Key,
		}
		got, err := client.UpsertStripeEntitlement(ctx, e)
		if err != nil {
			t.Fatalf("%s: upsert: %v", pkg.Key, err)
		}
		want, err := apiaccess.ValidateStoreScope(scope)
		if err != nil {
			t.Fatalf("%s: validate scope: %v", pkg.Key, err)
		}
		if got.StoreScope != want {
			t.Errorf("%s: store_scope %q want %q", pkg.Key, got.StoreScope, want)
		}
		if !reflect.DeepEqual(got.Modes, modes) {
			t.Errorf("%s: modes %v want %v", pkg.Key, got.Modes, modes)
		}
	}
}
