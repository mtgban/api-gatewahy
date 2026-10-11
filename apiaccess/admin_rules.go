package apiaccess

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ManualGrantInput is the manual-grant form both admins collect.
type ManualGrantInput struct {
	AccountID int64
	Games     []string
	Stores    string
	Modes     []string
	Lookback  int
	Until     string
	Note      string
}

// GrantError names which field of a manual grant failed; Value is the
// offending game, set only when Field is games.
type GrantError struct {
	Field string
	Value string
	Msg   string
}

func (e *GrantError) Error() string { return e.Msg }

// ManualGrant validates a manual grant and builds the entitlement to store.
func ManualGrant(in ManualGrantInput, games []string, now time.Time) (Entitlement, error) {
	if len(in.Games) == 0 {
		return Entitlement{}, &GrantError{Field: "games", Msg: "no games given"}
	}
	for _, g := range in.Games {
		if !slices.Contains(games, g) {
			return Entitlement{}, &GrantError{Field: "games", Value: g, Msg: fmt.Sprintf("unknown game %q, configured games are %s", g, strings.Join(games, ","))}
		}
	}
	modes, err := ValidateModes(in.Modes)
	if err != nil {
		return Entitlement{}, &GrantError{Field: "modes", Msg: err.Error()}
	}
	history := slices.Contains(modes, ModeHistory)
	switch {
	case history && len(modes) != 1:
		return Entitlement{}, &GrantError{Field: "modes", Msg: "history is a mode of its own"}
	case history && strings.TrimSpace(in.Stores) != "":
		return Entitlement{}, &GrantError{Field: "stores", Msg: "history takes no stores"}
	case history && in.Lookback < 1:
		return Entitlement{}, &GrantError{Field: "lookback", Msg: "lookback must be at least 1 day"}
	case history && in.Lookback > MaxLookbackDays:
		return Entitlement{}, &GrantError{Field: "lookback", Msg: fmt.Sprintf("lookback must be at most %d days", MaxLookbackDays)}
	case !history && in.Lookback != 0:
		return Entitlement{}, &GrantError{Field: "lookback", Msg: "lookback applies to a history grant only"}
	}
	scope := ""
	if !history {
		scope, err = ValidateStoreScope(in.Stores)
		if err != nil {
			return Entitlement{}, &GrantError{Field: "stores", Msg: err.Error()}
		}
	}
	e := Entitlement{AccountID: in.AccountID, Source: SourceManual, Games: in.Games, StoreScope: scope, Modes: modes, LookbackDays: in.Lookback, Note: in.Note}
	if in.Until != "" {
		t, err := time.Parse("2006-01-02", in.Until)
		if err != nil {
			return Entitlement{}, &GrantError{Field: "until", Msg: "until must be YYYY-MM-DD"}
		}
		if t.Before(now) {
			return Entitlement{}, &GrantError{Field: "until", Msg: "until must be in the future"}
		}
		e.ValidUntil = &t
	}
	return e, nil
}

// GrantDetail is the audit-log string both admins record for a grant.
func GrantDetail(e Entitlement) string {
	s := strings.Join(e.Games, ",") + " " + e.StoreScope + " " + strings.Join(e.Modes, ",")
	if e.LookbackDays > 0 {
		s += " " + strconv.Itoa(e.LookbackDays) + "d"
	}
	return s
}

// KeyKindFor is live when ents include an active Stripe entitlement, else demo.
func KeyKindFor(ents []Entitlement) KeyKind {
	if HasActiveStripePlan(ents) {
		return KeyLive
	}
	return KeyDemo
}

// WindowError names which usage-window field failed.
type WindowError struct {
	Field string
	Msg   string
}

func (e *WindowError) Error() string { return e.Msg }

// UsageWindow parses the usage command's since/until, defaulting to the
// trailing 30 days through tomorrow. from must not be after to.
func UsageWindow(since, until string, now time.Time) (from, to time.Time, err error) {
	from = now.AddDate(0, 0, -30)
	to = now.AddDate(0, 0, 1)
	if since != "" {
		if from, err = time.Parse("2006-01-02", since); err != nil {
			return time.Time{}, time.Time{}, &WindowError{Field: "since", Msg: "since must be YYYY-MM-DD"}
		}
	}
	if until != "" {
		if to, err = time.Parse("2006-01-02", until); err != nil {
			return time.Time{}, time.Time{}, &WindowError{Field: "until", Msg: "until must be YYYY-MM-DD"}
		}
	}
	if from.After(to) {
		return time.Time{}, time.Time{}, &WindowError{Field: "order", Msg: "since must not be after until"}
	}
	return from, to, nil
}

// SplitList splits a comma-separated field, trimming blanks and dropping empties.
func SplitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
