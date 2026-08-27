// Command imapsync-go synchronises IMAP accounts.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		// Cobra has already printed the error for usage problems; this covers
		// runtime failures.
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
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
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return setupLogging(g)
		},
	}

	cmd.PersistentFlags().StringVar(&g.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	cmd.PersistentFlags().BoolVar(&g.logJSON, "log-json", false, "force JSON logs (default: JSON when stderr is not a terminal)")

	cmd.AddCommand(newProbeCmd())

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
