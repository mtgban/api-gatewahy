package apiaccess

import (
	"errors"
	"slices"
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
