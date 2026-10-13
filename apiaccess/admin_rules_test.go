package apiaccess

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestManualGrant(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	games := []string{"magic", "pokemon"}
	cases := []struct {
		name         string
		in           ManualGrantInput
		wantField    string
		wantMsg      string
		wantValue    string
		wantErr      bool
		wantUntilNil bool
	}{
		{name: "no games", in: ManualGrantInput{Stores: "ALL_ACCESS", Modes: []string{"retail"}},
			wantField: "games", wantMsg: "no games given", wantErr: true},
		{name: "unknown game", in: ManualGrantInput{Games: []string{"magick"}, Stores: "ALL_ACCESS", Modes: []string{"retail"}},
			wantField: "games", wantMsg: `unknown game "magick", configured games are magic,pokemon`, wantValue: "magick", wantErr: true},
		{name: "bad scope", in: ManualGrantInput{Games: []string{"magic"}, Stores: "DEV_ACCESS", Modes: []string{"retail"}},
			wantField: "stores", wantMsg: "DEV_ACCESS cannot be granted", wantErr: true},
		{name: "bad modes", in: ManualGrantInput{Games: []string{"magic"}, Stores: "ALL_ACCESS", Modes: []string{"nonsense"}},
			wantField: "modes", wantMsg: `unknown mode "nonsense"`, wantErr: true},
		{name: "bad until", in: ManualGrantInput{Games: []string{"magic"}, Stores: "ALL_ACCESS", Modes: []string{"retail"}, Until: "not-a-date"},
			wantField: "until", wantMsg: "until must be YYYY-MM-DD", wantErr: true},
		{name: "past until", in: ManualGrantInput{Games: []string{"magic"}, Stores: "ALL_ACCESS", Modes: []string{"retail"}, Until: "2020-01-01"},
			wantField: "until", wantMsg: "until must be in the future", wantErr: true},
		{name: "happy path", in: ManualGrantInput{AccountID: 7, Games: []string{"magic", "pokemon"}, Stores: "CK,TCG", Modes: []string{"buylist", "retail"}, Until: "2027-01-01", Note: "annual"}},
		{name: "happy path, no until", in: ManualGrantInput{AccountID: 7, Games: []string{"magic", "pokemon"}, Stores: "CK,TCG", Modes: []string{"buylist", "retail"}, Note: "annual"},
			wantUntilNil: true},
	}
	for _, c := range cases {
		e, err := ManualGrant(c.in, games, now)
		if c.wantErr {
			var ge *GrantError
			if err == nil {
				t.Errorf("%s: want error", c.name)
				continue
			}
			if !errors.As(err, &ge) {
				t.Errorf("%s: err %v is not a *GrantError", c.name, err)
				continue
			}
			if ge.Field != c.wantField || ge.Msg != c.wantMsg {
				t.Errorf("%s: field %q msg %q, want %q %q", c.name, ge.Field, ge.Msg, c.wantField, c.wantMsg)
			}
			if c.wantValue != "" && ge.Value != c.wantValue {
				t.Errorf("%s: value %q, want %q", c.name, ge.Value, c.wantValue)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if e.AccountID != 7 || e.Source != SourceManual || e.StoreScope != "CK,TCG" || e.Note != "annual" {
			t.Errorf("%s: entitlement %+v", c.name, e)
		}
		if !slices.Equal(e.Modes, []string{"retail", "buylist"}) {
			t.Errorf("%s: modes %v", c.name, e.Modes)
		}
		if c.wantUntilNil {
			if e.ValidUntil != nil {
				t.Errorf("%s: until %v, want nil", c.name, e.ValidUntil)
			}
			continue
		}
		if e.ValidUntil == nil || e.ValidUntil.Year() != 2027 {
			t.Errorf("%s: until %v", c.name, e.ValidUntil)
		}
		if got := GrantDetail(e); got != "magic,pokemon CK,TCG retail,buylist" {
			t.Errorf("%s: detail %q", c.name, got)
		}
	}
}

func TestKeyKindFor(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	cases := []struct {
		name string
		ents []Entitlement
		want KeyKind
	}{
		{"live", []Entitlement{{Source: SourceStripe, Status: EntitlementActive, ValidFrom: past}}, KeyLive},
		{"demo, no entitlements", nil, KeyDemo},
		{"demo, manual only", []Entitlement{{Source: SourceManual, Status: EntitlementActive, ValidFrom: past}}, KeyDemo},
	}
	for _, c := range cases {
		if got := KeyKindFor(c.ents); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestUsageWindow(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name             string
		since, until     string
		wantField        string
		wantErr          bool
		wantFrom, wantTo time.Time
	}{
		{name: "defaults", wantFrom: now.AddDate(0, 0, -30), wantTo: now.AddDate(0, 0, 1)},
		{name: "bad since", since: "not-a-date", wantField: "since", wantErr: true},
		{name: "bad until", until: "not-a-date", wantField: "until", wantErr: true},
		{name: "order", since: "2026-09-20", until: "2026-09-01", wantField: "order", wantErr: true},
		{name: "until only, more than 30 days back", until: "2020-01-01", wantField: "order", wantErr: true},
		{name: "since equals until", since: "2026-01-01", until: "2026-01-01",
			wantFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), wantTo: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "happy path", since: "2026-01-01", until: "2026-02-01",
			wantFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), wantTo: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		from, to, err := UsageWindow(c.since, c.until, now)
		if c.wantErr {
			var we *WindowError
			if err == nil {
				t.Errorf("%s: want error", c.name)
				continue
			}
			if !errors.As(err, &we) {
				t.Errorf("%s: err %v is not a *WindowError", c.name, err)
				continue
			}
			if we.Field != c.wantField {
				t.Errorf("%s: field %q, want %q", c.name, we.Field, c.wantField)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !from.Equal(c.wantFrom) || !to.Equal(c.wantTo) {
			t.Errorf("%s: got %v..%v, want %v..%v", c.name, from, to, c.wantFrom, c.wantTo)
		}
	}
}

func TestSplitList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"magic", []string{"magic"}},
		{"magic, pokemon", []string{"magic", "pokemon"}},
		{" , magic ,, pokemon ,", []string{"magic", "pokemon"}},
	}
	for _, c := range cases {
		got := SplitList(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%q: got %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: got %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

// A history grant stands alone, takes no stores and needs a lookback; a
// price grant takes no lookback.
func TestManualGrantHistory(t *testing.T) {
	games := []string{"magic"}
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		in    ManualGrantInput
		field string
		msg   string
	}{
		{"history alone", ManualGrantInput{AccountID: 1, Games: games, Modes: []string{"history"}, Lookback: 365}, "", ""},
		{"history with stores", ManualGrantInput{AccountID: 1, Games: games, Stores: "CK", Modes: []string{"history"}, Lookback: 365}, "stores", "history takes no stores"},
		{"history without lookback", ManualGrantInput{AccountID: 1, Games: games, Modes: []string{"history"}}, "lookback", "lookback must be at least 1"},
		{"history past ten years", ManualGrantInput{AccountID: 1, Games: games, Modes: []string{"history"}, Lookback: 3651}, "lookback", "lookback must be at most 3650 days"},
		{"history beside retail", ManualGrantInput{AccountID: 1, Games: games, Stores: "CK", Modes: []string{"retail", "history"}, Lookback: 30}, "modes", "history is a mode of its own"},
		{"price with lookback", ManualGrantInput{AccountID: 1, Games: games, Stores: "CK", Modes: []string{"retail"}, Lookback: 30}, "lookback", "lookback applies to a history grant only"},
		{"price without stores", ManualGrantInput{AccountID: 1, Games: games, Modes: []string{"retail"}}, "stores", "store scope is empty"},
	}
	for _, c := range cases {
		e, err := ManualGrant(c.in, games, now)
		if c.field == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
				continue
			}
			if !e.IsHistory() || e.StoreScope != "" || e.LookbackDays != 365 {
				t.Errorf("%s: %+v", c.name, e)
			}
			continue
		}
		var ge *GrantError
		if !errors.As(err, &ge) || ge.Field != c.field || !strings.Contains(ge.Msg, c.msg) {
			t.Errorf("%s: err %v, want field %q containing %q", c.name, err, c.field, c.msg)
		}
	}
}

// GrantDetail names the lookback on a history row so the audit log says what was granted.
func TestGrantDetailHistory(t *testing.T) {
	e := Entitlement{Games: []string{"magic"}, Modes: []string{"history"}, LookbackDays: 365}
	if got := GrantDetail(e); got != "magic  history 365d" {
		t.Errorf("GrantDetail = %q", got)
	}
}

// CanonicalEntitlement is the one rule set the client and the memstore share.
func TestCanonicalEntitlementHistory(t *testing.T) {
	base := Entitlement{AccountID: 1, Source: SourceManual, Games: []string{"magic"}}
	h := base
	h.Modes = []string{"history"}
	h.LookbackDays = 90
	got, err := CanonicalEntitlement(h)
	if err != nil || got.StoreScope != "" || got.LookbackDays != 90 || !got.IsHistory() {
		t.Errorf("history row: %+v %v", got, err)
	}
	h.LookbackDays = MaxLookbackDays
	if got, err := CanonicalEntitlement(h); err != nil || got.LookbackDays != 3650 {
		t.Errorf("ten-year row: %+v %v", got, err)
	}
	for _, c := range []struct {
		name string
		e    func() Entitlement
		want string
	}{
		{"history with scope", func() Entitlement { x := h; x.StoreScope = "CK"; return x }, "takes no store scope"},
		{"history without lookback", func() Entitlement { x := h; x.LookbackDays = 0; return x }, "lookback_days must be at least 1"},
		{"history past ten years", func() Entitlement { x := h; x.LookbackDays = 3651; return x }, "lookback_days must be at most 3650"},
		{"history beside sealed", func() Entitlement { x := h; x.Modes = []string{"sealed", "history"}; x.StoreScope = "CK"; return x }, "history is a mode of its own"},
		{"price with lookback", func() Entitlement {
			x := base
			x.Modes = []string{"retail"}
			x.StoreScope = "CK"
			x.LookbackDays = 30
			return x
		}, "lookback_days applies to a history row only"},
		{"price without scope", func() Entitlement { x := base; x.Modes = []string{"retail"}; return x }, "store scope is empty"},
	} {
		if _, err := CanonicalEntitlement(c.e()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.name, err, c.want)
		}
	}
}

