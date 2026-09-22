package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/config"
)

// adminStore is the slice of apiaccess.Client the subcommands use.
type adminStore interface {
	CreateAccount(ctx context.Context, email, note string) (apiaccess.Account, error)
	GetAccountByEmail(ctx context.Context, email string) (apiaccess.Account, error)
	SetAccountStatus(ctx context.Context, id int64, status string) error
	ListAccounts(ctx context.Context) ([]apiaccess.Account, error)
	CreateKey(ctx context.Context, accountID int64, label string) (string, apiaccess.Key, error)
	RevokeKeyByPrefix(ctx context.Context, prefix string) (apiaccess.Key, error)
	ListKeys(ctx context.Context, accountID int64) ([]apiaccess.Key, error)
	AddEntitlement(ctx context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error)
	EndEntitlement(ctx context.Context, id int64, at time.Time) error
	ListEntitlements(ctx context.Context, accountID int64) ([]apiaccess.Entitlement, error)
	SummarizeUsage(ctx context.Context, since, until time.Time, accountID int64) ([]apiaccess.UsageRow, error)
	Notify(ctx context.Context, payload string) error
	RecordAdminAction(ctx context.Context, actor, action string, accountID int64, target, detail string) error
}

func init() {
	for _, name := range []string{"account", "key", "grant", "usage"} {
		name := name
		commands[name] = command{
			usage: adminUsage[name],
			run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
				return withStore(ctx, args, stderr, func(store *apiaccess.Client, cfg *config.Config, rest []string) int {
					return runAdmin(ctx, store, cfg.KnownStores, cfg.GameNames(), name, rest, stdout, stderr)
				})
			},
		}
	}
}

var adminUsage = map[string]string{
	"account": "add | suspend | reinstate | list",
	"key":     "create | revoke | list",
	"grant":   "add | end | list",
	"usage":   "summarize requests by account and game",
}

// extractConfigFlag takes the -config value from anywhere in args and removes
// it, so the flag works before or after the verb.
func extractConfigFlag(args []string) (string, []string) {
	path := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "-config", arg == "--config":
			if i+1 < len(args) {
				path = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-config="):
			path = strings.TrimPrefix(arg, "-config=")
		case strings.HasPrefix(arg, "--config="):
			path = strings.TrimPrefix(arg, "--config=")
		default:
			rest = append(rest, arg)
		}
	}
	return path, rest
}

// withStore peels the -config flag, opens the store, and runs fn.
func withStore(ctx context.Context, args []string, stderr io.Writer, fn func(*apiaccess.Client, *config.Config, []string) int) int {
	configPath, args := extractConfigFlag(args)
	cfg, err := config.Load(ctx, configPath)
	if err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	store, err := apiaccess.NewClient(*cfg.APIAccess)
	if err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	store.SetKnownStores(cfg.KnownStores)
	return fn(store, cfg, args)
}

