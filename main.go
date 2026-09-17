// Command api-gatewahy fronts the per-game BAN price APIs behind one bearer
// key per customer. `serve` runs the gateway; the other subcommands manage
// accounts, keys, and entitlements against the same config.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
)

// version is set by -ldflags "-X main.version=..." at build time.
var version = "dev"

type command struct {
	usage string
	run   func(ctx context.Context, args []string, stdout, stderr io.Writer) int
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
