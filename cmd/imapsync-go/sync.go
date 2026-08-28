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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/hilli/imapsync-go/internal/budget"
	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/folder"
	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/pool"
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
	insecureSrc bool
	insecureDst bool
	trace       bool

	srcConns    int
	dstConns    int
	memoryLimit string

	progressEvery time.Duration
	full          bool
	resyncFlags   bool
	noResyncFlags bool

	delete2       bool
	deleteCeiling float64
	force         bool
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

Copying runs over several connections at once, across folders and within a
single large one. Servers differ enormously in how many connections they will
tolerate, so raise --source-connections and --dest-connections gradually and
watch for authentication failures.`,
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

	cmd.Flags().IntVar(&f.srcConns, "source-connections", 4, "connections to open to the source")
	cmd.Flags().IntVar(&f.dstConns, "dest-connections", 8, "connections to open to the destination")
	cmd.Flags().StringVar(&f.memoryLimit, "memory-limit", "256MiB", "how much message data may be held in memory at once")

	cmd.Flags().BoolVar(&f.full, "full", false, "examine every folder, even ones the server says have not changed")
	// Both spellings, because imapsync has both and this is meant to be a
	// drop-in. The positive one carries the default so --help states it.
	cmd.Flags().BoolVar(&f.resyncFlags, "resyncflags", true, "bring flags on already-copied messages back into line with the source")
	cmd.Flags().BoolVar(&f.noResyncFlags, "noresyncflags", false, "leave flags on already-copied messages alone")
	cmd.Flags().BoolVar(&f.delete2, "delete2", false, "delete destination messages whose source counterpart is gone")
	cmd.Flags().Float64Var(&f.deleteCeiling, "delete2-ceiling", 0.10, "refuse to delete more than this fraction of a folder's copied messages in one run")
	cmd.Flags().BoolVar(&f.force, "force", false, "carry out deletions the ceiling would otherwise refuse")
	cmd.Flags().DurationVar(&f.progressEvery, "progress-interval", 30*time.Second, "how often to report what the sync has done so far; 0 to keep quiet")

	cmd.Flags().DurationVar(&f.dialTimeout, "dial-timeout", 30*time.Second, "connection establishment timeout")
	cmd.Flags().BoolVar(&f.insecureSrc, "source-insecure", false, "skip TLS certificate verification for the source (test use only)")
	cmd.Flags().BoolVar(&f.insecureDst, "dest-insecure", false, "skip TLS certificate verification for the destination, for example a self-signed server on your own network")
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

	limit, err := parseBytes(f.memoryLimit)
	if err != nil {
		return fmt.Errorf("invalid --memory-limit: %w", err)
	}
	bytesInFlight, err := budget.New(limit)
	if err != nil {
		return fmt.Errorf("invalid --memory-limit: %w", err)
	}

	// EXAMINE on the source: reading a message must not mark it \Seen, which
	// would rewrite an account this tool is only supposed to read.
	srcPool, err := pool.New(pool.Options{
		Cap:    f.srcConns,
		Dial:   dialer(source, f, f.insecureSrc, trace),
		Select: imapx.SelectOptions{ReadOnly: true},
	})
	if err != nil {
		return fmt.Errorf("source connections: %w", err)
	}
	defer closePool(ctx, srcPool, "source")

	dstPool, err := pool.New(pool.Options{
		Cap:  f.dstConns,
		Dial: dialer(dest, f, f.insecureDst, trace),
	})
	if err != nil {
		return fmt.Errorf("destination connections: %w", err)
	}
	defer closePool(ctx, dstPool, "dest")

	if pairName == "" {
		pairName, err = derivePairID(source, dest)
		if err != nil {
			return err
		}
	}

	started := time.Now()
	report, err := syncer.New(srcPool, dstPool, db, bytesInFlight, syncer.Options{
		PairID:        pairName,
		Folders:       opts,
		DryRun:        f.dryRun,
		Full:          f.full,
		NoResyncFlags: f.noResyncFlags || !f.resyncFlags,
		Delete2:       f.delete2,
		DeleteCeiling: f.deleteCeiling,
		Force:         f.force,
		ProgressEvery: f.progressEvery,
		Logger:        slog.Default(),
	}).Run(ctx)

	writeErr := writeSyncReport(out, report, time.Since(started), f.dryRun)

	// A run that ended badly still copied something, and after an interruption
	// or a run the engine gave up on, what was copied is the thing worth
	// knowing: it is what the next run will not have to do again. So the
	// report is written first and the error returned after it.
	if err != nil {
		return err
	}

	// The report is written before any of these are returned too: knowing which
	// folders failed is more useful than the error itself.
	if _, _, failed := report.Totals(); failed > 0 {
		return fmt.Errorf("%d messages could not be copied", failed)
	}
	for _, fr := range report.Folders {
		if fr.Err != nil {
			return fmt.Errorf("folder %q: %w", fr.Source, fr.Err)
		}
	}
	// A refusal is not a failure — nothing went wrong and nothing was lost —
	// but it is a run that did not do what was asked of it, and a zero exit
	// would let a scheduled sync report success for months while a folder
	// quietly stopped being mirrored. The message says what to do about it.
	if refused := report.Refused(); refused > 0 {
		return fmt.Errorf("refused to delete %d %s: see above", refused, plural(refused, "message"))
	}
	return writeErr
}

// writeFolderTable prints the per-folder counts.
//
// Split out from writeSyncReport only because the columns that appear
// conditionally — vanished, deleted, refused — each cost two branches, and
// together they took the report past what the linter will accept in one
// function.
func writeFolderTable(p *printer, report syncer.Report, dryRun bool) {
	t, flush := p.table()
	copiedHeading := "COPIED"
	if dryRun {
		copiedHeading = "TO COPY"
	}
	// The vanished column earns its place only when there is something in it.
	// Most servers never produce one, and a column of zeroes on every report
	// would be a worse trade than the occasional wider table.
	vanished := report.Vanished()
	gone := ""
	if vanished > 0 {
		gone = "\tVANISHED"
	}
	// Likewise deletion: the column appears when the run was allowed to delete,
	// not only when it did, because "--delete2 and nothing went" is itself the
	// answer to the question the flag asks.
	deleted, refused := report.Deleted(), report.Refused()
	removals := ""
	if deleted > 0 || refused > 0 {
		removals = "\tDELETED"
		if refused > 0 {
			removals += "\tREFUSED"
		}
	}
	deletedHeading := removals
	if dryRun {
		deletedHeading = strings.Replace(removals, "\tDELETED", "\tTO DELETE", 1)
	}
	t.printf("SOURCE\tDESTINATION\tMESSAGES\t%s\tADOPTED\tALREADY%s%s\tFAILED\n", copiedHeading, gone, deletedHeading)
	for _, fr := range report.Folders {
		status := ""
		if fr.Err != nil {
			status = "  ← " + fr.Err.Error()
		}
		if vanished > 0 {
			gone = fmt.Sprintf("\t%d", fr.Vanished)
		}
		if removals != "" {
			removals = fmt.Sprintf("\t%d", fr.Deleted)
			if refused > 0 {
				removals += fmt.Sprintf("\t%d", fr.Refused)
			}
		}
		t.printf("%s\t%s\t%d\t%d\t%d\t%d%s%s\t%d%s\n",
			fr.Source, fr.Dest, fr.Messages, fr.Copied, fr.Adopted, fr.AlreadyDone, gone, removals, fr.Failed, status)
	}
	flush()
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

// connect dials one side. insecure is passed per side rather than taken from
// the flags because the two sides routinely differ: a self-signed destination
// on your own network is a reasonable thing to accept, and accepting it must
// not also stop verifying a public source over the internet.
// dialer builds the function a pool calls to open one more connection.
//
// The password is resolved once, here, rather than on every dial: a keychain
// lookup can prompt, and a pool that grows to thirty connections should not
// prompt thirty times.
func dialer(ep config.Endpoint, f syncFlags, insecure bool, trace io.Writer) pool.DialFunc {
	var (
		once     sync.Once
		addr     config.Address
		password string
		resolve  error
	)
	return func(ctx context.Context) (imapx.Conn, error) {
		once.Do(func() {
			if addr, resolve = ep.Address(); resolve != nil {
				return
			}
			password, resolve = ep.Password.Resolve()
		})
		if resolve != nil {
			return nil, resolve
		}
		return imapx.Dial(ctx, imapx.DialOptions{
			Addr:               addr,
			Password:           password,
			DebugWriter:        trace,
			Timeout:            f.dialTimeout,
			InsecureSkipVerify: insecure,
		})
	}
}

func closePool(ctx context.Context, p *pool.Pool, label string) {
	// LOGOUT is the polite ending and lets a server release the session
	// promptly, which matters on accounts with a low connection ceiling. A
	// failure here cannot affect what was already copied.
	if err := p.Close(ctx); err != nil {
		slog.Debug("closing connections failed", "side", label, "error", err)
	}
}

// parseBytes reads a size written the way people write sizes.
func parseBytes(s string) (int64, error) {
	t := strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	}
	mult := int64(1)
	for _, u := range units {
		if rest, ok := strings.CutSuffix(t, u.suffix); ok {
			t, mult = strings.TrimSpace(rest), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size such as 256MiB", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%q must be greater than zero", s)
	}
	return n * mult, nil
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

	vanished := report.Vanished()
	deleted, refused := report.Deleted(), report.Refused()
	writeFolderTable(p, report, dryRun)

	copied, adopted, failed := report.Totals()
	copiedWord := "copied"
	if dryRun {
		copiedWord = "to copy"
	}
	goneWord := ""
	if vanished > 0 {
		goneWord = fmt.Sprintf("%d vanished, ", vanished)
	}
	if deleted > 0 {
		verb := "deleted"
		if dryRun {
			verb = "to delete"
		}
		goneWord += fmt.Sprintf("%d %s, ", deleted, verb)
	}
	p.printf("\n%d %s, %d %s, %d adopted, %s%d failed, in %s%s\n",
		len(report.Folders), plural(len(report.Folders), "folder"), copied, copiedWord, adopted, goneWord, failed,
		elapsed.Round(time.Millisecond), rate(copied, elapsed, dryRun))

	if vanished > 0 {
		p.printf("\n%d %s the source listed but had no message for. Nothing was lost:\nthere is nothing at those numbers to copy, and they will not be asked for again.\n",
			vanished, plural(vanished, "UID"))
	}

	// A refusal is the one thing here that asks the reader to do something, so
	// it says which folders and what to do about it rather than leaving a
	// number in a column to be noticed.
	if refused > 0 {
		p.printf("\nREFUSED to delete %d %s. That is a larger share of a folder's copied\nmessages than --delete2-ceiling allows to go in one run, and the usual cause is a\nsource that answered a listing with less than the truth.\n",
			refused, plural(refused, "message"))
		for _, fr := range report.Folders {
			if fr.Refused > 0 {
				p.printf("  %s: %d of %d\n", fr.Dest, fr.Refused, fr.Refused+fr.AlreadyDone+fr.Copied)
			}
		}
		p.println("\nCheck the source, then pass --force to go ahead, or raise --delete2-ceiling.")
	}

	// Skips the caller asked for are not news: narrowing a 144-folder account
	// to two with --folder should not bury the two skips that were our own
	// decision under 142 that were theirs.
	var notable []folder.Skip
	byRequest := 0
	for _, skip := range report.Skips {
		if skip.ByRequest {
			byRequest++
			continue
		}
		notable = append(notable, skip)
	}

	if len(notable) > 0 {
		p.printf("\nSkipped %d %s:\n", len(notable), plural(len(notable), "folder"))
		st, flushSkips := p.table()
		for _, skip := range notable {
			st.printf("  %s\t%s\n", skip.Source, skip.Reason)
		}
		flushSkips()
	}
	if byRequest > 0 {
		p.printf("\n%d further %s left out by --folder, --include or --exclude.\n",
			byRequest, plural(byRequest, "folder"))
	}

	for _, fr := range report.Folders {
		for _, msg := range fr.Errors {
			p.printf("  %s: %s\n", fr.Source, msg)
		}
	}
	return p.err
}

// rate reports how fast messages were copied.
//
// It is the number that says whether a run of 776,747 messages will take days
// or hours, and the only way to tell whether raising --source-connections
// helped. Adopted and already-recorded messages are left out: they cost a
// header comparison rather than a transfer, and counting them would flatter a
// re-run into looking like a fast copy.
func rate(copied int, elapsed time.Duration, dryRun bool) string {
	if dryRun || copied == 0 || elapsed <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%.1f messages/second)", float64(copied)/elapsed.Seconds())
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