// runAdmin dispatches one operator command. Exit codes: 0 ok, 1 error, 2 usage.
func runAdmin(ctx context.Context, store adminStore, knownStores, knownGames []string, cmd string, args []string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	if cmd == "usage" {
		return adminUsageCmd(ctx, store, args, stdout, stderr)
	}
	if len(args) == 0 {
		fmt.Fprintf(stderr, "usage: api-gatewahy %s <%s>\n", cmd, adminUsage[cmd])
		return 2
	}
	verb, args := args[0], args[1:]
	fs := flag.NewFlagSet(cmd+" "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	email := fs.String("email", "", "account email")
	note := fs.String("note", "", "free-text note")
	label := fs.String("label", "", "key label")
	prefix := fs.String("prefix", "", "key prefix as shown by key list")
	games := fs.String("games", "", "comma-separated game names")
	stores := fs.String("stores", "", "ALL_ACCESS, BASE_ACCESS, or a comma-separated store list")
	modes := fs.String("modes", "", "comma-separated subset of retail,buylist,sealed")
	until := fs.String("until", "", "end date YYYY-MM-DD, exclusive")
	id := fs.Int64("id", 0, "entitlement id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	need := func(name, val string) bool {
		if val == "" {
			fmt.Fprintf(stderr, "api-gatewahy: -%s is required\n", name)
			return false
		}
		return true
	}
	byEmail := func() (apiaccess.Account, bool) {
		if !need("email", *email) {
			return apiaccess.Account{}, false
		}
		a, err := store.GetAccountByEmail(ctx, *email)
		if err != nil {
			fail(fmt.Errorf("%s: %w", *email, err))
			return apiaccess.Account{}, false
		}
		return a, true
	}
	// The write already landed, so a failed notify is a warning, not an error.
	notify := func() {
		if err := store.Notify(ctx, ""); err != nil {
			fmt.Fprintf(stderr, "api-gatewahy: warning: cache reload notification failed (%v); gateways apply the change within cache_ttl_seconds\n", err)
		}
	}
	// Every mutation lands in admin_actions like the web admin, actor "cli:<user>".
	audit := func(action string, accountID int64, target, detail string) {
		if err := store.RecordAdminAction(ctx, cliActor(), action, accountID, target, detail); err != nil {
			fmt.Fprintf(stderr, "api-gatewahy: audit %s: %v\n", action, err)
		}
	}

	switch cmd + " " + verb {
	case "account add":
		if !need("email", *email) {
			return 2
		}
		a, err := store.CreateAccount(ctx, *email, *note)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "account %d %s created\n", a.ID, a.Email)
		return 0
	case "account suspend", "account reinstate":
		a, ok := byEmail()
		if !ok {
			return exitFor(*email)
		}
		status := "suspended"
		if verb == "reinstate" {
			status = "active"
		}
		if err := store.SetAccountStatus(ctx, a.ID, status); err != nil {
			return fail(err)
		}
		audit("status", a.ID, "", status)
		notify()
		fmt.Fprintf(stdout, "account %d %s is now %s\n", a.ID, a.Email, status)
		return 0
	case "account list":
		list, err := store.ListAccounts(ctx)
		if err != nil {
			return fail(err)
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tEMAIL\tSTATUS\tCREATED\tNOTE")
		for _, a := range list {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", a.ID, a.Email, a.Status, a.CreatedAt.Format("2006-01-02"), a.Note)
		}
		return flushTab(tw)
	case "key create":
		a, ok := byEmail()
		if !ok {
			return exitFor(*email)
		}
		plain, k, err := store.CreateKey(ctx, a.ID, *label)
		if err != nil {
			return fail(err)
		}
		audit("key create", a.ID, k.Prefix, *label)
		notify()
		fmt.Fprintf(stdout, "key created for %s (prefix %s). Shown once, copy it now:\n\n    %s\n\n", a.Email, k.Prefix, plain)
		return 0
	case "key revoke":
		if !need("prefix", *prefix) {
			return 2
		}
		k, err := store.RevokeKeyByPrefix(ctx, *prefix)
		if err != nil {
			return fail(err)
		}
		audit("key revoke", k.AccountID, k.Prefix, "")
		notify()
		fmt.Fprintf(stdout, "key %s revoked\n", k.Prefix)
		return 0
	case "key list":
		a, ok := byEmail()
		if !ok {
			return exitFor(*email)
		}
		keys, err := store.ListKeys(ctx, a.ID)
		if err != nil {
			return fail(err)
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PREFIX\tLABEL\tCREATED\tLAST USED\tREVOKED")
		for _, k := range keys {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.Prefix, k.Label, k.CreatedAt.Format("2006-01-02"), fmtTime(k.LastUsedAt), fmtTime(k.RevokedAt))
		}
		return flushTab(tw)
	case "grant add":
		a, ok := byEmail()
		if !ok {
			return exitFor(*email)
		}
		if !need("games", *games) || !need("stores", *stores) || !need("modes", *modes) {
			return 2
		}
		scope, err := apiaccess.ValidateStoreScope(*stores, knownStores)
		if err != nil {
			return fail(err)
		}
		modeList, err := apiaccess.ValidateModes(strings.Split(*modes, ","))
		if err != nil {
			return fail(err)
		}
		e := apiaccess.Entitlement{AccountID: a.ID, Source: "manual", Games: splitList(*games), StoreScope: scope, Modes: modeList, Note: *note}
		if len(e.Games) == 0 {
			return fail(errors.New("no games given"))
		}
		for _, g := range e.Games {
			if !slices.Contains(knownGames, g) {
				return fail(fmt.Errorf("unknown game %q, configured games are %s", g, strings.Join(knownGames, ",")))
			}
		}
		if *until != "" {
			t, err := time.Parse("2006-01-02", *until)
			if err != nil {
				return fail(fmt.Errorf("-until: %w", err))
			}
			e.ValidUntil = &t
		}
		e, err = store.AddEntitlement(ctx, e)
		if err != nil {
			return fail(err)
		}
		audit("grant", a.ID, "entitlement "+strconv.FormatInt(e.ID, 10), strings.Join(e.Games, ",")+" "+e.StoreScope+" "+strings.Join(e.Modes, ","))
		notify()
		fmt.Fprintf(stdout, "entitlement %d added for %s: games %s, stores %s, modes %s\n", e.ID, a.Email,
			strings.Join(e.Games, ","), e.StoreScope, strings.Join(e.Modes, ","))
		return 0
	case "grant end":
		if *id == 0 {
			fmt.Fprintln(stderr, "api-gatewahy: -id is required")
			return 2
		}
		if err := store.EndEntitlement(ctx, *id, time.Now()); err != nil {
			return fail(err)
		}
		audit("end", 0, "entitlement "+strconv.FormatInt(*id, 10), "")
		notify()
		fmt.Fprintf(stdout, "entitlement %d ended\n", *id)
		return 0
	case "grant list":
		a, ok := byEmail()
		if !ok {
			return exitFor(*email)
		}
		ents, err := store.ListEntitlements(ctx, a.ID)
		if err != nil {
			return fail(err)
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSOURCE\tSTATUS\tGAMES\tSTORES\tMODES\tFROM\tUNTIL\tNOTE")
		for _, e := range ents {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.Source, e.Status, strings.Join(e.Games, ","),
				e.StoreScope, strings.Join(e.Modes, ","), e.ValidFrom.Format("2006-01-02"), fmtTime(e.ValidUntil), e.Note)
		}
		return flushTab(tw)
	}
	fmt.Fprintf(stderr, "usage: api-gatewahy %s <%s>\n", cmd, adminUsage[cmd])
	return 2
}

func adminUsageCmd(ctx context.Context, store adminStore, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.String("since", "", "start date YYYY-MM-DD (default 30 days ago)")
	until := fs.String("until", "", "end date YYYY-MM-DD, exclusive (default tomorrow)")
	email := fs.String("email", "", "restrict to one account")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	from := time.Now().AddDate(0, 0, -30)
	to := time.Now().AddDate(0, 0, 1)
	var err error
	if *since != "" {
		if from, err = time.Parse("2006-01-02", *since); err != nil {
			fmt.Fprintln(stderr, "api-gatewahy: -since:", err)
			return 2
		}
	}
	if *until != "" {
		if to, err = time.Parse("2006-01-02", *until); err != nil {
			fmt.Fprintln(stderr, "api-gatewahy: -until:", err)
			return 2
		}
	}
	var accountID int64
	if *email != "" {
		a, err := store.GetAccountByEmail(ctx, *email)
		if err != nil {
			fmt.Fprintln(stderr, "api-gatewahy:", err)
			return 1
		}
		accountID = a.ID
	}
	rows, err := store.SummarizeUsage(ctx, from, to, accountID)
	if err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "EMAIL\tGAME\tREQUESTS\tBYTES\tERRORS")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", r.Email, r.Game, r.Requests, r.Bytes, r.Errors)
	}
	return flushTab(tw)
}

func exitFor(email string) int {
	if email == "" {
		return 2
	}
	return 1
}

func flushTab(tw *tabwriter.Writer) int {
	if err := tw.Flush(); err != nil {
		return 1
	}
	return 0
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format("2006-01-02")
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// cliActor names who ran the command in the audit log.
func cliActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "cli:" + u.Username
	}
	return "cli"
}
