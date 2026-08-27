package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/folder"
	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/state"
	"github.com/hilli/imapsync-go/internal/syncer"
)

type syncFlags struct {
	configPath string
	pair       string

	sourceURL              string
	sourcePasswordEnv      string
	sourcePasswordFile     string
	sourcePasswordKeychain string

	destURL              string
	destPasswordEnv      string
	destPasswordFile     string
	destPasswordKeychain string

	statePath string
	dryRun    bool

	only           []string
	include        []string
	exclude        []string
	mappings       []string
	subfolder      string
	automap        bool
	includeVirtual bool

	dialTimeout time.Duration
	insecure    bool
	trace       bool
}

func newSyncCmd() *cobra.Command {
	var f syncFlags

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Copy messages from one IMAP account to another",
		Long: `Sync copies every message the destination does not already hold.

It is safe to interrupt and re-run. Progress is recorded in a state database as
the copy proceeds, and a message whose append was in flight when the process
stopped is searched for on the destination before it is copied again.

This is the single-connection engine: folders and messages are copied in
sequence. Concurrency arrives in a later release.`,
		Example: `  # Straight from flags
  imapsync-go sync \
      --source-url imaps://you@imap.mail.me.com --source-password-env ICLOUD_APP_PW \
      --dest-url imaps://you@mox.example.net   --dest-password-keychain mox-imap

  # See what would happen without writing anything
  imapsync-go sync --config imapsync.yaml --pair icloud-to-mox --dry-run

  # One folder only, mapped somewhere specific
  imapsync-go sync --config imapsync.yaml --folder INBOX --map 'INBOX=Archive/Old'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSync(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	cmd.Flags().StringVar(&f.configPath, "config", "", "path to a configuration file")
	cmd.Flags().StringVar(&f.pair, "pair", "", "pair name within the configuration file")

	cmd.Flags().StringVar(&f.sourceURL, "source-url", "", "source endpoint URL, for example imaps://user@host:993")
	cmd.Flags().StringVar(&f.sourcePasswordEnv, "source-password-env", "", "environment variable holding the source password")
	cmd.Flags().StringVar(&f.sourcePasswordFile, "source-password-file", "", "file holding the source password")
	cmd.Flags().StringVar(&f.sourcePasswordKeychain, "source-password-keychain", "", "macOS keychain service name holding the source password")

	cmd.Flags().StringVar(&f.destURL, "dest-url", "", "destination endpoint URL")
	cmd.Flags().StringVar(&f.destPasswordEnv, "dest-password-env", "", "environment variable holding the destination password")
	cmd.Flags().StringVar(&f.destPasswordFile, "dest-password-file", "", "file holding the destination password")
	cmd.Flags().StringVar(&f.destPasswordKeychain, "dest-password-keychain", "", "macOS keychain service name holding the destination password")

	cmd.Flags().StringVar(&f.statePath, "state", "", "path to the state database (default: per-user application state directory)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "report what would be copied without writing anything")

	cmd.Flags().StringSliceVar(&f.only, "folder", nil, "copy only these source folders, by exact name (repeatable)")
	cmd.Flags().StringSliceVar(&f.include, "include", nil, "copy only source folders matching this regular expression (repeatable)")
	cmd.Flags().StringSliceVar(&f.exclude, "exclude", nil, "skip source folders matching this regular expression (repeatable)")
	cmd.Flags().StringArrayVar(&f.mappings, "map", nil, "explicit folder mapping as source=dest (repeatable)")
	cmd.Flags().StringVar(&f.subfolder, "subfolder2", "", "nest the whole copied tree under this destination folder")
	cmd.Flags().BoolVar(&f.automap, "automap", true, "map special folders such as Sent and Trash onto the destination's own names")
	cmd.Flags().BoolVar(&f.includeVirtual, "include-virtual", false, "copy virtual mailboxes such as Gmail's All Mail, which duplicate the account")

	cmd.Flags().DurationVar(&f.dialTimeout, "dial-timeout", 30*time.Second, "connection establishment timeout")
	cmd.Flags().BoolVar(&f.insecure, "insecure", false, "skip TLS certificate verification (test use only)")
	cmd.Flags().BoolVar(&f.trace, "trace", false, "print the raw IMAP conversation to stderr, with credentials redacted")

	cmd.MarkFlagsMutuallyExclusive("config", "source-url")
	cmd.MarkFlagsMutuallyExclusive("source-password-env", "source-password-file", "source-password-keychain")
	cmd.MarkFlagsMutuallyExclusive("dest-password-env", "dest-password-file", "dest-password-keychain")

	return cmd
}

func runSync(ctx context.Context, out io.Writer, f syncFlags) error {
	source, dest, folders, pairName, err := syncEndpoints(f)
	if err != nil {
		return err
	}

	opts, err := folderOptions(f, folders)
	if err != nil {
		return err
	}

	dbPath, err := statePath(f.statePath)
	if err != nil {
		return err
	}
	db, err := state.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("opening state database: %w", err)
	}
	defer func() { _ = db.Close() }()
	slog.Info("state database ready", "path", dbPath)

	var trace io.Writer
	if f.trace {
		trace = os.Stderr
	}

	src, err := connect(ctx, source, f, trace)
	if err != nil {
		return fmt.Errorf("connecting to source: %w", err)
	}
	defer closeConn(ctx, src, "source")

	dst, err := connect(ctx, dest, f, trace)
	if err != nil {
		return fmt.Errorf("connecting to destination: %w", err)
	}
	defer closeConn(ctx, dst, "dest")

	if pairName == "" {
		pairName, err = derivePairID(source, dest)
		if err != nil {
			return err
		}
	}

	started := time.Now()
	report, err := syncer.New(src, dst, db, syncer.Options{
		PairID:  pairName,
		Folders: opts,
		DryRun:  f.dryRun,
		Logger:  slog.Default(),
	}).Run(ctx)
	if err != nil {
		return err
	}

	writeErr := writeSyncReport(out, report, time.Since(started), f.dryRun)

	// The report is written before any of these are returned: knowing which
	// folders failed is more useful than the error itself.
	if _, _, failed := report.Totals(); failed > 0 {
		return fmt.Errorf("%d messages could not be copied", failed)
	}
	for _, fr := range report.Folders {
		if fr.Err != nil {
			return fmt.Errorf("folder %q: %w", fr.Source, fr.Err)
		}
	}
	return writeErr
}

// syncEndpoints resolves the two sides, from a config file or from flags.
func syncEndpoints(f syncFlags) (source, dest config.Endpoint, folders config.Folders, pairName string, err error) {
	if f.configPath != "" {
		cfg, loadErr := config.Load(f.configPath)
		if loadErr != nil {
			return source, dest, folders, "", loadErr
		}

		pair := &cfg.Pairs[0]
		if f.pair != "" {
			if pair, err = cfg.Pair(f.pair); err != nil {
				return source, dest, folders, "", err
			}
		} else if len(cfg.Pairs) > 1 {
			return source, dest, folders, "", fmt.Errorf("config defines %d pairs, select one with --pair", len(cfg.Pairs))
		}
		return pair.Source, pair.Dest, pair.Folders, pair.Name, nil
	}

	if f.sourceURL == "" || f.destURL == "" {
		return source, dest, folders, "", errors.New("provide either --config or both --source-url and --dest-url")
	}

	source = config.Endpoint{
		URL:      f.sourceURL,
		Password: config.Secret{Env: f.sourcePasswordEnv, File: f.sourcePasswordFile, Keychain: f.sourcePasswordKeychain},
	}
	dest = config.Endpoint{
		URL:      f.destURL,
		Password: config.Secret{Env: f.destPasswordEnv, File: f.destPasswordFile, Keychain: f.destPasswordKeychain},
	}
	for label, ep := range map[string]config.Endpoint{"source": source, "dest": dest} {
		if err := ep.Password.Validate(); err != nil {
			return source, dest, folders, "", fmt.Errorf("%s password: %w", label, err)
		}
		if _, err := ep.Address(); err != nil {
			return source, dest, folders, "", fmt.Errorf("--%s-url: %w", label, err)
		}
	}
	return source, dest, folders, "", nil
}

// folderOptions merges the configuration file's folder rules with the flags.
func folderOptions(f syncFlags, cfg config.Folders) (folder.Options, error) {
	opts := folder.Options{
		Automap:        f.automap || cfg.Map == config.MapSpecialUse,
		IncludeVirtual: f.includeVirtual,
		Only:           f.only,
		DestSubfolder:  f.subfolder,
		Mappings:       make(map[string]string),
	}

	for _, rule := range cfg.Rules {
		opts.Mappings[rule.From] = rule.To
	}
	for _, m := range f.mappings {
		from, to, ok := strings.Cut(m, "=")
		if !ok || from == "" {
			return opts, fmt.Errorf("invalid --map %q, want source=dest", m)
		}
		opts.Mappings[from] = to
	}

	var err error
	if opts.Include, err = compilePatterns(append(cfg.Include, f.include...), "include"); err != nil {
		return opts, err
	}
	if opts.Exclude, err = compilePatterns(append(cfg.Exclude, f.exclude...), "exclude"); err != nil {
		return opts, err
	}
	return opts, nil
}

func compilePatterns(patterns []string, label string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid --%s pattern %q: %w", label, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func connect(ctx context.Context, ep config.Endpoint, f syncFlags, trace io.Writer) (imapx.Conn, error) {
	addr, err := ep.Address()
	if err != nil {
		return nil, err
	}
	password, err := ep.Password.Resolve()
	if err != nil {
		return nil, err
	}
	return imapx.Dial(ctx, imapx.DialOptions{
		Addr:               addr,
		Password:           password,
		DebugWriter:        trace,
		Timeout:            f.dialTimeout,
		InsecureSkipVerify: f.insecure,
	})
}

func closeConn(ctx context.Context, c imapx.Conn, label string) {
	// LOGOUT is the polite ending and lets a server release the session
	// promptly, which matters on accounts with a low connection ceiling. A
	// failure here cannot affect what was already copied.
	if err := c.Logout(ctx); err != nil {
		slog.Debug("logout failed", "side", label, "error", err)
	}
	_ = c.Close()
}

// derivePairID names this migration in the state database.
//
// The name has to depend on both endpoints. One database holding two migrations
// that share a name would treat one's progress as the other's and skip messages
// it never copied.
func derivePairID(source, dest config.Endpoint) (string, error) {
	src, err := source.Address()
	if err != nil {
		return "", err
	}
	dst, err := dest.Address()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s@%s>%s@%s", src.User, src.Host, dst.User, dst.Host), nil
}

// statePath resolves where progress is recorded.
func statePath(override string) (string, error) {
	if override != "" {
		if dir := filepath.Dir(override); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", fmt.Errorf("creating state directory: %w", err)
			}
		}
		return override, nil
	}

	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the state directory: %w", err)
	}
	dir := filepath.Join(base, "imapsync-go")
	// 0700 because the file records which messages exist in someone's mail.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating state directory: %w", err)
	}
	return filepath.Join(dir, "state.db"), nil
}

func writeSyncReport(out io.Writer, report syncer.Report, elapsed time.Duration, dryRun bool) error {
	p := &printer{w: out}

	if dryRun {
		p.println("DRY RUN — nothing was written")
		p.println()
	}

	if len(report.Created) > 0 {
		verb := "Created"
		if dryRun {
			verb = "Would create"
		}
		p.printf("%s %d destination %s: %s\n\n",
			verb, len(report.Created), plural(len(report.Created), "folder"), strings.Join(report.Created, ", "))
	}

	t, flush := p.table()
	copiedHeading := "COPIED"
	if dryRun {
		copiedHeading = "TO COPY"
	}
	t.printf("SOURCE\tDESTINATION\tMESSAGES\t%s\tADOPTED\tALREADY\tFAILED\n", copiedHeading)
	for _, fr := range report.Folders {
		status := ""
		if fr.Err != nil {
			status = "  ← " + fr.Err.Error()
		}
		t.printf("%s\t%s\t%d\t%d\t%d\t%d\t%d%s\n",
			fr.Source, fr.Dest, fr.Messages, fr.Copied, fr.Adopted, fr.AlreadyDone, fr.Failed, status)
	}
	flush()

	copied, adopted, failed := report.Totals()
	p.printf("\n%d %s, %d copied, %d adopted, %d failed, in %s\n",
		len(report.Folders), plural(len(report.Folders), "folder"), copied, adopted, failed, elapsed.Round(time.Millisecond))

	if len(report.Skips) > 0 {
		p.printf("\nSkipped %d %s:\n", len(report.Skips), plural(len(report.Skips), "folder"))
		st, flushSkips := p.table()
		for _, skip := range report.Skips {
			st.printf("  %s\t%s\n", skip.Source, skip.Reason)
		}
		flushSkips()
	}

	for _, fr := range report.Folders {
		for _, msg := range fr.Errors {
			p.printf("  %s: %s\n", fr.Source, msg)
		}
	}
	return p.err
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
