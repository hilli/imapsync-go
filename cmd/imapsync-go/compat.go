package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/hilli/imapsync-go/internal/compat"
)

func newCompatCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compat [imapsync options]",
		Short: "Run a sync from an imapsync command line",
		Long: `Compat reads imapsync's options and runs the equivalent sync.

It is meant for a command line that already works. Link this binary as
"imapsync" and it will behave as though it had been asked for compat, so an
existing cron line needs no editing at all:

    ln -s imapsync-go imapsync

Every option is answered. Most are translated, some are accepted and reported
as having done nothing, and the rest are refused with the reason and, where
there is one, the native flag to use instead. Nothing is dropped in silence: a
flag that changes which messages move, or what becomes of the ones that do not,
stops the run rather than being quietly forgotten.

The translation is printed before it runs, so it can be read, corrected, and
eventually pasted into the cron line in place of the imapsync one. A password
given as --password1 is passed through the environment rather than on the
command line, so the printed translation is safe to keep.`,
		Example: `  # As imapsync would have been called
  imapsync-go compat --host1 imap.mail.me.com --user1 you@example.com --password1 secret --ssl1 \
                     --host2 mox.example.net  --user2 you           --password2 other  --ssl2

  # See what it becomes without doing anything
  imapsync-go compat --dry --host1 ... --host2 ...`,

		// imapsync's options are not cobra's, and several of them collide with
		// this tool's own. The whole argument list has to arrive untouched.
		DisableFlagParsing: true,

		RunE: runCompat,
	}
	return cmd
}

func runCompat(cmd *cobra.Command, args []string) error {
	// Answered here rather than by the table, because the table's answer for
	// both of these would have to be "run the command you just ran".
	switch {
	case len(args) == 0 || slices.Contains(args, "-h") || slices.Contains(args, "--help"):
		return cmd.Help()
	case slices.Contains(args, "--version"):
		fmt.Fprintln(cmd.OutOrStdout(), cmd.Root().Name(), version)
		return nil
	}

	plan, err := compat.Translate(args)
	if err != nil {
		return err
	}

	fmt.Fprint(cmd.OutOrStdout(), plan.Explain(cmd.Root().Name()))
	fmt.Fprintln(cmd.OutOrStdout())

	for name, value := range plan.Env {
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("passing the password through %s: %w", name, err)
		}
		defer func() { _ = os.Unsetenv(name) }()
	}

	// A fresh root rather than the one already running: this command is inside
	// that one's RunE, and re-entering a command whose flags are already parsed
	// is the sort of thing that works until it does not.
	root := newRootCmd()
	root.SetArgs(plan.Args)
	root.SetOut(cmd.OutOrStdout())
	root.SetErr(cmd.ErrOrStderr())
	return root.ExecuteContext(cmd.Context())
}
