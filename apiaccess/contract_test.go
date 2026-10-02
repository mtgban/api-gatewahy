package apiaccess_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/apiaccess/apiaccesstest"
)

// store is the surface both the client and the in-memory store offer.
type store interface {
	CreateAccount(ctx context.Context, email, note string) (apiaccess.Account, error)
	GetAccount(ctx context.Context, id int64) (apiaccess.Account, error)
	GetAccountByEmail(ctx context.Context, email string) (apiaccess.Account, error)
	GetOrCreateAccount(ctx context.Context, email, note string) (apiaccess.Account, error)
	SetAccountStatus(ctx context.Context, id int64, status apiaccess.AccountStatus) error
	SetAccountNote(ctx context.Context, id int64, note string) error
	SetStripeCustomerID(ctx context.Context, accountID int64, customerID string) (string, error)
	GetAccountByStripeCustomer(ctx context.Context, customerID string) (apiaccess.Account, error)
	SearchAccounts(ctx context.Context, q string) ([]apiaccess.Account, error)
	ListAccounts(ctx context.Context) ([]apiaccess.Account, error)
	BumpSessionEpoch(ctx context.Context, accountID int64) (int64, error)
	CreateKey(ctx context.Context, accountID int64, label string, kind apiaccess.KeyKind) (string, apiaccess.Key, error)
	RevokeKey(ctx context.Context, id, accountID int64) (apiaccess.Key, error)
	RevokeKeyByPrefix(ctx context.Context, prefix string) (apiaccess.Key, error)
	ListKeys(ctx context.Context, accountID int64) ([]apiaccess.Key, error)
	AddEntitlement(ctx context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error)
	UpsertStripeEntitlement(ctx context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error)
	ListActiveStripeRefs(ctx context.Context) ([]apiaccess.StripeRef, error)
	EndEntitlement(ctx context.Context, id, accountID int64, at time.Time) (apiaccess.Entitlement, error)
	ListEntitlements(ctx context.Context, accountID int64) ([]apiaccess.Entitlement, error)
	InsertUsage(ctx context.Context, rows []apiaccess.Usage) error
	SummarizeUsage(ctx context.Context, since, until time.Time, accountID int64) ([]apiaccess.UsageRow, error)
	UsageByKey(ctx context.Context, since, until time.Time, accountID int64) ([]apiaccess.KeyUsageRow, error)
	TopPaths(ctx context.Context, since, until time.Time, keyID int64, limit int) ([]apiaccess.PathUsageRow, error)
	CreateTrial(ctx context.Context, email string, endsAt, notBefore time.Time, ent apiaccess.Entitlement) (apiaccess.Trial, error)
	LastTrial(ctx context.Context, email string) (apiaccess.Trial, error)
	TrialsToRemind(ctx context.Context, from, to time.Time) ([]apiaccess.Trial, error)
	MarkTrialReminded(ctx context.Context, id int64, at time.Time) error
	CreateInvite(ctx context.Context, intervalKey, email string, ttl time.Duration, note string) (string, apiaccess.Invite, error)
	ConsumeInvite(ctx context.Context, token, email string, now time.Time) (apiaccess.Invite, error)
	ReleaseInvite(ctx context.Context, token string) error
	CreateMagicLink(ctx context.Context, accountID int64, ttl time.Duration) (string, error)
	ConsumeMagicLink(ctx context.Context, token string, now time.Time) (apiaccess.Account, error)
	DeleteMagicLink(ctx context.Context, token string) error
	BeginStripeEvent(ctx context.Context, id, typ string) (bool, error)
	FinishStripeEvent(ctx context.Context, id string) error
	DeleteStripeEvent(ctx context.Context, id string) error
	ConsumeNonce(ctx context.Context, nonce string, expiresAt, now time.Time) error
	RecordAdminAction(ctx context.Context, actor, action string, accountID int64, target, detail string) error
	ListAdminActions(ctx context.Context, accountID int64, limit int) ([]apiaccess.AdminAction, error)
	ListDemoAccess(ctx context.Context) ([]apiaccess.DemoAccess, error)
}

var (
	_ store = (*apiaccess.Client)(nil)
	_ store = (*apiaccesstest.MemStore)(nil)
)

// contractScenarios each run once against memory and once against Postgres.
var contractScenarios = []struct {
	name string
	run  func(t *testing.T, s store)
}{
	{"add entitlement canonicalizes scope and modes", contractCanonicalEntitlement},
	{"upsert keeps valid_from", contractUpsertKeepsValidFrom},
	{"stripe customer needs an account", contractStripeCustomer},
	{"usage summaries group like the sql", contractUsageGrouping},
	{"list accounts orders by id", contractListAccountsOrder},
	{"trials to remind order by ends_at", contractTrialsToRemindOrder},
	{"create trial takes granted_at from the clock", contractTrialGrantedAt},
	{"create trial writes the trial and its grant or neither", contractTrialAtomic},
	{"end refuses a stripe row", contractEndRefusesStripe},
	{"a second end is not found", contractEndTwice},
	{"accounts round trip", contractAccounts},
	{"keys round trip", contractKeys},
	{"entitlements round trip", contractEntitlements},
	{"invites round trip", contractInvites},
	{"magic links round trip", contractMagicLinks},
	{"stripe events round trip", contractStripeEvents},
	{"nonces round trip", contractNonces},
	{"admin actions round trip", contractAdminActions},
	{"demo access lists trial and manual grants", contractDemoAccess},
}

