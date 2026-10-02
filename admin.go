package main

import (
	"context"
	"fmt"
	"io"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/config"
)

// accountLookup finds an account by email; admin and billing verbs share it.
type accountLookup interface {
	GetAccountByEmail(ctx context.Context, email string) (apiaccess.Account, error)
}

// entitlementLister lists an account's grants, which also set a new key's kind.
type entitlementLister interface {
	ListEntitlements(ctx context.Context, accountID int64) ([]apiaccess.Entitlement, error)
}

// changeRecorder audits a mutation and tells the gateways to reload.
type changeRecorder interface {
	Notify(ctx context.Context, payload string) error
	RecordAdminAction(ctx context.Context, actor, action string, accountID int64, target, detail string) error
}

// accountStore is what the account verbs read and write.
type accountStore interface {
	accountLookup
	CreateAccount(ctx context.Context, email, note string) (apiaccess.Account, error)
	SetAccountStatus(ctx context.Context, id int64, status apiaccess.AccountStatus) error
	ListAccounts(ctx context.Context) ([]apiaccess.Account, error)
}

// keyStore is what the key verbs read and write.
type keyStore interface {
	accountLookup
	entitlementLister
	CreateKey(ctx context.Context, accountID int64, label string, kind apiaccess.KeyKind) (string, apiaccess.Key, error)
	RevokeKeyByPrefix(ctx context.Context, prefix string) (apiaccess.Key, error)
	ListKeys(ctx context.Context, accountID int64) ([]apiaccess.Key, error)
}

// grantStore is what the grant verbs read and write.
type grantStore interface {
	accountLookup
	entitlementLister
	AddEntitlement(ctx context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error)
	EndEntitlement(ctx context.Context, id, accountID int64, at time.Time) (apiaccess.Entitlement, error)
}

// usageStore is what the usage command reads.
type usageStore interface {
	accountLookup
	SummarizeUsage(ctx context.Context, since, until time.Time, accountID int64) ([]apiaccess.UsageRow, error)
}

// adminStore is the slice of apiaccess.Client the admin commands use.
type adminStore interface {
	accountStore
	keyStore
	grantStore
	usageStore
	changeRecorder
}

// adminDeps is what the admin verbs run against.
type adminDeps struct {
	store adminStore
	games []string
}

// adminVerbs is the verb table of the account, key and grant commands.
var adminVerbs = map[string][]verb[adminDeps]{
	"account": {
		{"add", accountAdd},
		{"suspend", accountSetStatus("suspend", apiaccess.AccountSuspended)},
		{"reinstate", accountSetStatus("reinstate", apiaccess.AccountActive)},
		{"list", accountList},
	},
	"key": {
		{"create", keyCreate},
		{"revoke", keyRevoke},
		{"list", keyList},
	},
	"grant": {
		{"add", grantAdd},
		{"end", grantEnd},
		{"list", grantList},
	},
}

func init() {
	for name, verbs := range adminVerbs {
		commands[name] = command{usage: verbList(verbs), run: adminCommand(name)}
	}
	commands["usage"] = command{usage: "summarize requests by account and game", run: adminCommand("usage")}
}

// adminCommand opens the store from the config and runs one admin command.
func adminCommand(name string) func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
		return withStore(ctx, args, stderr, func(store *apiaccess.Client, cfg *config.Config, rest []string) int {
			return runAdmin(ctx, store, cfg.GameNames(), name, rest, stdout, stderr)
		})
	}
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
	store, err := apiaccess.NewClient(ctx, *cfg.APIAccess)
	if err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	return fn(store, cfg, args)
}

// runAdmin dispatches one admin command. Exit codes: 0 ok, 1 error, 2 usage.
func runAdmin(ctx context.Context, store adminStore, knownGames []string, cmd string, args []string, stdout, stderr io.Writer) int {
	if cmd == "usage" {
		return adminUsageCmd(ctx, store, args, stdout, stderr)
	}
	return runVerb(ctx, cmd, adminVerbs[cmd], adminDeps{store: store, games: knownGames}, args, stdout, stderr)
}

func accountAdd(ctx context.Context, d adminDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("account add", stderr)
	email := fs.String("email", "", "account email")
	note := fs.String("note", "", "free-text note")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !need(stderr, "email", *email) {
		return 2
	}
	a, err := d.store.CreateAccount(ctx, *email, *note)
	if err != nil {
		return fail(stderr, err)
	}
	audit(ctx, d.store, stderr, "account add", a.ID, "", *note)
	fmt.Fprintf(stdout, "account %d %s created\n", a.ID, a.Email)
	return 0
}

