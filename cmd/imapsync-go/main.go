// Command imapsync-go synchronises IMAP accounts.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run())
}

// run is main's body, returning a status rather than calling os.Exit itself so
// that the deferred stop actually runs. Exiting from inside main skipped it,
// leaving the signal handler installed as the process went down.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := newRootCmd()
	root.SetArgs(argsFor(os.Args))

	if err := root.ExecuteContext(ctx); err != nil {
		// Cobra has already printed the error for usage problems; this covers
		// runtime failures.
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		return 1
	}
	return 0
}

// argsFor decides what the arguments mean, which depends on what this binary
// was called.
//
// Linked as "imapsync" it is asked for compat, so that an existing cron line
// keeps working with nothing edited but the path — which is the only sense in
// which anything can honestly be called a drop-in replacement.
func argsFor(argv []string) []string {
	args := argv[1:]
	if filepath.Base(argv[0]) == "imapsync" {
		return append([]string{"compat"}, args...)
	}
	return args
}

type globalFlags struct {
	logLevel string
	logJSON  bool
}

func newRootCmd() *cobra.Command {
	var g globalFlags

	cmd := &cobra.Command{
		Use:           "imapsync-go",
		Short:         "Concurrent IMAP account synchronisation",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return setupLogging(g)
		},
	}

	cmd.PersistentFlags().StringVar(&g.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	cmd.PersistentFlags().BoolVar(&g.logJSON, "log-json", false, "force JSON logs (default: JSON when stderr is not a terminal)")

	cmd.AddCommand(newCompatCmd())
	cmd.AddCommand(newOAuthCmd())
	cmd.AddCommand(newProbeCmd())
	cmd.AddCommand(newSyncCmd())

	return cmd
}

func setupLogging(g globalFlags) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(g.logLevel)); err != nil {
		return fmt.Errorf("invalid --log-level %q: %w", g.logLevel, err)
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if g.logJSON || !isTerminal(os.Stderr) {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))
	return nil
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
