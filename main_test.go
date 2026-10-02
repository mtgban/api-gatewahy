package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apiproductlist"
)

func TestRunUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"bogus"}, &out, &errb)
	if code != 2 {
		t.Errorf("exit code %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Errorf("stderr %q", errb.String())
	}
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(nil, &out, &errb)
	if code != 2 {
		t.Errorf("exit code %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage:") {
		t.Errorf("stderr %q", errb.String())
	}
}

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "api-gatewahy ") {
		t.Errorf("stdout %q", out.String())
	}
}

// TestUsageNamesEveryVerb pins each verb table: the usage lists every verb,
// and a missing or unknown verb prints the same list on stderr.
func TestUsageNamesEveryVerb(t *testing.T) {
	want := map[string]string{
		"account":  "add | suspend | reinstate | list",
		"key":      "create | revoke | list",
		"grant":    "add | end | list",
		"catalog":  "seed",
		"checkout": "link",
		"invite":   "create",
		"stripe":   "reconcile",
		"portal":   "link",
		"plan":     "change",
	}
	var buf bytes.Buffer
	usage(&buf)
	for cmd, verbs := range want {
		if line := fmt.Sprintf("  %-10s %s\n", cmd, verbs); !strings.Contains(buf.String(), line) {
			t.Errorf("usage lacks %q:\n%s", line, buf.String())
		}
	}
	if len(adminVerbs)+len(billingVerbs) != len(want) {
		t.Errorf("%d commands have verb tables, want %d", len(adminVerbs)+len(billingVerbs), len(want))
	}
	for _, args := range [][]string{{"account"}, {"account", "frobnicate"}} {
		var out, errb bytes.Buffer
		code := runAdmin(context.Background(), &memStore{}, nil, args[0], args[1:], &out, &errb)
		if code != 2 || errb.String() != "usage: api-gatewahy account <"+want["account"]+">\n" {
			t.Errorf("%v: exit %d, stderr %q", args, code, errb.String())
		}
	}
	for _, args := range [][]string{{"plan"}, {"plan", "cancel"}} {
		var out, errb bytes.Buffer
		code := runBilling(context.Background(), billingDeps{}, args[0], args[1:], &out, &errb)
		if code != 2 || errb.String() != "usage: api-gatewahy plan <change>\n" {
			t.Errorf("%v: exit %d, stderr %q", args, code, errb.String())
		}
	}
}

// TestVerbRejectsAnotherVerbsFlag pins one FlagSet per verb: a flag only
// another verb takes exits 2 before the verb touches the store or Stripe.
func TestVerbRejectsAnotherVerbsFlag(t *testing.T) {
	s := &memStore{}
	admin(t, s, "account", "add", "-email", "ck@example.com")
	s.audit = nil
	for _, args := range [][]string{
		{"account", "list", "-email", "ck@example.com"},
		{"account", "add", "-email", "x@example.com", "-label", "prod"},
		{"key", "revoke", "-prefix", "ban_demo_x", "-email", "ck@example.com"},
		{"grant", "end", "-id", "1", "-games", "magic"},
	} {
		code, out, errb := admin(t, s, args...)
		if code != 2 || out != "" || !strings.Contains(errb, "flag provided but not defined") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errb)
		}
	}
	if len(s.accounts) != 1 || s.notified != 0 || len(s.audit) != 0 {
		t.Errorf("a rejected verb still ran: %d accounts, notified %d, audit %v", len(s.accounts), s.notified, s.audit)
	}

	api, store := newFakeBillingAPI(), newFakeBillingStore()
	store.addAccount(apiaccess.Account{ID: 1, Email: "x@example.com", Status: "active", StripeCustomerID: "cus_1"})
	d := billingTestDeps(api, store)
	for _, args := range [][]string{
		{"plan", "change", "-email", "x@example.com", "-package", "all_data", "-interval", "quarterly"},
		{"checkout", "link", "-email", "x@example.com", "-package", "all_data", "-sub", "sub_1"},
		{"portal", "link", "-email", "x@example.com", "-days", "7"},
	} {
		code, out, errb := billingCmd(t, d, args...)
		if code != 2 || out != "" || !strings.Contains(errb, "flag provided but not defined") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errb)
		}
	}
	if len(api.updates)+len(api.sessions)+len(api.portals) != 0 {
		t.Errorf("a rejected verb still reached Stripe: %d updates, %d sessions, %d portals", len(api.updates), len(api.sessions), len(api.portals))
	}
}

// TestEveryVerbRejectsAnUnknownFlag reads both tables, so a verb added later
// is covered: an undefined flag exits 2 before any store or Stripe call.
func TestEveryVerbRejectsAnUnknownFlag(t *testing.T) {
	rejected := func(t *testing.T, code int, out, errb string) {
		t.Helper()
		if code != 2 || out != "" || !strings.Contains(errb, "flag provided but not defined: -nosuchflag") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errb)
		}
	}
	adminRuns := [][]string{{"usage", "-nosuchflag"}}
	for cmd, verbs := range adminVerbs {
		for _, v := range verbs {
			adminRuns = append(adminRuns, []string{cmd, v.name, "-nosuchflag"})
		}
	}
	for _, args := range adminRuns {
		t.Run(strings.Join(args[:len(args)-1], " "), func(t *testing.T) {
			s := &memStore{}
			admin(t, s, "account", "add", "-email", "ck@example.com")
			s.audit = nil
			code, out, errb := admin(t, s, args...)
			rejected(t, code, out, errb)
			if len(s.accounts) != 1 || s.accounts[0].Status != "active" || len(s.keys)+len(s.ents)+len(s.audit)+s.notified != 0 {
				t.Errorf("side effect: accounts %+v, %d keys, %d grants, audit %v, notified %d", s.accounts, len(s.keys), len(s.ents), s.audit, s.notified)
			}
		})
	}

	for cmd, verbs := range billingVerbs {
		for _, v := range verbs {
			t.Run(cmd+" "+v.name, func(t *testing.T) {
				// A subscription to reconcile and an account to bill, so a verb that ran would leave a trace.
				api, store := newFakeBillingAPI(), newFakeBillingStore()
				pkgKey := apiproductlist.LookupKey("all_data", "monthly")
				api.addPrice(pkgKey)
				api.addSub(t, "sub_1", map[string]string{
					"package": "all_data", "interval": "monthly", "games": "magic", "stores": "", "account_id": "1",
				}, map[string]int64{pkgKey: 1})
				store.addAccount(apiaccess.Account{ID: 1, Email: "x@example.com", Status: "active", StripeCustomerID: "cus_1"})
				code, out, errb := billingCmd(t, billingTestDeps(api, store), cmd, v.name, "-nosuchflag")
				rejected(t, code, out, errb)
				if len(api.prices) != 1 || len(api.products)+len(api.updates)+len(api.sessions)+len(api.portals) != 0 {
					t.Errorf("reached Stripe: %d prices, %d products, %d updates, %d sessions, %d portals",
						len(api.prices), len(api.products), len(api.updates), len(api.sessions), len(api.portals))
				}
				if len(store.upserts)+len(store.invites)+store.notified != 0 {
					t.Errorf("reached the store: %d upserts, %d invites, notified %d", len(store.upserts), len(store.invites), store.notified)
				}
			})
		}
	}
}