// accountSetStatus is suspend and reinstate, which differ only in the status set.
func accountSetStatus(name string, status apiaccess.AccountStatus) verbFunc[adminDeps] {
	return func(ctx context.Context, d adminDeps, args []string, stdout, stderr io.Writer) int {
		fs := verbFlags("account "+name, stderr)
		email := fs.String("email", "", "account email")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		a, ok := byEmail(ctx, d.store, *email, stderr)
		if !ok {
			return exitFor(*email)
		}
		if err := d.store.SetAccountStatus(ctx, a.ID, status); err != nil {
			return fail(stderr, err)
		}
		recordChange(ctx, d.store, stderr, "status", a.ID, "", string(status))
		fmt.Fprintf(stdout, "account %d %s is now %s\n", a.ID, a.Email, status)
		return 0
	}
}

func accountList(ctx context.Context, d adminDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("account list", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	list, err := d.store.ListAccounts(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tEMAIL\tSTATUS\tCREATED\tNOTE")
	for _, a := range list {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", a.ID, a.Email, a.Status, a.CreatedAt.Format("2006-01-02"), a.Note)
	}
	return flushTab(tw)
}

func keyCreate(ctx context.Context, d adminDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("key create", stderr)
	email := fs.String("email", "", "account email")
	label := fs.String("label", "", "key label")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	a, ok := byEmail(ctx, d.store, *email, stderr)
	if !ok {
		return exitFor(*email)
	}
	kind, err := keyKindFor(ctx, d.store, a.ID)
	if err != nil {
		return fail(stderr, err)
	}
	plain, k, err := d.store.CreateKey(ctx, a.ID, *label, kind)
	if err != nil {
		return fail(stderr, err)
	}
	recordChange(ctx, d.store, stderr, "key create", a.ID, k.Prefix, strings.TrimSpace(string(kind)+" "+*label))
	fmt.Fprintf(stdout, "key created for %s (%s, prefix %s). Shown once, copy it now:\n\n    %s\n\n", a.Email, kind, k.Prefix, plain)
	return 0
}

func keyRevoke(ctx context.Context, d adminDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("key revoke", stderr)
	prefix := fs.String("prefix", "", "key prefix as shown by key list")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !need(stderr, "prefix", *prefix) {
		return 2
	}
	k, err := d.store.RevokeKeyByPrefix(ctx, *prefix)
	if err != nil {
		return fail(stderr, err)
	}
	recordChange(ctx, d.store, stderr, "key revoke", k.AccountID, k.Prefix, "")
	fmt.Fprintf(stdout, "key %s revoked\n", k.Prefix)
	return 0
}

func keyList(ctx context.Context, d adminDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("key list", stderr)
	email := fs.String("email", "", "account email")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	a, ok := byEmail(ctx, d.store, *email, stderr)
	if !ok {
		return exitFor(*email)
	}
	keys, err := d.store.ListKeys(ctx, a.ID)
	if err != nil {
		return fail(stderr, err)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PREFIX\tKIND\tLABEL\tCREATED\tLAST USED\tREVOKED")
	for _, k := range keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", k.Prefix, k.Kind, k.Label, k.CreatedAt.Format("2006-01-02"), fmtTime(k.LastUsedAt), fmtTime(k.RevokedAt))
	}
	return flushTab(tw)
}

func grantAdd(ctx context.Context, d adminDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("grant add", stderr)
	email := fs.String("email", "", "account email")
	games := fs.String("games", "", "comma-separated game names")
	stores := fs.String("stores", "", "ALL_ACCESS, BASE_ACCESS, or comma-separated backend shorthands, typed exactly")
	modes := fs.String("modes", "", "comma-separated subset of retail,buylist,sealed")
	until := fs.String("until", "", "end date YYYY-MM-DD, exclusive")
	note := fs.String("note", "", "free-text note")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	a, ok := byEmail(ctx, d.store, *email, stderr)
	if !ok {
		return exitFor(*email)
	}
	if !need(stderr, "games", *games) || !need(stderr, "stores", *stores) || !need(stderr, "modes", *modes) {
		return 2
	}
	in := apiaccess.ManualGrantInput{AccountID: a.ID, Games: apiaccess.SplitList(*games), Stores: *stores,
		Modes: strings.Split(*modes, ","), Until: *until, Note: *note}
	e, err := apiaccess.ManualGrant(in, d.games, time.Now())
	if err != nil {
		return fail(stderr, err)
	}
	e, err = d.store.AddEntitlement(ctx, e)
	if err != nil {
		return fail(stderr, err)
	}
	recordChange(ctx, d.store, stderr, "grant", a.ID, "entitlement "+strconv.FormatInt(e.ID, 10), apiaccess.GrantDetail(e))
	fmt.Fprintf(stdout, "entitlement %d added for %s: games %s, stores %s, modes %s\n", e.ID, a.Email,
		strings.Join(e.Games, ","), e.StoreScope, strings.Join(e.Modes, ","))
	return 0
}

func grantEnd(ctx context.Context, d adminDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("grant end", stderr)
	id := fs.Int64("id", 0, "entitlement id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *id == 0 {
		fmt.Fprintln(stderr, "api-gatewahy: -id is required")
		return 2
	}
	e, err := d.store.EndEntitlement(ctx, *id, 0, time.Now())
	if err != nil {
		return fail(stderr, err)
	}
	recordChange(ctx, d.store, stderr, "end", e.AccountID, "entitlement "+strconv.FormatInt(*id, 10), "")
	fmt.Fprintf(stdout, "entitlement %d ended\n", *id)
	return 0
}

func grantList(ctx context.Context, d adminDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("grant list", stderr)
	email := fs.String("email", "", "account email")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	a, ok := byEmail(ctx, d.store, *email, stderr)
	if !ok {
		return exitFor(*email)
	}
	ents, err := d.store.ListEntitlements(ctx, a.ID)
	if err != nil {
		return fail(stderr, err)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSOURCE\tSTATUS\tGAMES\tSTORES\tMODES\tFROM\tUNTIL\tNOTE")
	for _, e := range ents {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.Source, e.Status, strings.Join(e.Games, ","),
			e.StoreScope, strings.Join(e.Modes, ","), e.ValidFrom.Format("2006-01-02"), fmtTime(e.ValidUntil), e.Note)
	}
	return flushTab(tw)
}

func adminUsageCmd(ctx context.Context, store usageStore, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("usage", stderr)
	since := fs.String("since", "", "start date YYYY-MM-DD (default 30 days ago)")
	until := fs.String("until", "", "end date YYYY-MM-DD, exclusive (default tomorrow)")
	email := fs.String("email", "", "restrict to one account")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	from, to, err := apiaccess.UsageWindow(*since, *until, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 2
	}
	var accountID int64
	if *email != "" {
		a, err := store.GetAccountByEmail(ctx, *email)
		if err != nil {
			return fail(stderr, err)
		}
		accountID = a.ID
	}
	rows, err := store.SummarizeUsage(ctx, from, to, accountID)
	if err != nil {
		return fail(stderr, err)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "EMAIL\tGAME\tREQUESTS\tBYTES\tERRORS")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", r.Email, r.Game, r.Requests, r.Bytes, r.Errors)
	}
	return flushTab(tw)
}

// fail reports err on stderr and returns exit code 1.
func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "api-gatewahy:", err)
	return 1
}

