// Command api-gatewahy fronts the per-game BAN price APIs behind one bearer
// key per customer. `serve` runs the gateway; the other subcommands manage
// accounts, keys, and entitlements against the same config.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
)

// version is set by -ldflags "-X main.version=..." at build time.
var version = "dev"

type command struct {
	usage string
	run   func(ctx context.Context, args []string, stdout, stderr io.Writer) int
}

// verbFunc runs one verb against its deps D. Exit codes: 0 ok, 1 error, 2 usage.
type verbFunc[D any] func(ctx context.Context, d D, args []string, stdout, stderr io.Writer) int

// verb is one entry of a command's verb table; run builds its own FlagSet.
type verb[D any] struct {
	name string
	run  verbFunc[D]
}

// verbList is a command's usage line: its verb names in table order.
func verbList[D any](verbs []verb[D]) string {
	names := make([]string, len(verbs))
	for i, v := range verbs {
		names[i] = v.name
	}
	return strings.Join(names, " | ")
}

// runVerb runs the verb args[0] names; a missing or unknown verb exits 2 with the usage line.
func runVerb[D any](ctx context.Context, cmd string, verbs []verb[D], d D, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		for _, v := range verbs {
			if v.name == args[0] {
				return v.run(ctx, d, args[1:], stdout, stderr)
			}
		}
	}
	fmt.Fprintf(stderr, "usage: api-gatewahy %s <%s>\n", cmd, verbList(verbs))
	return 2
}

// verbFlags is a fresh FlagSet for one verb, reporting parse errors on stderr.
func verbFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// commands is filled by init functions in the files that own each command.
var commands = map[string]command{}

func init() {
	commands["version"] = command{
		usage: "print the build version",
		run: func(_ context.Context, _ []string, stdout, _ io.Writer) int {
			fmt.Fprintln(stdout, "api-gatewahy", version)
			return 0
		},
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: api-gatewahy <command> [flags]")
	fmt.Fprintln(w)
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %-10s %s\n", name, commands[name].usage)
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "api-gatewahy: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cmd.run(ctx, args[1:], stdout, stderr)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