// A newspaper row is an ordinary price row: it keeps a scope and takes no lookback.
func TestCanonicalEntitlementNewspaper(t *testing.T) {
	base := Entitlement{AccountID: 1, Source: SourceManual, Games: []string{"magic"}}
	n := base
	n.Modes = []string{"newspaper", "retail"}
	n.StoreScope = "TCG, CK"
	got, err := CanonicalEntitlement(n)
	if err != nil || got.StoreScope != "CK,TCG" || !slices.Equal(got.Modes, []string{"retail", "newspaper"}) || got.IsHistory() {
		t.Errorf("newspaper row: %+v %v", got, err)
	}
	for _, c := range []struct {
		name string
		e    func() Entitlement
		want string
	}{
		{"newspaper without scope", func() Entitlement { x := base; x.Modes = []string{"newspaper"}; return x }, "store scope is empty"},
		{"newspaper with lookback", func() Entitlement { x := n; x.LookbackDays = 30; return x }, "lookback_days applies to a history row only"},
		{"newspaper beside history", func() Entitlement {
			x := base
			x.Modes = []string{"newspaper", "history"}
			x.LookbackDays = 30
			return x
		}, "history is a mode of its own"},
	} {
		if _, err := CanonicalEntitlement(c.e()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.name, err, c.want)
		}
	}
}

// A manual newspaper grant needs stores like any price grant.
func TestManualGrantNewspaper(t *testing.T) {
	games := []string{"magic"}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	e, err := ManualGrant(ManualGrantInput{AccountID: 1, Games: games, Stores: "CK", Modes: []string{"newspaper", "retail"}}, games, now)
	if err != nil || e.StoreScope != "CK" || !slices.Equal(e.Modes, []string{"retail", "newspaper"}) || e.LookbackDays != 0 {
		t.Errorf("newspaper grant: %+v %v", e, err)
	}
	_, err = ManualGrant(ManualGrantInput{AccountID: 1, Games: games, Modes: []string{"newspaper"}}, games, now)
	var ge *GrantError
	if !errors.As(err, &ge) || ge.Field != "stores" || !strings.Contains(ge.Msg, "store scope is empty") {
		t.Errorf("newspaper without stores: %v", err)
	}
}
