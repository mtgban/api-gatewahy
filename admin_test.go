package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

type memStore struct {
	accounts  []apiaccess.Account
	keys      []apiaccess.Key
	ents      []apiaccess.Entitlement
	notified  int
	notifyErr error
	audit     []string
}

func (m *memStore) CreateAccount(_ context.Context, email, note string) (apiaccess.Account, error) {
	a := apiaccess.Account{ID: int64(len(m.accounts) + 1), Email: apiaccess.NormalizeEmail(email), Status: "active", Note: note}
	m.accounts = append(m.accounts, a)
	return a, nil
}
func (m *memStore) GetAccountByEmail(_ context.Context, email string) (apiaccess.Account, error) {
	for _, a := range m.accounts {
		if a.Email == apiaccess.NormalizeEmail(email) {
			return a, nil
		}
	}
	return apiaccess.Account{}, apiaccess.ErrNotFound
}
func (m *memStore) SetAccountStatus(_ context.Context, id int64, status string) error {
	for i := range m.accounts {
		if m.accounts[i].ID == id {
			m.accounts[i].Status = status
			return nil
		}
	}
	return apiaccess.ErrNotFound
}
func (m *memStore) ListAccounts(context.Context) ([]apiaccess.Account, error) { return m.accounts, nil }
func (m *memStore) CreateKey(_ context.Context, accountID int64, label string, kind apiaccess.KeyKind) (string, apiaccess.Key, error) {
	plain, hash, prefix, _ := apiaccess.GenerateKey(kind)
	k := apiaccess.Key{ID: int64(len(m.keys) + 1), AccountID: accountID, Hash: hash, Prefix: prefix, Label: label, Kind: kind}
	m.keys = append(m.keys, k)
	return plain, k, nil
}
func (m *memStore) RevokeKeyByPrefix(_ context.Context, prefix string) (apiaccess.Key, error) {
	for i := range m.keys {
		if m.keys[i].Prefix == prefix && m.keys[i].RevokedAt == nil {
			t := time.Now()
			m.keys[i].RevokedAt = &t
			return m.keys[i], nil
		}
	}
	return apiaccess.Key{}, apiaccess.ErrNotFound
}
func (m *memStore) ListKeys(_ context.Context, accountID int64) ([]apiaccess.Key, error) {
	var out []apiaccess.Key
	for _, k := range m.keys {
		if k.AccountID == accountID {
			out = append(out, k)
		}
	}
	return out, nil
}
func (m *memStore) AddEntitlement(_ context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error) {
	e.ID = int64(len(m.ents) + 1)
	e.Status = "active"
	m.ents = append(m.ents, e)
	return e, nil
}
func (m *memStore) EndEntitlement(_ context.Context, id int64, at time.Time) error {
	for i := range m.ents {
		if m.ents[i].ID == id {
			m.ents[i].Status = "ended"
			m.ents[i].ValidUntil = &at
			return nil
		}
	}
	return apiaccess.ErrNotFound
}
func (m *memStore) ListEntitlements(_ context.Context, accountID int64) ([]apiaccess.Entitlement, error) {
	var out []apiaccess.Entitlement
	for _, e := range m.ents {
		if e.AccountID == accountID {
			out = append(out, e)
		}
	}
	return out, nil
}
func (m *memStore) SummarizeUsage(context.Context, time.Time, time.Time, int64) ([]apiaccess.UsageRow, error) {
	return []apiaccess.UsageRow{{Email: "ck@example.com", Game: "magic", Requests: 12, Bytes: 3456, Errors: 1}}, nil
}
func (m *memStore) Notify(context.Context, string) error { m.notified++; return m.notifyErr }

func (m *memStore) RecordAdminAction(_ context.Context, actor, action string, accountID int64, target, detail string) error {
	if !strings.HasPrefix(actor, "cli") {
		return errors.New("actor must name the cli")
	}
	m.audit = append(m.audit, action+" "+target)
	return nil
}

func admin(t *testing.T, store adminStore, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runAdmin(context.Background(), store, []string{"magic", "pokemon"}, args[0], args[1:], &out, &errb)
	return code, out.String(), errb.String()
}

