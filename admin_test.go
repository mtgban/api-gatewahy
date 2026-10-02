package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/apiaccess/apiaccesstest"
)

var (
	_ adminStore   = (*apiaccesstest.MemStore)(nil)
	_ billingStore = (*apiaccesstest.MemStore)(nil)
)

// stored reads an account back by email.
func stored(t *testing.T, s *apiaccesstest.MemStore, email string) apiaccess.Account {
	t.Helper()
	a, err := s.GetAccountByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// keysOf lists the keys of the account with email.
func keysOf(t *testing.T, s *apiaccesstest.MemStore, email string) []apiaccess.Key {
	t.Helper()
	keys, err := s.ListKeys(context.Background(), stored(t, s, email).ID)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// entsOf lists the entitlements of the account with email.
func entsOf(t *testing.T, s *apiaccesstest.MemStore, email string) []apiaccess.Entitlement {
	t.Helper()
	ents, err := s.ListEntitlements(context.Background(), stored(t, s, email).ID)
	if err != nil {
		t.Fatal(err)
	}
	return ents
}

// auditRows lists the recorded actions oldest first; every actor must name the cli.
func auditRows(t *testing.T, s *apiaccesstest.MemStore) []apiaccess.AdminAction {
	t.Helper()
	acts, err := s.ListAdminActions(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(acts)
	for _, a := range acts {
		if !strings.HasPrefix(a.Actor, "cli") {
			t.Errorf("actor %q does not name the cli", a.Actor)
		}
	}
	return acts
}

// auditLog is auditRows as "action target" strings.
func auditLog(t *testing.T, s *apiaccesstest.MemStore) []string {
	t.Helper()
	var out []string
	for _, a := range auditRows(t, s) {
		out = append(out, a.Action+" "+a.Target)
	}
	return out
}

func admin(t *testing.T, store adminStore, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runAdmin(context.Background(), store, []string{"magic", "pokemon"}, args[0], args[1:], &out, &errb)
	return code, out.String(), errb.String()
}

func TestAdminAccountLifecycle(t *testing.T) {
	s := apiaccesstest.New()
	if code, out, errb := admin(t, s, "account", "add", "-email", "CK@Example.com", "-note", "zoho 12"); code != 0 || !strings.Contains(out, "ck@example.com") {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	if code, _, _ := admin(t, s, "account", "suspend", "-email", "ck@example.com"); code != 0 || stored(t, s, "ck@example.com").Status != apiaccess.AccountSuspended {
		t.Fatalf("suspend: %d %+v", code, stored(t, s, "ck@example.com"))
	}
	if code, _, _ := admin(t, s, "account", "reinstate", "-email", "ck@example.com"); code != 0 || stored(t, s, "ck@example.com").Status != apiaccess.AccountActive {
		t.Fatalf("reinstate: %d", code)
	}
	if code, out, _ := admin(t, s, "account", "list"); code != 0 || !strings.Contains(out, "ck@example.com") {
		t.Fatalf("list: %d %q", code, out)
	}
	if code, _, errb := admin(t, s, "account", "suspend", "-email", "nobody@example.com"); code != 1 || !strings.Contains(errb, "not found") {
		t.Fatalf("missing: %d %q", code, errb)
	}
	if len(s.Notified) != 2 {
		t.Errorf("notified %d times, want 2 (suspend, reinstate)", len(s.Notified))
	}
	if got := auditLog(t, s); len(got) != 3 || !strings.HasPrefix(got[0], "account add") || got[1] != "status " || got[2] != "status " {
		t.Errorf("audit %v, want the add, the suspend and the reinstate", got)
	}
}

func TestAdminWarnsOnNotifyFailure(t *testing.T) {
	s := apiaccesstest.New()
	s.Fail["Notify"] = errors.New("listener gone")
	admin(t, s, "account", "add", "-email", "ck@example.com")
	code, out, errb := admin(t, s, "account", "suspend", "-email", "ck@example.com")
	if code != 0 {
		t.Fatalf("exit %d, want 0: %q", code, errb)
	}
	if !strings.Contains(out, "is now suspended") {
		t.Errorf("stdout %q", out)
	}
	want := "warning: cache reload notification failed (listener gone); gateways apply the change within cache_ttl_seconds"
	if !strings.Contains(errb, want) {
		t.Errorf("stderr %q, want it to contain %q", errb, want)
	}
}

func TestAdminKeys(t *testing.T) {
	s := apiaccesstest.New()
	admin(t, s, "account", "add", "-email", "ck@example.com")
	code, out, errb := admin(t, s, "key", "create", "-email", "ck@example.com", "-label", "prod")
	if code != 0 || !strings.Contains(out, "ban_demo_") || !strings.Contains(out, "(ban_demo, prefix") {
		t.Fatalf("create: %d %q %q", code, out, errb)
	}
	prefix := keysOf(t, s, "ck@example.com")[0].Prefix
	// The kind column prints ban_demo; only a leaked plaintext carries the underscore.
	code, out, _ = admin(t, s, "key", "list", "-email", "ck@example.com")
	if code != 0 || !strings.Contains(out, prefix) || strings.Contains(out, "ban_demo_") {
		t.Fatalf("list leaked or missed: %d %q", code, out)
	}
	if !strings.Contains(out, "ban_demo") {
		t.Errorf("list does not show the key kind: %q", out)
	}
	if code, _, _ := admin(t, s, "key", "revoke", "-prefix", prefix); code != 0 || keysOf(t, s, "ck@example.com")[0].RevokedAt == nil {
		t.Fatalf("revoke: %d", code)
	}
	if len(s.Notified) != 2 {
		t.Errorf("notified %d, want 2 (create, revoke)", len(s.Notified))
	}
	if got := auditLog(t, s); len(got) != 3 || !strings.HasPrefix(got[0], "account add") || !strings.HasPrefix(got[1], "key create ") || !strings.HasPrefix(got[2], "key revoke ") {
		t.Errorf("audit %v, want the add, the create and the revoke", got)
	}
}

func TestAdminKeyCreateIsLiveOnAStripePlan(t *testing.T) {
	s := apiaccesstest.New()
	admin(t, s, "account", "add", "-email", "ck@example.com")
	if _, err := s.AddEntitlement(context.Background(), apiaccess.Entitlement{AccountID: stored(t, s, "ck@example.com").ID, Source: apiaccess.SourceStripe,
		Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail"}, ValidFrom: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	code, out, errb := admin(t, s, "key", "create", "-email", "ck@example.com", "-label", "prod")
	if code != 0 || !strings.Contains(out, "ban_live_") || !strings.Contains(out, "(ban_live, prefix") {
		t.Fatalf("create: %d %q %q", code, out, errb)
	}
	if keys := keysOf(t, s, "ck@example.com"); len(keys) != 1 || keys[0].Kind != apiaccess.KeyLive {
		t.Errorf("stored keys %+v, want one live key", keys)
	}
}

func TestAdminGrants(t *testing.T) {
	s := apiaccesstest.New()
	admin(t, s, "account", "add", "-email", "ck@example.com")
	code, out, errb := admin(t, s, "grant", "add", "-email", "ck@example.com", "-games", "magic, pokemon",
		"-stores", "CK,TCG", "-modes", "buylist,retail", "-until", "2027-01-01", "-note", "annual")
	if code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	e := entsOf(t, s, "ck@example.com")[0]
	if e.StoreScope != "CK,TCG" || len(e.Games) != 2 || e.Games[1] != "pokemon" || e.Modes[0] != "retail" ||
		e.ValidUntil == nil || e.ValidUntil.Year() != 2027 || e.Source != apiaccess.SourceManual || e.Note != "annual" {
		t.Errorf("grant %+v", e)
	}
	if code, _, errb := admin(t, s, "grant", "add", "-email", "ck@example.com", "-games", "magic", "-stores", "DEV_ACCESS", "-modes", "retail"); code != 1 || !strings.Contains(errb, "DEV_ACCESS") {
		t.Errorf("dev access: %d %q", code, errb)
	}
	if code, _, errb := admin(t, s, "grant", "add", "-email", "ck@example.com", "-games", "magic", "-stores", "TCG CK", "-modes", "retail"); code != 1 || !strings.Contains(errb, "TCG CK") {
		t.Errorf("missing comma: %d %q", code, errb)
	}
	if code, _, errb := admin(t, s, "grant", "add", "-email", "ck@example.com", "-games", "magic", "-stores", "TCG", "-modes", "all"); code != 1 || !strings.Contains(errb, "all") {
		t.Errorf("mode all: %d %q", code, errb)
	}
	if code, out, _ := admin(t, s, "grant", "list", "-email", "ck@example.com"); code != 0 || !strings.Contains(out, "CK,TCG") {
		t.Errorf("list: %d %q", code, out)
	}
	if code, _, _ := admin(t, s, "grant", "end", "-id", strconv.FormatInt(e.ID, 10)); code != 0 || entsOf(t, s, "ck@example.com")[0].Status != apiaccess.EntitlementEnded {
		t.Errorf("end: %d %+v", code, entsOf(t, s, "ck@example.com"))
	}
}

func TestAdminAccountAddIsAudited(t *testing.T) {
	s := apiaccesstest.New()
	if code, _, errb := admin(t, s, "account", "add", "-email", "ck@example.com"); code != 0 {
		t.Fatalf("add: %d %q", code, errb)
	}
	acts := auditRows(t, s)
	if len(acts) != 1 || acts[0].Action != "account add" {
		t.Fatalf("audit %+v, want account add logged", acts)
	}
	if want := stored(t, s, "ck@example.com").ID; acts[0].AccountID != want {
		t.Errorf("audit account %d, want %d", acts[0].AccountID, want)
	}
}

func TestAdminGrantEndAuditsTheRealAccount(t *testing.T) {
	s := apiaccesstest.New()
	// A throwaway account first keeps the granted account's id (2) different
	// from the entitlement's id (1), so auditing the wrong one still fails.
	admin(t, s, "account", "add", "-email", "other@example.com")
	admin(t, s, "account", "add", "-email", "ck@example.com")
	admin(t, s, "grant", "add", "-email", "ck@example.com", "-games", "magic", "-stores", "TCG", "-modes", "retail")
	if code, _, errb := admin(t, s, "grant", "end", "-id", "1"); code != 0 {
		t.Fatalf("end: %d %q", code, errb)
	}
	acts := auditRows(t, s)
	if got, want := acts[len(acts)-1].AccountID, stored(t, s, "ck@example.com").ID; got != want {
		t.Errorf("grant end audited account %d, want %d", got, want)
	}
}

func TestAdminGrantEndRefusesStripeRow(t *testing.T) {
	s := apiaccesstest.New()
	admin(t, s, "account", "add", "-email", "ck@example.com")
	if _, err := s.UpsertStripeEntitlement(context.Background(), apiaccess.Entitlement{AccountID: stored(t, s, "ck@example.com").ID, Source: "stripe",
		Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail"}, ExternalRef: "sub_1"}); err != nil {
		t.Fatal(err)
	}
	auditsBefore, notifiedBefore := len(auditLog(t, s)), len(s.Notified)
	code, _, errb := admin(t, s, "grant", "end", "-id", "1")
	if code != 1 || !strings.Contains(strings.ToLower(errb), "cancel") || !strings.Contains(errb, "Stripe") || !strings.Contains(strings.ToLower(errb), "suspend") {
		t.Errorf("end stripe row: %d %q", code, errb)
	}
	if ents := entsOf(t, s, "ck@example.com"); ents[0].Status != "active" {
		t.Errorf("stripe row ended: %+v", ents[0])
	}
	if got := auditLog(t, s); len(got) != auditsBefore || len(s.Notified) != notifiedBefore {
		t.Errorf("stripe refusal audited or notified: %v, notified %d", got, len(s.Notified))
	}
}

func TestAdminGrantEndSecondCallIsNotAudited(t *testing.T) {
	s := apiaccesstest.New()
	admin(t, s, "account", "add", "-email", "ck@example.com")
	admin(t, s, "grant", "add", "-email", "ck@example.com", "-games", "magic", "-stores", "TCG", "-modes", "retail")
	admin(t, s, "grant", "end", "-id", "1")
	auditsBefore := len(auditLog(t, s))
	if code, _, errb := admin(t, s, "grant", "end", "-id", "1"); code != 1 || !strings.Contains(errb, "not found") {
		t.Errorf("second end: %d %q", code, errb)
	}
	if got := auditLog(t, s); len(got) != auditsBefore {
		t.Errorf("second end audited again: %v", got)
	}
}

func TestAdminUsage(t *testing.T) {
	s := apiaccesstest.New()
	a, _ := s.CreateAccount(context.Background(), "ck@example.com", "")
	rows := make([]apiaccess.Usage, 12)
	for i := range rows {
		rows[i] = apiaccess.Usage{Ts: time.Now().Add(-time.Hour), AccountID: a.ID, Game: "magic", Path: "/sets.json", Status: 200, Bytes: 288}
	}
	rows[0].Status = 500
	if err := s.InsertUsage(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	code, out, _ := admin(t, s, "usage", "-since", "2026-09-01")
	if code != 0 || !strings.Contains(out, "ck@example.com") || !strings.Contains(out, "12") {
		t.Errorf("usage: %d %q", code, out)
	}
}

func TestAdminUsageErrors(t *testing.T) {
	s := apiaccesstest.New()
	if code, _, _ := admin(t, s, "account", "add"); code != 2 {
		t.Errorf("missing -email should be usage error, got %d", code)
	}
	if code, _, _ := admin(t, s, "account", "frobnicate"); code != 2 {
		t.Errorf("unknown verb should be usage error, got %d", code)
	}
}

func TestAdminGrantRejectsUnknownGame(t *testing.T) {
	s := apiaccesstest.New()
	admin(t, s, "account", "add", "-email", "ck@example.com")
	code, _, errb := admin(t, s, "grant", "add", "-email", "ck@example.com", "-games", "magick",
		"-stores", "TCG", "-modes", "retail")
	if code != 1 || !strings.Contains(errb, "magick") {
		t.Errorf("unknown game: %d %q", code, errb)
	}
	if ents := entsOf(t, s, "ck@example.com"); len(ents) != 0 {
		t.Errorf("entitlement stored anyway: %+v", ents)
	}
}

func TestAdminConfigFlagAnywhere(t *testing.T) {
	cases := []struct {
		name string
		args []string
		rest []string
	}{
		{"before the verb", []string{"-config", "x.json", "add", "-email", "y"}, []string{"add", "-email", "y"}},
		{"after the verb", []string{"add", "-config", "x.json", "-email", "y"}, []string{"add", "-email", "y"}},
		{"at the end", []string{"add", "-email", "y", "--config", "x.json"}, []string{"add", "-email", "y"}},
		{"joined value", []string{"add", "-config=x.json", "-email", "y"}, []string{"add", "-email", "y"}},
	}
	for _, c := range cases {
		path, rest := extractConfigFlag(c.args)
		if path != "x.json" || !slices.Equal(rest, c.rest) {
			t.Errorf("%s: path %q rest %v", c.name, path, rest)
		}
	}
	if path, rest := extractConfigFlag([]string{"add", "-email", "y"}); path != "" || len(rest) != 3 {
		t.Errorf("absent: path %q rest %v", path, rest)
	}
}

func TestAdminGrantRejectsPastUntil(t *testing.T) {
	s := apiaccesstest.New()
	admin(t, s, "account", "add", "-email", "ck@example.com")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	code, _, errb := admin(t, s, "grant", "add", "-email", "ck@example.com", "-games", "magic",
		"-stores", "TCG", "-modes", "retail", "-until", yesterday)
	if code != 1 || !strings.Contains(errb, "until must be in the future") {
		t.Errorf("past until: %d %q", code, errb)
	}
	if ents := entsOf(t, s, "ck@example.com"); len(ents) != 0 {
		t.Errorf("entitlement stored anyway: %+v", ents)
	}
}

func TestAdminUsageRejectsSinceAfterUntil(t *testing.T) {
	s := apiaccesstest.New()
	code, _, errb := admin(t, s, "usage", "-since", "2026-02-01", "-until", "2026-01-01")
	if code != 2 || !strings.Contains(errb, "since must not be after until") {
		t.Errorf("since after until: %d %q", code, errb)
	}
	if n := s.Calls["SummarizeUsage"]; n != 0 {
		t.Errorf("store was called despite the bad range: %d calls", n)
	}
}