func TestContract(t *testing.T) {
	for _, sc := range contractScenarios {
		t.Run(sc.name+"/memory", func(t *testing.T) { sc.run(t, apiaccesstest.New()) })
		t.Run(sc.name+"/postgres", func(t *testing.T) { sc.run(t, postgresStore(t)) })
	}
}

// postgresStore opens APIACCESS_TEST_DSN empty, or skips, and empties it again after.
func postgresStore(t *testing.T) *apiaccess.Client {
	t.Helper()
	dsn := os.Getenv("APIACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("APIACCESS_TEST_DSN not set")
	}
	c, err := apiaccess.OpenDSN(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	apiaccesstest.ResetPostgres(t, dsn)
	t.Cleanup(func() {
		apiaccesstest.ResetPostgres(t, dsn)
		_ = c.Close()
	})
	return c
}

var ctx = context.Background()

// missing is an id no fresh table holds.
const missing = 1 << 40

func mustAccount(t *testing.T, s store, email string) apiaccess.Account {
	t.Helper()
	a, err := s.CreateAccount(ctx, email, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// near reports whether got falls within a minute of [before, after]; Postgres stamps its own now().
func near(got, before, after time.Time) bool {
	return !got.Before(before.Add(-time.Minute)) && !got.After(after.Add(time.Minute))
}

// sameMicro reports whether a stored time is t at the microsecond Postgres keeps.
func sameMicro(stored, t time.Time) bool {
	return stored.Sub(t).Abs() <= time.Microsecond
}

func grant(accountID int64, source apiaccess.Source, scope string, modes ...string) apiaccess.Entitlement {
	return apiaccess.Entitlement{AccountID: accountID, Source: source, Games: []string{"magic"}, StoreScope: scope, Modes: modes}
}

// trialGrant is the entitlement a trial for the account writes.
func trialGrant(accountID int64) apiaccess.Entitlement {
	return grant(accountID, "trial", "ALL_ACCESS", "retail")
}

func contractCanonicalEntitlement(t *testing.T, s store) {
	a := mustAccount(t, s, "canon@example.com")
	before := time.Now()
	e, err := s.AddEntitlement(ctx, grant(a.ID, apiaccess.SourceManual, "base_access", "Retail", "retail", " SEALED"))
	if err != nil {
		t.Fatal(err)
	}
	if e.StoreScope != "BASE_ACCESS" || !slices.Equal(e.Modes, []string{"retail", "sealed"}) || e.Status != apiaccess.EntitlementActive ||
		e.Addons == nil || len(e.Addons) != 0 || !near(e.ValidFrom, before, time.Now()) {
		t.Errorf("preset row %+v", e)
	}
	if _, err := s.AddEntitlement(ctx, grant(a.ID, apiaccess.SourceManual, " TCG, CK,TCG ", "buylist")); err != nil {
		t.Fatal(err)
	}
	ents, err := s.ListEntitlements(ctx, a.ID)
	if err != nil || len(ents) != 2 {
		t.Fatalf("list %+v %v", ents, err)
	}
	if ents[0].ID != e.ID || ents[0].StoreScope != "BASE_ACCESS" || !slices.Equal(ents[0].Modes, []string{"retail", "sealed"}) ||
		ents[1].StoreScope != "CK,TCG" || !slices.Equal(ents[1].Modes, []string{"buylist"}) {
		t.Errorf("listed %+v", ents)
	}
	for name, bad := range map[string]apiaccess.Entitlement{
		"dev access": grant(a.ID, apiaccess.SourceManual, "DEV_ACCESS", "retail"),
		"mode all":   grant(a.ID, apiaccess.SourceManual, "ALL_ACCESS", "all"),
		"no games":   {AccountID: a.ID, Source: apiaccess.SourceManual, StoreScope: "ALL_ACCESS", Modes: []string{"retail"}},
		"no account": grant(missing, apiaccess.SourceManual, "ALL_ACCESS", "retail"),
	} {
		if _, err := s.AddEntitlement(ctx, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func contractUpsertKeepsValidFrom(t *testing.T, s store) {
	a := mustAccount(t, s, "upsert@example.com")
	first := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	e := grant(a.ID, apiaccess.SourceStripe, "all_access", "retail")
	e.ExternalRef, e.ValidFrom = "sub_contract", first
	e1, err := s.UpsertStripeEntitlement(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	until := first.AddDate(0, 2, 0)
	e.ValidFrom, e.Status, e.ValidUntil, e.Modes, e.Note = first.AddDate(0, 1, 0), apiaccess.EntitlementEnded, &until, []string{"buylist"}, "changed"
	e2, err := s.UpsertStripeEntitlement(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if e2.ID != e1.ID || !e2.ValidFrom.Equal(first) || e2.Status != apiaccess.EntitlementEnded || e2.ValidUntil == nil || !e2.ValidUntil.Equal(until) ||
		!slices.Equal(e2.Modes, []string{"buylist"}) || e2.Note != "changed" || e2.StoreScope != "ALL_ACCESS" {
		t.Errorf("second upsert %+v, first id %d", e2, e1.ID)
	}
	if ents, _ := s.ListEntitlements(ctx, a.ID); len(ents) != 1 || !ents[0].ValidFrom.Equal(first) {
		t.Errorf("rows %+v", ents)
	}
	e.ExternalRef = ""
	if _, err := s.UpsertStripeEntitlement(ctx, e); err == nil {
		t.Error("upsert without external_ref accepted")
	}
	dup := grant(a.ID, apiaccess.SourceStripe, "ALL_ACCESS", "retail")
	dup.ExternalRef = "sub_contract"
	if _, err := s.AddEntitlement(ctx, dup); err == nil {
		t.Error("a second row for one external_ref accepted")
	}
	for _, ref := range []string{"sub_b", "sub_a"} {
		live := grant(a.ID, apiaccess.SourceStripe, "ALL_ACCESS", "retail")
		live.ExternalRef = ref
		if _, err := s.UpsertStripeEntitlement(ctx, live); err != nil {
			t.Fatal(err)
		}
	}
	manual := grant(a.ID, apiaccess.SourceManual, "ALL_ACCESS", "retail")
	manual.ExternalRef = "manual_ref"
	if _, err := s.AddEntitlement(ctx, manual); err != nil {
		t.Fatal(err)
	}
	if refs, err := s.ListActiveStripeRefs(ctx); err != nil || !slices.Equal(refs, []apiaccess.StripeRef{{AccountID: a.ID, SubID: "sub_a"}, {AccountID: a.ID, SubID: "sub_b"}}) {
		t.Errorf("active refs %v %v", refs, err)
	}
}

func contractStripeCustomer(t *testing.T, s store) {
	a := mustAccount(t, s, "cust@example.com")
	b := mustAccount(t, s, "nocust@example.com")
	if _, err := s.SetStripeCustomerID(ctx, missing, "cus_ghost"); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("unknown account: %v", err)
	}
	if _, err := s.GetAccountByStripeCustomer(ctx, "cus_ghost"); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("unknown account was created: %v", err)
	}
	if got, err := s.SetStripeCustomerID(ctx, a.ID, "cus_1"); err != nil || got != "cus_1" {
		t.Errorf("first: %q %v", got, err)
	}
	if got, err := s.SetStripeCustomerID(ctx, a.ID, "cus_2"); err != nil || got != "cus_1" {
		t.Errorf("second: %q %v", got, err)
	}
	if got, err := s.GetAccountByStripeCustomer(ctx, "cus_1"); err != nil || got.ID != a.ID || got.StripeCustomerID != "cus_1" {
		t.Errorf("by customer: %+v %v", got, err)
	}
	if _, err := s.GetAccountByStripeCustomer(ctx, ""); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("empty customer matched an account: %v", err)
	}
	if _, err := s.SetStripeCustomerID(ctx, b.ID, "cus_1"); err == nil {
		t.Error("one customer stored on two accounts")
	}
}

func contractUsageGrouping(t *testing.T, s store) {
	zed := mustAccount(t, s, "zed@example.com")
	amy := mustAccount(t, s, "amy@example.com")
	_, zk, _ := s.CreateKey(ctx, zed.ID, "z", apiaccess.KeyLive)
	_, ak, _ := s.CreateKey(ctx, amy.ID, "a", apiaccess.KeyLive)
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	since, until := base.Add(-24*time.Hour), base.Add(24*time.Hour)
	use := func(acct apiaccess.Account, key apiaccess.Key, game, path string, status int, bytes int64, ts time.Time) apiaccess.Usage {
		return apiaccess.Usage{Ts: ts, KeyID: key.ID, AccountID: acct.ID, Game: game, Path: path, Status: status, Bytes: bytes, ClientIP: "203.0.113.9"}
	}
	rows := []apiaccess.Usage{
		use(amy, ak, "magic", "/a", 200, 100, base),
		use(amy, ak, "magic", "/a", 404, 10, base.Add(time.Minute)),
		use(amy, ak, "magic", "/b", 500, 0, base.Add(2*time.Minute)),
		use(amy, ak, "pokemon", "/c", 200, 5, base.Add(3*time.Minute)),
		use(zed, zk, "magic", "/z", 200, 1, base),
		use(zed, zk, "magic", "/z", 200, 1, base.Add(13*time.Hour)),
		use(amy, ak, "magic", "/old", 200, 1000, since.Add(-time.Second)),
		use(amy, ak, "magic", "/late", 200, 1000, until),
	}
	if err := s.InsertUsage(ctx, rows); err != nil {
		t.Fatal(err)
	}
	got, err := s.SummarizeUsage(ctx, since, until, 0)
	want := []apiaccess.UsageRow{
		{AccountID: amy.ID, Email: "amy@example.com", Game: "magic", Requests: 3, Bytes: 110, Errors: 2},
		{AccountID: amy.ID, Email: "amy@example.com", Game: "pokemon", Requests: 1, Bytes: 5},
		{AccountID: zed.ID, Email: "zed@example.com", Game: "magic", Requests: 2, Bytes: 2},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("summary\n got %+v %v\nwant %+v", got, err, want)
	}
	if got, err := s.SummarizeUsage(ctx, since, until, zed.ID); err != nil || !slices.Equal(got, want[2:]) {
		t.Errorf("zed only %+v %v", got, err)
	}
	day := func(d int) time.Time { return time.Date(2026, 3, d, 0, 0, 0, 0, time.UTC) }
	byKey, err := s.UsageByKey(ctx, since, until, 0)
	if err != nil || len(byKey) != 3 {
		t.Fatalf("by key %+v %v", byKey, err)
	}
	wantKeys := []apiaccess.KeyUsageRow{
		{KeyID: zk.ID, Prefix: zk.Prefix, Label: "z", Day: day(10), Requests: 1, Bytes: 1},
		{KeyID: zk.ID, Prefix: zk.Prefix, Label: "z", Day: day(11), Requests: 1, Bytes: 1},
		{KeyID: ak.ID, Prefix: ak.Prefix, Label: "a", Day: day(10), Requests: 4, Bytes: 115, Errors: 2},
	}
	for i, r := range byKey {
		w := wantKeys[i]
		if r.KeyID != w.KeyID || r.Prefix != w.Prefix || r.Label != w.Label || !r.Day.Equal(w.Day) || r.Requests != w.Requests || r.Bytes != w.Bytes || r.Errors != w.Errors {
			t.Errorf("by key %d: got %+v want %+v", i, r, w)
		}
	}
	paths, err := s.TopPaths(ctx, since, until, ak.ID, 0)
	wantPaths := []apiaccess.PathUsageRow{{Path: "/a", Requests: 2, Errors: 1}, {Path: "/b", Requests: 1, Errors: 1}, {Path: "/c", Requests: 1}}
	if err != nil || !slices.Equal(paths, wantPaths) {
		t.Errorf("top paths %+v %v", paths, err)
	}
	if paths, _ := s.TopPaths(ctx, since, until, ak.ID, 1); !slices.Equal(paths, wantPaths[:1]) {
		t.Errorf("top path %+v", paths)
	}
}

func contractListAccountsOrder(t *testing.T, s store) {
	for _, email := range []string{"zed-order@example.com", "amy-order@example.com", "mia-order@example.com"} {
		mustAccount(t, s, email)
	}
	all, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var emails []string
	for _, a := range all {
		emails = append(emails, a.Email)
	}
	if !slices.Equal(emails, []string{"zed-order@example.com", "amy-order@example.com", "mia-order@example.com"}) {
		t.Errorf("order %v, want creation order", emails)
	}
}

func contractTrialsToRemindOrder(t *testing.T, s store) {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	long := base.AddDate(-1, 0, 0)
	trial := func(email string, ends time.Time) apiaccess.Trial {
		a := mustAccount(t, s, email)
		tr, err := s.CreateTrial(ctx, email, ends, long, trialGrant(a.ID))
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	trial("third@example.com", base.Add(3*24*time.Hour))
	trial("first@example.com", base.Add(24*time.Hour))
	reminded := trial("reminded@example.com", base.Add(36*time.Hour))
	trial("second@example.com", base.Add(2*24*time.Hour))
	trial("outside@example.com", base.Add(9*24*time.Hour))
	if err := s.MarkTrialReminded(ctx, reminded.ID, base); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTrialReminded(ctx, missing, base); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("unknown trial: %v", err)
	}
	got, err := s.TrialsToRemind(ctx, base, base.Add(5*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var emails []string
	for _, tr := range got {
		emails = append(emails, tr.PatreonEmail)
	}
	if !slices.Equal(emails, []string{"first@example.com", "second@example.com", "third@example.com"}) {
		t.Errorf("order %v, want by ends_at", emails)
	}
}

func contractTrialGrantedAt(t *testing.T, s store) {
	a := mustAccount(t, s, "trial@example.com")
	ends := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	before := time.Now()
	first, err := s.CreateTrial(ctx, " Trial@Example.com", ends, before.Add(-time.Hour), trialGrant(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	if first.PatreonEmail != "trial@example.com" || first.AccountID != a.ID || !first.EndsAt.Equal(ends) || first.ReminderSentAt != nil ||
		!near(first.GrantedAt, before, time.Now()) {
		t.Errorf("trial %+v", first)
	}
	if _, err := s.CreateTrial(ctx, "trial@example.com", ends, before.Add(-time.Hour), trialGrant(a.ID)); !errors.Is(err, apiaccess.ErrTrialTooSoon) {
		t.Errorf("inside the cooldown: %v", err)
	}
	if ents, _ := s.ListEntitlements(ctx, a.ID); len(ents) != 1 {
		t.Errorf("a refused trial still granted: %+v", ents)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := s.CreateTrial(ctx, "trial@example.com", ends, time.Now().Add(time.Hour), trialGrant(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	if last, err := s.LastTrial(ctx, "TRIAL@example.com"); err != nil || last.ID != second.ID || last.ID == first.ID {
		t.Errorf("last %+v %v, want %d", last, err, second.ID)
	}
	if _, err := s.LastTrial(ctx, "nobody@example.com"); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("no trial: %v", err)
	}
	if _, err := s.CreateTrial(ctx, "ghost@example.com", ends, before, trialGrant(missing)); err == nil {
		t.Error("trial for an unknown account accepted")
	}
}

func contractTrialAtomic(t *testing.T, s store) {
	a := mustAccount(t, s, "atomic@example.com")
	ends := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	long := ends.AddDate(-1, 0, 0)
	noTrial := func(name string) {
		t.Helper()
		if _, err := s.LastTrial(ctx, "atomic@example.com"); !errors.Is(err, apiaccess.ErrNotFound) {
			t.Errorf("%s: trial row left behind: %v", name, err)
		}
		if ents, _ := s.ListEntitlements(ctx, a.ID); len(ents) != 0 {
			t.Errorf("%s: entitlement left behind: %+v", name, ents)
		}
	}
	// A scope the client refuses before the transaction, and a grant whose insert fails inside it.
	if _, err := s.CreateTrial(ctx, "atomic@example.com", ends, long, grant(a.ID, "trial", "DEV_ACCESS", "retail")); err == nil {
		t.Error("dev access trial accepted")
	}
	noTrial("bad scope")
	noGames := trialGrant(a.ID)
	noGames.Games = nil
	if _, err := s.CreateTrial(ctx, "atomic@example.com", ends, long, noGames); err == nil {
		t.Error("trial grant without games accepted")
	}
	noTrial("failed grant")
	tr, err := s.CreateTrial(ctx, "atomic@example.com", ends, long, trialGrant(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	ents, _ := s.ListEntitlements(ctx, a.ID)
	if len(ents) != 1 || ents[0].Source != "trial" || ents[0].Status != "active" || ents[0].ValidUntil == nil || !ents[0].ValidUntil.Equal(ends) ||
		tr.AccountID != a.ID || !tr.EndsAt.Equal(ends) {
		t.Errorf("trial %+v grant %+v", tr, ents)
	}
}

func contractEndRefusesStripe(t *testing.T, s store) {
	a := mustAccount(t, s, "endstripe@example.com")
	e := grant(a.ID, "stripe", "ALL_ACCESS", "retail")
	e.ExternalRef = "sub_end"
	row, err := s.UpsertStripeEntitlement(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.EndEntitlement(ctx, row.ID, 0, at); !errors.Is(err, apiaccess.ErrStripeEntitlement) {
		t.Errorf("live stripe row: %v", err)
	}
	// Stripe is checked before "already ended", so an ended stripe row is refused the same way.
	e.Status, e.ValidUntil = "ended", &at
	if _, err := s.UpsertStripeEntitlement(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndEntitlement(ctx, row.ID, a.ID, at.Add(time.Hour)); !errors.Is(err, apiaccess.ErrStripeEntitlement) {
		t.Errorf("ended stripe row: %v", err)
	}
	if ents, _ := s.ListEntitlements(ctx, a.ID); len(ents) != 1 || ents[0].ValidUntil == nil || !ents[0].ValidUntil.Equal(at) {
		t.Errorf("refused end still wrote %+v", ents)
	}
}

func contractEndTwice(t *testing.T, s store) {
	a := mustAccount(t, s, "endtwice@example.com")
	other := mustAccount(t, s, "endother@example.com")
	row, err := s.AddEntitlement(ctx, grant(a.ID, "manual", "ALL_ACCESS", "retail"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.EndEntitlement(ctx, row.ID, other.ID, at); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("end through another account: %v", err)
	}
	ended, err := s.EndEntitlement(ctx, row.ID, a.ID, at)
	if err != nil || ended.ID != row.ID || ended.Status != "ended" || ended.ValidUntil == nil || !ended.ValidUntil.Equal(at) {
		t.Fatalf("end %+v %v", ended, err)
	}
	if _, err := s.EndEntitlement(ctx, row.ID, 0, at.Add(time.Hour)); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("second end: %v", err)
	}
	if ents, _ := s.ListEntitlements(ctx, a.ID); len(ents) != 1 || !ents[0].ValidUntil.Equal(at) {
		t.Errorf("second end moved valid_until: %+v", ents)
	}
}

func contractAccounts(t *testing.T, s store) {
	before := time.Now()
	a, err := s.CreateAccount(ctx, "  Ann@Example.COM ", "zoho 12")
	if err != nil {
		t.Fatal(err)
	}
	if a.Email != "ann@example.com" || a.Status != apiaccess.AccountActive || a.Note != "zoho 12" || a.SessionEpoch != 0 || !near(a.CreatedAt, before, time.Now()) {
		t.Errorf("created %+v", a)
	}
	if _, err := s.CreateAccount(ctx, "ann@example.com", ""); err == nil {
		t.Error("duplicate email accepted")
	}
	if got, err := s.GetAccountByEmail(ctx, "ANN@example.com"); err != nil || got.ID != a.ID {
		t.Errorf("by email %+v %v", got, err)
	}
	if got, err := s.GetAccount(ctx, a.ID); err != nil || got.Email != a.Email {
		t.Errorf("by id %+v %v", got, err)
	}
	if _, err := s.GetAccount(ctx, missing); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("missing id: %v", err)
	}
	if _, err := s.GetAccountByEmail(ctx, "nobody@example.com"); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("missing email: %v", err)
	}
	if got, err := s.GetOrCreateAccount(ctx, "Ann@example.com", "other"); err != nil || got.ID != a.ID || got.Note != "zoho 12" {
		t.Errorf("get or create existing %+v %v", got, err)
	}
	b, err := s.GetOrCreateAccount(ctx, "Bob@example.com", "new")
	if err != nil || b.ID == a.ID || b.Email != "bob@example.com" || b.Note != "new" {
		t.Errorf("get or create new %+v %v", b, err)
	}
	if err := s.SetAccountStatus(ctx, a.ID, "frozen"); err == nil {
		t.Error("unknown status accepted")
	}
	if err := s.SetAccountStatus(ctx, missing, apiaccess.AccountActive); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("status of missing: %v", err)
	}
	if err := s.SetAccountStatus(ctx, a.ID, apiaccess.AccountSuspended); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountNote(ctx, a.ID, "renewed"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountNote(ctx, missing, "x"); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("note of missing: %v", err)
	}
	if got, _ := s.GetAccount(ctx, a.ID); got.Status != apiaccess.AccountSuspended || got.Note != "renewed" {
		t.Errorf("after updates %+v", got)
	}
	search := func(q string) []string {
		found, err := s.SearchAccounts(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range found {
			out = append(out, f.Email)
		}
		return out
	}
	if got := search(" ANN "); !slices.Equal(got, []string{"ann@example.com"}) {
		t.Errorf("search ann %v", got)
	}
	if got := search("example"); !slices.Equal(got, []string{"ann@example.com", "bob@example.com"}) {
		t.Errorf("search example %v", got)
	}
	if got := search("%"); len(got) != 0 {
		t.Errorf("search %% matched %v", got)
	}
	for want := int64(1); want <= 2; want++ {
		if got, err := s.BumpSessionEpoch(ctx, a.ID); err != nil || got != want {
			t.Errorf("bump %d: %d %v", want, got, err)
		}
	}
	if _, err := s.BumpSessionEpoch(ctx, missing); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("bump missing: %v", err)
	}
}

func contractKeys(t *testing.T, s store) {
	a := mustAccount(t, s, "keys@example.com")
	other := mustAccount(t, s, "other@example.com")
	plain, k1, err := s.CreateKey(ctx, a.ID, "laptop", apiaccess.KeyDemo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "ban_demo_") || !apiaccess.LooksLikeKey(plain) || k1.Hash != apiaccess.HashKey(plain) ||
		k1.Prefix != plain[9:17] || k1.Kind != apiaccess.KeyDemo || k1.Label != "laptop" || k1.RevokedAt != nil || k1.LastUsedAt != nil {
		t.Errorf("demo key %q %+v", plain, k1)
	}
	_, k2, err := s.CreateKey(ctx, a.ID, "server", apiaccess.KeyLive)
	if err != nil || k2.Kind != apiaccess.KeyLive {
		t.Fatalf("live key %+v %v", k2, err)
	}
	if _, _, err := s.CreateKey(ctx, a.ID, "", apiaccess.KeyKind("mtgban_live")); err == nil {
		t.Error("unknown kind accepted")
	}
	if _, _, err := s.CreateKey(ctx, missing, "", apiaccess.KeyLive); err == nil {
		t.Error("key for an unknown account accepted")
	}
	keys, err := s.ListKeys(ctx, a.ID)
	if err != nil || len(keys) != 2 || keys[0].ID != k1.ID || keys[1].ID != k2.ID {
		t.Fatalf("list %+v %v", keys, err)
	}
	if _, err := s.RevokeKey(ctx, k1.ID, other.ID); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("revoke through another account: %v", err)
	}
	before := time.Now()
	if got, err := s.RevokeKey(ctx, k1.ID, a.ID); err != nil || got.RevokedAt == nil || !near(*got.RevokedAt, before, time.Now()) {
		t.Errorf("revoke %+v %v", got, err)
	}
	if _, err := s.RevokeKey(ctx, k1.ID, 0); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("revoke twice: %v", err)
	}
	if got, err := s.RevokeKeyByPrefix(ctx, k2.Prefix); err != nil || got.ID != k2.ID || got.RevokedAt == nil {
		t.Errorf("revoke by prefix %+v %v", got, err)
	}
	if _, err := s.RevokeKeyByPrefix(ctx, k2.Prefix); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("revoke by prefix twice: %v", err)
	}
	if keys, _ := s.ListKeys(ctx, a.ID); len(keys) != 2 || keys[0].RevokedAt == nil || keys[1].RevokedAt == nil {
		t.Errorf("after revokes %+v", keys)
	}
	if keys, err := s.ListKeys(ctx, other.ID); err != nil || len(keys) != 0 {
		t.Errorf("other's keys %+v %v", keys, err)
	}
}

func contractEntitlements(t *testing.T, s store) {
	a := mustAccount(t, s, "ents@example.com")
	if ents, err := s.ListEntitlements(ctx, a.ID); err != nil || ents == nil || len(ents) != 0 {
		t.Errorf("none yet: %#v %v", ents, err)
	}
	until := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	e := grant(a.ID, apiaccess.SourceManual, "ALL_ACCESS", "retail")
	e.ValidUntil, e.Note, e.Addons, e.Games = &until, "annual", []string{"extra_game:1"}, []string{"magic", "pokemon"}
	got, err := s.AddEntitlement(ctx, e)
	if err != nil || got.ValidUntil == nil || !got.ValidUntil.Equal(until) || got.Note != "annual" ||
		!slices.Equal(got.Addons, []string{"extra_game:1"}) || !slices.Equal(got.Games, []string{"magic", "pokemon"}) || got.ExternalRef != "" {
		t.Fatalf("added %+v %v", got, err)
	}
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.EndEntitlement(ctx, got.ID, 0, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndEntitlement(ctx, missing, 0, at); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("end missing: %v", err)
	}
	ents, _ := s.ListEntitlements(ctx, a.ID)
	if len(ents) != 1 || ents[0].Status != apiaccess.EntitlementEnded || ents[0].ValidUntil == nil || !ents[0].ValidUntil.Equal(at) {
		t.Errorf("ended %+v", ents)
	}
}

func contractInvites(t *testing.T, s store) {
	before := time.Now()
	token, inv, err := s.CreateInvite(ctx, "quarterly", "Bound@Example.com", time.Hour, "for bound")
	if err != nil {
		t.Fatal(err)
	}
	if inv.TokenHash != apiaccess.HashKey(token) || inv.Email != "bound@example.com" || inv.IntervalKey != "quarterly" || inv.Note != "for bound" ||
		inv.UsedAt != nil || !near(inv.ExpiresAt, before.Add(time.Hour), time.Now().Add(time.Hour)) {
		t.Errorf("invite %+v", inv)
	}
	now := time.Now()
	if _, err := s.ConsumeInvite(ctx, token, "other@example.com", now); !errors.Is(err, apiaccess.ErrInviteInvalid) {
		t.Errorf("wrong email: %v", err)
	}
	if got, err := s.ConsumeInvite(ctx, token, "BOUND@example.com", now); err != nil || got.UsedAt == nil || !sameMicro(*got.UsedAt, now) {
		t.Errorf("consume %+v %v", got, err)
	}
	if _, err := s.ConsumeInvite(ctx, token, "bound@example.com", now); !errors.Is(err, apiaccess.ErrInviteInvalid) {
		t.Errorf("replay: %v", err)
	}
	if err := s.ReleaseInvite(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeInvite(ctx, token, "bound@example.com", now); err != nil {
		t.Errorf("after release: %v", err)
	}
	open, _, err := s.CreateInvite(ctx, "annual", "", time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeInvite(ctx, open, "anyone@example.com", time.Now().Add(2*time.Hour)); !errors.Is(err, apiaccess.ErrInviteInvalid) {
		t.Errorf("expired: %v", err)
	}
	if _, err := s.ConsumeInvite(ctx, open, "anyone@example.com", time.Now()); err != nil {
		t.Errorf("unbound: %v", err)
	}
	if _, err := s.ConsumeInvite(ctx, "nope", "", time.Now()); !errors.Is(err, apiaccess.ErrInviteInvalid) {
		t.Errorf("unknown: %v", err)
	}
	if err := s.ReleaseInvite(ctx, "nope"); err != nil {
		t.Errorf("release unknown: %v", err)
	}
}

func contractMagicLinks(t *testing.T, s store) {
	a := mustAccount(t, s, "link@example.com")
	link := func() string {
		token, err := s.CreateMagicLink(ctx, a.ID, 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	used := link()
	if got, err := s.ConsumeMagicLink(ctx, used, time.Now()); err != nil || got.ID != a.ID || got.Email != a.Email {
		t.Errorf("consume %+v %v", got, err)
	}
	if _, err := s.ConsumeMagicLink(ctx, used, time.Now()); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("replay: %v", err)
	}
	if _, err := s.ConsumeMagicLink(ctx, link(), time.Now().Add(16*time.Minute)); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("expired: %v", err)
	}
	deleted := link()
	if err := s.DeleteMagicLink(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeMagicLink(ctx, deleted, time.Now()); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("deleted: %v", err)
	}
	if _, err := s.CreateMagicLink(ctx, missing, time.Minute); err == nil {
		t.Error("link for an unknown account accepted")
	}
}

func contractStripeEvents(t *testing.T, s store) {
	step := func(name string, want bool) {
		t.Helper()
		if got, err := s.BeginStripeEvent(ctx, "evt_contract", "invoice.paid"); err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", name, got, err, want)
		}
	}
	step("first claim", true)
	step("in flight", false)
	if err := s.FinishStripeEvent(ctx, "evt_contract"); err != nil {
		t.Fatal(err)
	}
	step("processed", false)
	if err := s.DeleteStripeEvent(ctx, "evt_contract"); err != nil {
		t.Fatal(err)
	}
	step("after delete", true)
}

func contractNonces(t *testing.T, s store) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if err := s.ConsumeNonce(ctx, "n1", now.Add(time.Hour), now); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.ConsumeNonce(ctx, "n1", now.Add(time.Hour), now); !errors.Is(err, apiaccess.ErrNonceUsed) {
		t.Errorf("second: %v", err)
	}
	if err := s.ConsumeNonce(ctx, "n1", now.Add(3*time.Hour), now.Add(2*time.Hour)); err != nil {
		t.Errorf("after the sweep: %v", err)
	}
}

func contractAdminActions(t *testing.T, s store) {
	a := mustAccount(t, s, "audited@example.com")
	for _, r := range []struct {
		actor, action string
		account       int64
		target        string
	}{
		{"Admin@Example.com", "grant", a.ID, "t1"},
		{"cli:bob", "status", 0, "t2"},
		{"admin@example.com", "revoke", a.ID, "t3"},
	} {
		if err := s.RecordAdminAction(ctx, r.actor, r.action, r.account, r.target, "detail "+r.target); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	targets := func(acts []apiaccess.AdminAction) []string {
		var out []string
		for _, act := range acts {
			out = append(out, act.Target)
		}
		return out
	}
	all, err := s.ListAdminActions(ctx, 0, 10)
	if err != nil || !slices.Equal(targets(all), []string{"t3", "t2", "t1"}) {
		t.Fatalf("all %+v %v", all, err)
	}
	if all[2].Actor != "admin@example.com" || all[2].Action != "grant" || all[2].AccountID != a.ID || all[2].Detail != "detail t1" || all[1].AccountID != 0 {
		t.Errorf("rows %+v", all)
	}
	if mine, _ := s.ListAdminActions(ctx, a.ID, 10); !slices.Equal(targets(mine), []string{"t3", "t1"}) {
		t.Errorf("one account %v", targets(mine))
	}
	if newest, _ := s.ListAdminActions(ctx, 0, 1); !slices.Equal(targets(newest), []string{"t3"}) {
		t.Errorf("limit 1 %v", targets(newest))
	}
}

func contractDemoAccess(t *testing.T, s store) {
	now := time.Now()
	future, past := now.Add(14*24*time.Hour), now.Add(-time.Hour)
	add := func(email string, source apiaccess.Source, from time.Time, until *time.Time, ref string) apiaccess.Account {
		a := mustAccount(t, s, email)
		e := grant(a.ID, source, "ALL_ACCESS", "retail")
		e.ValidFrom, e.ValidUntil, e.Note, e.ExternalRef = from, until, string(source)+" note", ref
		if _, err := s.AddEntitlement(ctx, e); err != nil {
			t.Fatal(err)
		}
		return a
	}
	trial := mustAccount(t, s, "trialist@example.com")
	tg := trialGrant(trial.ID)
	tg.ValidFrom, tg.Note = now.Add(-time.Hour), "trial note"
	if _, err := s.CreateTrial(ctx, "Patron@example.com", future, now.AddDate(-1, 0, 0), tg); err != nil {
		t.Fatal(err)
	}
	manual := add("manual@example.com", apiaccess.SourceManual, now.Add(-2*time.Hour), nil, "")
	for i := 0; i < 2; i++ {
		if _, _, err := s.CreateKey(ctx, manual.ID, "k", apiaccess.KeyDemo); err != nil {
			t.Fatal(err)
		}
	}
	keys, _ := s.ListKeys(ctx, manual.ID)
	if _, err := s.RevokeKey(ctx, keys[0].ID, 0); err != nil {
		t.Fatal(err)
	}
	add("stripe@example.com", apiaccess.SourceStripe, now.Add(-time.Minute), nil, "sub_demo")
	add("expired@example.com", apiaccess.SourceManual, now.Add(-48*time.Hour), &past, "")
	ended := add("ended@example.com", apiaccess.SourceManual, now.Add(-time.Minute), nil, "")
	ents, _ := s.ListEntitlements(ctx, ended.ID)
	if _, err := s.EndEntitlement(ctx, ents[0].ID, 0, future); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListDemoAccess(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("demo access %+v %v", got, err)
	}
	if d := got[0]; d.AccountID != trial.ID || d.Source != apiaccess.SourceTrial || d.Requester != "patron@example.com" || d.EndsAt == nil ||
		!sameMicro(*d.EndsAt, future) || d.Keys != 0 || d.LastUsed != nil || d.Note != "trial note" {
		t.Errorf("trial row %+v", d)
	}
	if d := got[1]; d.AccountID != manual.ID || d.Email != "manual@example.com" || d.Source != apiaccess.SourceManual || d.Requester != "" || d.EndsAt != nil || d.Keys != 1 {
		t.Errorf("manual row %+v", d)
	}
}