func TestAdminAccountLifecycle(t *testing.T) {
	s := &memStore{}
	if code, out, errb := admin(t, s, "account", "add", "-email", "CK@Example.com", "-note", "zoho 12"); code != 0 || !strings.Contains(out, "ck@example.com") {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	if code, _, _ := admin(t, s, "account", "suspend", "-email", "ck@example.com"); code != 0 || s.accounts[0].Status != "suspended" {
		t.Fatalf("suspend: %d %+v", code, s.accounts)
	}
	if code, _, _ := admin(t, s, "account", "reinstate", "-email", "ck@example.com"); code != 0 || s.accounts[0].Status != "active" {
		t.Fatalf("reinstate: %d", code)
	}
	if code, out, _ := admin(t, s, "account", "list"); code != 0 || !strings.Contains(out, "ck@example.com") {
		t.Fatalf("list: %d %q", code, out)
	}
	if code, _, errb := admin(t, s, "account", "suspend", "-email", "nobody@example.com"); code != 1 || !strings.Contains(errb, "not found") {
		t.Fatalf("missing: %d %q", code, errb)
	}
	if s.notified != 2 {
		t.Errorf("notified %d times, want 2 (suspend, reinstate)", s.notified)
	}
	if len(s.audit) != 2 || s.audit[0] != "status " || s.audit[1] != "status " {
		t.Errorf("audit %v, want the suspend and the reinstate", s.audit)
	}
}

func TestAdminWarnsOnNotifyFailure(t *testing.T) {
	s := &memStore{notifyErr: errors.New("listener gone")}
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
	s := &memStore{}
	admin(t, s, "account", "add", "-email", "ck@example.com")
	code, out, errb := admin(t, s, "key", "create", "-email", "ck@example.com", "-label", "prod")
	if code != 0 || !strings.Contains(out, "ban_demo_") || !strings.Contains(out, "(ban_demo, prefix") {
		t.Fatalf("create: %d %q %q", code, out, errb)
	}
	prefix := s.keys[0].Prefix
	// The kind column prints ban_demo; only a leaked plaintext carries the underscore.
	code, out, _ = admin(t, s, "key", "list", "-email", "ck@example.com")
	if code != 0 || !strings.Contains(out, prefix) || strings.Contains(out, "ban_demo_") {
		t.Fatalf("list leaked or missed: %d %q", code, out)
	}
	if !strings.Contains(out, "ban_demo") {
		t.Errorf("list does not show the key kind: %q", out)
	}
	if code, _, _ := admin(t, s, "key", "revoke", "-prefix", prefix); code != 0 || s.keys[0].RevokedAt == nil {
		t.Fatalf("revoke: %d", code)
	}
	if s.notified != 2 {
		t.Errorf("notified %d, want 2 (create, revoke)", s.notified)
	}
	if len(s.audit) != 2 || !strings.HasPrefix(s.audit[0], "key create ") || !strings.HasPrefix(s.audit[1], "key revoke ") {
		t.Errorf("audit %v, want the create and the revoke", s.audit)
	}
}

func TestAdminKeyCreateIsLiveOnAStripePlan(t *testing.T) {
	s := &memStore{}
	admin(t, s, "account", "add", "-email", "ck@example.com")
	s.ents = append(s.ents, apiaccess.Entitlement{ID: 1, AccountID: s.accounts[0].ID, Source: "stripe", Status: "active",
		Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail"}, ValidFrom: time.Now().Add(-time.Hour)})
	code, out, errb := admin(t, s, "key", "create", "-email", "ck@example.com", "-label", "prod")
	if code != 0 || !strings.Contains(out, "ban_live_") || !strings.Contains(out, "(ban_live, prefix") {
		t.Fatalf("create: %d %q %q", code, out, errb)
	}
	if len(s.keys) != 1 || s.keys[0].Kind != apiaccess.KeyLive {
		t.Errorf("stored keys %+v, want one live key", s.keys)
	}
}

func TestAdminGrants(t *testing.T) {
	s := &memStore{}
	admin(t, s, "account", "add", "-email", "ck@example.com")
	code, out, errb := admin(t, s, "grant", "add", "-email", "ck@example.com", "-games", "magic, pokemon",
		"-stores", "CK,TCG", "-modes", "buylist,retail", "-until", "2027-01-01", "-note", "annual")
	if code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	e := s.ents[0]
	if e.StoreScope != "CK,TCG" || len(e.Games) != 2 || e.Games[1] != "pokemon" || e.Modes[0] != "retail" ||
		e.ValidUntil == nil || e.ValidUntil.Year() != 2027 || e.Source != "manual" || e.Note != "annual" {
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
	if code, _, _ := admin(t, s, "grant", "end", "-id", "1"); code != 0 || s.ents[0].Status != "ended" {
		t.Errorf("end: %d %+v", code, s.ents[0])
	}
}

func TestAdminUsage(t *testing.T) {
	s := &memStore{}
	code, out, _ := admin(t, s, "usage", "-since", "2026-09-01")
	if code != 0 || !strings.Contains(out, "ck@example.com") || !strings.Contains(out, "12") {
		t.Errorf("usage: %d %q", code, out)
	}
}

func TestAdminUsageErrors(t *testing.T) {
	s := &memStore{}
	if code, _, _ := admin(t, s, "account", "add"); code != 2 {
		t.Errorf("missing -email should be usage error, got %d", code)
	}
	if code, _, _ := admin(t, s, "account", "frobnicate"); code != 2 {
		t.Errorf("unknown verb should be usage error, got %d", code)
	}
}

func TestAdminGrantRejectsUnknownGame(t *testing.T) {
	s := &memStore{}
	admin(t, s, "account", "add", "-email", "ck@example.com")
	code, _, errb := admin(t, s, "grant", "add", "-email", "ck@example.com", "-games", "magick",
		"-stores", "TCG", "-modes", "retail")
	if code != 1 || !strings.Contains(errb, "magick") {
		t.Errorf("unknown game: %d %q", code, errb)
	}
	if len(s.ents) != 0 {
		t.Errorf("entitlement stored anyway: %+v", s.ents)
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