// need reports a missing required flag; false means the verb exits.
func need(stderr io.Writer, name, val string) bool {
	if val == "" {
		fmt.Fprintf(stderr, "api-gatewahy: -%s is required\n", name)
		return false
	}
	return true
}

// byEmail looks up the -email account, reporting a missing flag or a failed lookup.
func byEmail(ctx context.Context, store accountLookup, email string, stderr io.Writer) (apiaccess.Account, bool) {
	if !need(stderr, "email", email) {
		return apiaccess.Account{}, false
	}
	a, err := store.GetAccountByEmail(ctx, email)
	if err != nil {
		fail(stderr, fmt.Errorf("%s: %w", email, err))
		return apiaccess.Account{}, false
	}
	return a, true
}

// audit records a mutation as "cli:<user>", like the web admin. The write
// already landed, so a failure is reported, not fatal.
func audit(ctx context.Context, store changeRecorder, stderr io.Writer, action string, accountID int64, target, detail string) {
	if err := store.RecordAdminAction(ctx, cliActor(), action, accountID, target, detail); err != nil {
		fmt.Fprintf(stderr, "api-gatewahy: audit %s: %v\n", action, err)
	}
}

// recordChange audits a mutation, then notifies the gateways. The write
// already landed, so neither failure is fatal.
func recordChange(ctx context.Context, store changeRecorder, stderr io.Writer, action string, accountID int64, target, detail string) {
	audit(ctx, store, stderr, action, accountID, target, detail)
	if err := store.Notify(ctx, ""); err != nil {
		fmt.Fprintf(stderr, "api-gatewahy: warning: cache reload notification failed (%v); gateways apply the change within cache_ttl_seconds\n", err)
	}
}

// keyKindFor is live when the account has an active Stripe entitlement, else demo.
func keyKindFor(ctx context.Context, store entitlementLister, accountID int64) (apiaccess.KeyKind, error) {
	ents, err := store.ListEntitlements(ctx, accountID)
	if err != nil {
		return "", err
	}
	return apiaccess.KeyKindFor(ents), nil
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

// cliActor names who ran the command in the audit log.
func cliActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "cli:" + u.Username
	}
	return "cli"
}
