package apiaccess

import (
	"context"
	"errors"
	"testing"
)

func TestNormalizeEmail(t *testing.T) {
	if got := NormalizeEmail("  Foo@Example.COM "); got != "foo@example.com" {
		t.Errorf("got %q", got)
	}
}

func TestAccountsRoundTrip(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	a, err := c.CreateAccount(ctx, "CK@Example.com", "card kingdom")
	if err != nil {
		t.Fatal(err)
	}
	if a.Email != "ck@example.com" || a.Status != "active" || a.Note != "card kingdom" || a.ID == 0 {
		t.Errorf("created %+v", a)
	}

	if _, err := c.CreateAccount(ctx, "ck@example.com", ""); err == nil {
		t.Error("duplicate email accepted")
	}

	got, err := c.GetAccountByEmail(ctx, "ck@example.com")
	if err != nil || got.ID != a.ID {
		t.Errorf("get by email: %+v %v", got, err)
	}

	if _, err := c.GetAccountByEmail(ctx, "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing account: %v", err)
	}

	if err := c.SetAccountStatus(ctx, a.ID, "suspended"); err != nil {
		t.Fatal(err)
	}
	got, _ = c.GetAccount(ctx, a.ID)
	if got.Status != "suspended" {
		t.Errorf("status %q", got.Status)
	}

	list, err := c.ListAccounts(ctx)
	if err != nil || len(list) != 1 {
		t.Errorf("list: %v %v", list, err)
	}
}

func TestStripeCustomerID(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, err := c.CreateAccount(ctx, "stripe@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.StripeCustomerID != "" {
		t.Errorf("new account has customer %q", a.StripeCustomerID)
	}
	got, err := c.SetStripeCustomerID(ctx, a.ID, "cus_first")
	if err != nil || got != "cus_first" {
		t.Fatalf("first set: %q %v", got, err)
	}
	got, err = c.SetStripeCustomerID(ctx, a.ID, "cus_second")
	if err != nil || got != "cus_first" {
		t.Errorf("second set should keep the first id: %q %v", got, err)
	}
	found, err := c.GetAccountByStripeCustomer(ctx, "cus_first")
	if err != nil || found.ID != a.ID || found.StripeCustomerID != "cus_first" {
		t.Errorf("by customer: %+v %v", found, err)
	}
	if _, err := c.GetAccountByStripeCustomer(ctx, "cus_nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing customer: %v", err)
	}
	if _, err := c.SetStripeCustomerID(ctx, 999999, "cus_x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing account: %v", err)
	}
	byEmail, _ := c.GetAccountByEmail(ctx, "stripe@example.com")
	if byEmail.StripeCustomerID != "cus_first" {
		t.Errorf("customer id not read back: %+v", byEmail)
	}
}
