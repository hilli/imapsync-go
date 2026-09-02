package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/hilli/imapsync-go/internal/localstore"
	"github.com/hilli/imapsync-go/internal/pool"
	"github.com/hilli/imapsync-go/internal/searchkey"
	"github.com/hilli/imapsync-go/internal/selection"
	"github.com/hilli/imapsync-go/internal/state"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// The two sides, named once. They reach the user in three different places —
// log lines, the connection flags, and probe's --side — so a typo in any one of
// them would be a lie that still compiled.
const (
	sideSource = "source"
	sideDest   = "dest"
)

type syncFlags struct {
	configPath string
	pair       string

	sourceURL              string
	sourcePasswordEnv      string
	sourcePasswordFile     string
	sourcePasswordKeychain string
	sourceOAuthCmd         string
	sourceOAuthFile        string
	sourceOAuthRefreshEnv  string
	sourceOAuthRefreshFile string
	sourceOAuthRefreshKey  string

	destURL              string
	destPasswordEnv      string
	destPasswordFile     string
	destPasswordKeychain string
	destOAuthCmd         string
	destOAuthFile        string
	destOAuthRefreshEnv  string
	destOAuthRefreshFile string
	destOAuthRefreshKey  string

	statePath string
	dryRun    bool

	only           []string
	include        []string
	exclude        []string
	mappings       []string
	subfolder      string
	automap        bool
	includeVirtual bool

	maxSize  string
	minSize  string
	maxAge   string
	minAge   string
	ageBasis string

	sourceSearch string
	destSearch   string

	dialTimeout time.Duration
	insecureSrc bool
	insecureDst bool
	trace       bool

	srcConns    int
	dstConns    int
	memoryLimit string

	// Whether the three above were named on the command line. A flag that was
	// not given has no opinion, and the config file's answer stands; a flag
	// that was given overrides it. Without this the flag defaults would be
	// indistinguishable from a deliberate 4, and the config could never win.
	srcConnsSet    bool
	dstConnsSet    bool
	memoryLimitSet bool
	delete2Set     bool

	progressEvery  time.Duration
	full           bool
	resyncFlags    bool
	noResyncFlags  bool
	verifyDest     bool
	syncDuplicates bool
	subscribe      bool

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
			f.srcConnsSet = cmd.Flags().Changed("source-connections")
			f.dstConnsSet = cmd.Flags().Changed("dest-connections")
			f.memoryLimitSet = cmd.Flags().Changed("memory-limit")
			f.delete2Set = cmd.Flags().Changed("delete2")
			return runSync(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	cmd.Flags().StringVar(&f.configPath, "config", "", "path to a configuration file")
	cmd.Flags().StringVar(&f.pair, "pair", "", "pair name within the configuration file")

	cmd.Flags().StringVar(&f.sourceURL, "source-url", "", "source endpoint URL, for example imaps://user@host:993")
	cmd.Flags().StringVar(&f.sourcePasswordEnv, "source-password-env", "", "environment variable holding the source password")
	cmd.Flags().StringVar(&f.sourcePasswordFile, "source-password-file", "", "file holding the source password")
	cmd.Flags().StringVar(&f.sourcePasswordKeychain, "source-password-keychain", "", "macOS keychain service name holding the source password")
	cmd.Flags().StringVar(&f.sourceOAuthCmd, "source-oauth-cmd", "", "command printing an OAuth access token for the source, re-run when the server refuses the one held")
	cmd.Flags().StringVar(&f.sourceOAuthFile, "source-oauth-file", "", "file holding an OAuth access token for the source, re-read when the server refuses the one held")
	cmd.Flags().StringVar(&f.sourceOAuthRefreshEnv, "source-oauth-refresh-env", "", "environment variable holding the source OAuth credential written by `oauth login`")
	cmd.Flags().StringVar(&f.sourceOAuthRefreshFile, "source-oauth-refresh-file", "", "file holding the source OAuth credential written by `oauth login`")
	cmd.Flags().StringVar(&f.sourceOAuthRefreshKey, "source-oauth-refresh-keychain", "", "macOS keychain service name holding the source OAuth credential written by `oauth login`")

	cmd.Flags().StringVar(&f.destURL, "dest-url", "", "destination endpoint URL")
	cmd.Flags().StringVar(&f.destPasswordEnv, "dest-password-env", "", "environment variable holding the destination password")
	cmd.Flags().StringVar(&f.destPasswordFile, "dest-password-file", "", "file holding the destination password")
	cmd.Flags().StringVar(&f.destPasswordKeychain, "dest-password-keychain", "", "macOS keychain service name holding the destination password")
	cmd.Flags().StringVar(&f.destOAuthCmd, "dest-oauth-cmd", "", "command printing an OAuth access token for the destination")
	cmd.Flags().StringVar(&f.destOAuthFile, "dest-oauth-file", "", "file holding an OAuth access token for the destination")
	cmd.Flags().StringVar(&f.destOAuthRefreshEnv, "dest-oauth-refresh-env", "", "environment variable holding the destination OAuth credential written by `oauth login`")
	cmd.Flags().StringVar(&f.destOAuthRefreshFile, "dest-oauth-refresh-file", "", "file holding the destination OAuth credential written by `oauth login`")
	cmd.Flags().StringVar(&f.destOAuthRefreshKey, "dest-oauth-refresh-keychain", "", "macOS keychain service name holding the destination OAuth credential written by `oauth login`")

	cmd.Flags().StringVar(&f.statePath, "state", "", "path to the state database (default: per-user application state directory)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "report what would be copied without writing anything")

	cmd.Flags().StringSliceVar(&f.only, "folder", nil, "copy only these source folders, by exact name (repeatable)")
	cmd.Flags().StringSliceVar(&f.include, "include", nil, "copy only source folders matching this regular expression (repeatable)")
	cmd.Flags().StringSliceVar(&f.exclude, "exclude", nil, "skip source folders matching this regular expression (repeatable)")
	cmd.Flags().StringArrayVar(&f.mappings, "map", nil, "explicit folder mapping as source=dest (repeatable)")
	cmd.Flags().StringVar(&f.subfolder, "subfolder2", "", "nest the whole copied tree under this destination folder")
	cmd.Flags().BoolVar(&f.automap, "automap", true, "map special folders such as Sent and Trash onto the destination's own names")
	cmd.Flags().BoolVar(&f.includeVirtual, "include-virtual", false, "copy virtual mailboxes such as Gmail's All Mail, which duplicate the account")

	cmd.Flags().StringVar(&f.maxSize, "max-size", "", "skip messages this large or larger, for example 25MiB")
	cmd.Flags().StringVar(&f.minSize, "min-size", "", "skip messages this small or smaller")
	cmd.Flags().StringVar(&f.maxAge, "max-age", "", "skip messages older than this, for example 30d")
	cmd.Flags().StringVar(&f.minAge, "min-age", "", "skip messages newer than this")
	cmd.Flags().StringVar(&f.ageBasis, "age-basis", "sent", `which date --max-age and --min-age measure from: "sent" (the Date: header) or "internal" (arrival in the mailbox)`)
	cmd.Flags().StringVar(&f.sourceSearch, "source-search", "", `copy only source messages matching this IMAP SEARCH, for example "UNSEEN SMALLER 100000"`)
	cmd.Flags().StringVar(&f.destSearch, "dest-search", "", "consider only destination messages matching this IMAP SEARCH for deletion by --delete2")

	cmd.Flags().IntVar(&f.srcConns, "source-connections", autoConnections, "connections to open to the source; overrides the config's concurrency.source")
	cmd.Flags().IntVar(&f.dstConns, "dest-connections", autoConnections, "connections to open to the destination; overrides the config's concurrency.dest")
	cmd.Flags().StringVar(&f.memoryLimit, "memory-limit", "256MiB", "how much message data may be held in memory at once; overrides the config's concurrency.max_inflight")

	cmd.Flags().BoolVar(&f.full, "full", false, "examine every folder, even ones the server says have not changed")
	// Both spellings, because imapsync has both and this is meant to be a
	// drop-in. The positive one carries the default so --help states it.
	cmd.Flags().BoolVar(&f.resyncFlags, "resyncflags", true, "bring flags on already-copied messages back into line with the source")
	cmd.Flags().BoolVar(&f.noResyncFlags, "noresyncflags", false, "leave flags on already-copied messages alone")
	// Only the positive spelling, unlike the pair above: that pair exists
	// because imapsync has both, and this option is ours. --verify-dest=false
	// is the way off.
	cmd.Flags().BoolVar(&f.verifyDest, "verify-dest", true, "check the destination still holds the copies the state database recorded, and copy back any it does not")
	cmd.Flags().BoolVar(&f.syncDuplicates, "sync-duplicates", false, "copy a folder's repeated messages once each instead of once in total")
	cmd.Flags().BoolVar(&f.subscribe, "subscribe", true, "subscribe to destination folders as they are created, so clients show them")
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
	cmd.MarkFlagsMutuallyExclusive("source-oauth-cmd", "source-oauth-file",
		"source-oauth-refresh-env", "source-oauth-refresh-file", "source-oauth-refresh-keychain")
	cmd.MarkFlagsMutuallyExclusive("dest-oauth-cmd", "dest-oauth-file",
		"dest-oauth-refresh-env", "dest-oauth-refresh-file", "dest-oauth-refresh-keychain")

	return cmd
}

func runSync(ctx context.Context, out io.Writer, f syncFlags) error {
	pair, err := syncPair(f)
	if err != nil {
		return err
	}
	source, dest, pairName := pair.Source, pair.Dest, pair.Name

	opts, err := folderOptions(f, pair.Folders)
	if err != nil {
		return err
	}

	messages, err := messageFilter(f)
	if err != nil {
		return err
	}

	sourceSearch, destSearch, err := searchFlags(f)
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

	srcConns, dstConns, limit, err := resolveConcurrency(f, pair.Concurrency)
	if err != nil {
		return err
	}

	bytesInFlight, err := budget.New(limit)
	if err != nil {
		return fmt.Errorf("invalid --memory-limit: %w", err)
	}

	srcPool, dstPool, release, err := pools(ctx, pair, f, trace, srcConns, dstConns)
	if err != nil {
		return err
	}
	defer release()

	if pairName == "" {
		pairName, err = derivePairID(source, dest)
		if err != nil {
			return err
		}
	}

	started := time.Now()
	report, err := syncer.New(srcPool, dstPool, db, bytesInFlight, syncer.Options{
		PairID:         pairName,
		Folders:        opts,
		DryRun:         f.dryRun,
		Full:           f.full,
		Filter:         messages,
		SourceSearch:   sourceSearch,
		DestSearch:     destSearch,
		NoResyncFlags:  f.noResyncFlags || !f.resyncFlags,
		NoVerifyDest:   !f.verifyDest,
		SyncDuplicates: f.syncDuplicates,
		NoSubscribe:    !f.subscribe,
		Delete2:        resolveDelete2(f, pair),
		DeleteCeiling:  f.deleteCeiling,
		Force:          f.force,
		ProgressEvery:  f.progressEvery,
		Logger:         slog.Default(),
	}).Run(ctx)

	writeErr := writeSyncReport(out, report, time.Since(started), f.dryRun, connections{
		{"source", sideSource + "-connections", srcConns, srcPool.Width()},
		{"destination", sideDest + "-connections", dstConns, dstPool.Width()},
	})

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
	// Filtered is its own column rather than being folded into vanished. The
	// two are both "not copied and not a failure", but a vanished message is
	// settled for good while a filtered one is a message the run declined and
	// may take later, and a reader deciding whether their --max-age is doing
	// what they meant needs to see it apart from everything else.
	filtered := report.Filtered()
	left := ""
	if filtered > 0 {
		left = "\tFILTERED"
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
	t.printf("SOURCE\tDESTINATION\tMESSAGES\t%s\tADOPTED\tALREADY%s%s%s\tFAILED\n",
		copiedHeading, gone, left, deletedHeading)
	for _, fr := range report.Folders {
		status := ""
		if fr.Err != nil {
			status = "  ← " + fr.Err.Error()
		}
		if vanished > 0 {
			gone = fmt.Sprintf("\t%d", fr.Vanished)
		}
		if filtered > 0 {
			left = fmt.Sprintf("\t%d", fr.Filtered)
		}
		if removals != "" {
			removals = fmt.Sprintf("\t%d", fr.Deleted)
			if refused > 0 {
				removals += fmt.Sprintf("\t%d", fr.Refused)
			}
		}
		t.printf("%s\t%s\t%d\t%d\t%d\t%d%s%s%s\t%d%s\n",
			fr.Source, fr.Dest, fr.Messages, fr.Copied, fr.Adopted, fr.AlreadyDone,
			gone, left, removals, fr.Failed, status)
	}
	flush()
}

// writeUncopiedNotes explains the messages that were neither copied nor failed.
//
// Both counts sit in columns of their own, and a column is not enough. Someone
// reading "0 failed" and stopping there would conclude the account is mirrored,
// which is exactly what neither of these means — so each says in words what it
// is and whether anything is expected of the reader.
func writeUncopiedNotes(p *printer, report syncer.Report) {
	if vanished := report.Vanished(); vanished > 0 {
		p.printf("\n%d %s the source listed but had no message for. Nothing was lost:\nthere is nothing at those numbers to copy, and they will not be asked for again.\n",
			vanished, plural(vanished, "UID"))
	}
	if filtered := report.Filtered(); filtered > 0 {
		p.printf("\n%d %s left out by the message selection. They are not on the destination\nand are not recorded as copied, so they will be picked up by a later run whose\nselection admits them.\n",
			filtered, plural(filtered, "message"))
	}
}

// writeMissingNote reports copies that had gone from the destination.
//
// A note rather than a column, because on a healthy account it is always zero
// and the table is wide already. It reads as a warning because it is the one
// number in the report that describes something outside this tool deleting
// mail: the run put those messages there, recorded them, and found them gone.
// Repairing it silently would hide a destination that is losing mail.
func writeMissingNote(p *printer, report syncer.Report, dryRun bool) {
	missing := report.Missing()
	if missing == 0 {
		return
	}
	// Agreement is done by hand because both halves of the sentence carry the
	// number: "1 message ... were ... them" reads as a bug in the report, and
	// this report has already been wrong about a run three times.
	verb, object, subject, have := "were", "them", "they", "have"
	if missing == 1 {
		verb, object, subject, have = "was", "it", "it", "has"
	}
	tail := fmt.Sprintf("%s %s been copied again", subject, have)
	if dryRun {
		tail = fmt.Sprintf("a real run would copy %s again", object)
	}
	p.printf("\n%d %s recorded as copied %s no longer on the destination.\nSomething other than this run removed %s, and %s.\n",
		missing, plural(missing, "message"), verb, object, tail)
}

// writeDuplicatesNote reports source messages that repeated one another.
//
// A note rather than a column, because it is zero on almost every account and
// the table is wide already. It is not a warning: nothing went wrong, and the
// destination holds every distinct message the source did.
//
// It says "byte for byte" plainly, because the reader's first question is
// whether mail was dropped on a guess. It was not, and --sync-duplicates is
// named in the note so that anyone who wants every copy can have them.
func writeDuplicatesNote(p *printer, report syncer.Report) {
	n := report.Duplicates()
	if n == 0 {
		return
	}
	// Agreement by hand, as in writeMissingNote: this report has been wrong
	// about its own run often enough to be worth the four words.
	subject, verb, each := "messages were", "were", "each"
	if n == 1 {
		subject, verb, each = "message was", "was", "it"
	}
	p.printf("\n%d source %s byte for byte identical to one already copied and %s not copied a second time.\n"+
		"The destination holds one copy of %s. Use --sync-duplicates to copy every one.\n",
		n, subject, verb, each)
}

// writeHeaderlessNote reports a server that would not return headers.
//
// This reads like a warning because it is one, and nothing else in the run will
// say so: the messages copied, no folder failed, and the exit code is zero. What
// was lost is the ability to recognise those messages again, which stays
// invisible until the day this database is gone and they are copied a second
// time. The run that can still name the server is the run that has to.
func writeHeaderlessNote(p *printer, report syncer.Report) {
	source, dest := report.Headerless()
	total := source + dest
	if total == 0 {
		return
	}
	var sides []string
	if source > 0 {
		sides = append(sides, fmt.Sprintf("%d on the source", source))
	}
	if dest > 0 {
		sides = append(sides, fmt.Sprintf("%d on the destination", dest))
	}
	// The side counts go on a line of their own because their width varies with
	// the numbers and with whether one side or both are named, and a clause of
	// unknown length cannot be hand-wrapped into the middle of a paragraph.
	p.printf("\n%d %s came back with no header at all, though the server gave each a size\n(%s).\nTheir bodies copied normally, but a message with no header cannot be identified,\nso each copy was stamped instead and none can be adopted by digest if this\ndatabase is lost. That is a defect in the server.\n",
		total, plural(total, "message"), strings.Join(sides, ", "))
}

// autoConnections is the width each side opens when nobody has said otherwise.
//
// The governor can only ever give capacity up, so "auto" cannot discover a
// limit from below — it has to start somewhere and be shrunk toward the wall.
// That makes the starting number a judgement about strangers' servers, not
// about any particular one: too low wastes a migration's time, too high spends
// a burst of refused logins on every server that will not hold it, and some
// servers count refused logins against you.
//
// Sixteen is four times the old source default and twice the old destination
// one, and sits under every ceiling this tool has actually measured (mox 30,
// iCloud 48), so it costs a well-provisioned server nothing. A server that
// caps lower — Dovecot ships a limit of 10 per address — refuses the excess
// once and the pool settles, which is the half of the governor that is proven.
//
// It is a floor on ambition rather than an answer. Anyone who cares about
// throughput should run `imapsync-go probe`, which measures the real ceiling,
// and put that number in the config where it will not have to be rediscovered.
const autoConnections = 16

// resolveDelete2 decides whether this run may delete destination messages.
//
// Same rule as the connection widths: a flag named on the command line wins,
// and one that was not leaves the config to speak. This is the one option here
// that destroys mail, so it has to be possible to say --delete2=false against a
// config that says true — which is why it asks whether the flag was *given*
// rather than what its value is. A plain boolean check could only ever turn
// deletion on.
func resolveDelete2(f syncFlags, pair config.Pair) bool {
	if f.delete2Set {
		return f.delete2
	}
	return pair.Delete2
}

// resolveConcurrency decides the two pool widths and the in-flight byte budget.
//
// Three sources of truth, in falling order of precedence: a flag named on the
// command line, the pair's `concurrency:` block, and the built-in default.
// A flag that was not given is not an opinion — otherwise its default would be
// indistinguishable from a deliberate choice and the config could never win.
//
// The config block was parsed and then dropped on the floor for the whole of
// this tool's life, so a pair asking for 40 connections got 4, and the 512 MiB
// `max_inflight` was silently 256 MiB. A knob that does nothing is worse than
// no knob: it is a setting people tune, and measure against, and believe.
func resolveConcurrency(f syncFlags, c config.Concurrency) (src, dst int, inflight int64, err error) {
	pick := func(flagSet bool, flagVal int, limit config.Limit) int {
		switch {
		case flagSet:
			return flagVal
		case !limit.Auto():
			return int(limit)
		default:
			return autoConnections
		}
	}
	src = pick(f.srcConnsSet, f.srcConns, c.Source)
	dst = pick(f.dstConnsSet, f.dstConns, c.Dest)

	switch {
	case f.memoryLimitSet || c.MaxInflight == 0:
		if inflight, err = parseBytes(f.memoryLimit); err != nil {
			return 0, 0, 0, fmt.Errorf("invalid --memory-limit: %w", err)
		}
	default:
		inflight = int64(c.MaxInflight)
	}
	return src, dst, inflight, nil
}

// syncPair resolves the whole pair, from a config file or from flags.
//
// It returns the pair itself rather than a handful of its fields. The previous
// shape returned four of the five, and the two it left behind — `concurrency:`
// and `delete2:` — were silently inert for the life of the tool. Handing back
// the whole thing means a field added to config.Pair arrives here for free,
// and the only way to ignore one is to visibly not use it.
func syncPair(f syncFlags) (config.Pair, error) {
	if f.configPath != "" {
		cfg, err := config.Load(f.configPath)
		if err != nil {
			return config.Pair{}, err
		}

		pair := &cfg.Pairs[0]
		if f.pair != "" {
			if pair, err = cfg.Pair(f.pair); err != nil {
				return config.Pair{}, err
			}
		} else if len(cfg.Pairs) > 1 {
			return config.Pair{}, fmt.Errorf("config defines %d pairs, select one with --pair", len(cfg.Pairs))
		}

		merged := *pair
		merged.Source.OAuth = oauthFrom(merged.Source.OAuth, f.sourceOAuth())
		merged.Dest.OAuth = oauthFrom(merged.Dest.OAuth, f.destOAuth())
		return merged, validateSides(merged, "%s endpoint: %w")
	}

	if f.sourceURL == "" || f.destURL == "" {
		return config.Pair{}, errors.New("provide either --config or both --source-url and --dest-url")
	}

	pair := config.Pair{
		Source: config.Endpoint{
			URL:      f.sourceURL,
			Password: config.Secret{Env: f.sourcePasswordEnv, File: f.sourcePasswordFile, Keychain: f.sourcePasswordKeychain},
			OAuth:    f.sourceOAuth(),
		},
		Dest: config.Endpoint{
			URL:      f.destURL,
			Password: config.Secret{Env: f.destPasswordEnv, File: f.destPasswordFile, Keychain: f.destPasswordKeychain},
			OAuth:    f.destOAuth(),
		},
	}
	return pair, validateSides(pair, "--%s-url: %w")
}

// validateSides checks both endpoints, naming which one failed.
func validateSides(pair config.Pair, format string) error {
	for label, ep := range map[string]config.Endpoint{sideSource: pair.Source, sideDest: pair.Dest} {
		if err := ep.Validate(); err != nil {
			return fmt.Errorf(format, label, err)
		}
	}
	return nil
}

// oauthFrom lets a flag override the configured token source.
//
// A flag naming a source replaces the configured block outright rather than
// merging field by field. Merging would let a flag command sit beside a
// configured file, which is two sources for one credential -- the ambiguity
// validation exists to refuse.
func oauthFrom(configured, flags config.OAuth) config.OAuth {
	if !flags.Set() {
		return configured
	}
	return flags
}

// sourceOAuth and destOAuth read the OAuth flags for one side.
func (f syncFlags) sourceOAuth() config.OAuth {
	return config.OAuth{
		Command: f.sourceOAuthCmd,
		File:    f.sourceOAuthFile,
		Refresh: config.Secret{
			Env:      f.sourceOAuthRefreshEnv,
			File:     f.sourceOAuthRefreshFile,
			Keychain: f.sourceOAuthRefreshKey,
		},
	}
}

func (f syncFlags) destOAuth() config.OAuth {
	return config.OAuth{
		Command: f.destOAuthCmd,
		File:    f.destOAuthFile,
		Refresh: config.Secret{
			Env:      f.destOAuthRefreshEnv,
			File:     f.destOAuthRefreshFile,
			Keychain: f.destOAuthRefreshKey,
		},
	}
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
// pools builds both connection pools and hands back one function that takes
// them down again, in the order they have to come down in.
//
// Both sides are built together because their teardown is interleaved: a local
// store must be settled after the pool that was reading it has returned its
// connections, and getting that order wrong leaves a store an unattended
// backup cannot safely copy.
//
// The unwind closures capture locals rather than the named results on purpose.
// A `return nil, nil, nil, err` assigns the named results *before* the deferred
// unwind runs, so a closure reading them would find the pool it was meant to
// close already nil — closing nothing, and leaking every connection the side
// that did succeed had opened.
func pools(
	ctx context.Context,
	pair config.Pair,
	f syncFlags,
	trace io.Writer,
	srcConns, dstConns int,
) (src, dst *pool.Pool, release func(), err error) {
	var closers []func()
	unwind := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	defer func() {
		if err != nil {
			unwind()
		}
	}()

	srcDial, srcStore, err := endpoint(pair.Source, f, f.insecureSrc, trace, sideSource)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("source: %w", err)
	}
	closers = append(closers, func() { closeStore(srcStore, sideSource) })

	dstDial, dstStore, err := endpoint(pair.Dest, f, f.insecureDst, trace, sideDest)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("destination: %w", err)
	}
	closers = append(closers, func() { closeStore(dstStore, sideDest) })

	// EXAMINE on the source: reading a message must not mark it \Seen, which
	// would rewrite an account this tool is only supposed to read.
	srcPool, err := pool.New(pool.Options{
		Cap:      srcConns,
		Dial:     srcDial,
		Select:   imapx.SelectOptions{ReadOnly: true},
		OnShrink: shrinkLogger(sideSource),
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("source connections: %w", err)
	}
	closers = append(closers, func() { closePool(ctx, srcPool, sideSource) })

	dstPool, err := pool.New(pool.Options{
		Cap:      dstConns,
		Dial:     dstDial,
		OnShrink: shrinkLogger(sideDest),
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("destination connections: %w", err)
	}
	closers = append(closers, func() { closePool(ctx, dstPool, sideDest) })

	return srcPool, dstPool, unwind, nil
}

// endpoint prepares one side of a pair for its pool.
//
// It returns a closer as well as a dial function because a local store has an
// end to its run that a socket does not: its databases have to be
// checkpointed and closed, or the store is left with -wal files beside every
// folder and an unattended backup copying it may copy a transaction in
// progress. Quiescent at rest is a promise the store cannot keep alone.
func endpoint(ep config.Endpoint, f syncFlags, insecure bool, trace io.Writer, side string) (pool.DialFunc, io.Closer, error) {
	if !ep.IsLocal() {
		return dialer(ep, f, insecure, trace), nil, nil
	}

	path, err := ep.LocalPath()
	if err != nil {
		return nil, nil, err
	}

	// A source has to be there already. Creating one on demand turns a mistyped
	// restore path into an empty directory, a report of nothing to do and an
	// exit code of zero — a backup tool telling someone their mail is safe
	// because it looked in the wrong place.
	if side == sideSource {
		if err := mustExist(path); err != nil {
			return nil, nil, err
		}
	}

	// A dry run promises to change nothing, and both halves of this are needed
	// to keep that promise.
	//
	// Where the directory is not there yet, an empty scratch store answers
	// every question the run actually asks — no folders, no messages — and is
	// the truth rather than a stand-in for it.
	//
	// Where it is there, the run has to read it or report a backup it has
	// already made as entirely uncopied. Opening it the ordinary way writes:
	// Open creates an INBOX inside it, and selecting a folder reconciles,
	// which renames stray files and rewrites rows. OpenReadOnly reads the same
	// answers and refuses every write.
	if side == sideDest && f.dryRun {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return scratchStore()
		}
		store, err := localstore.OpenReadOnly(path)
		if err != nil {
			return nil, nil, err
		}
		return store.Connect, store, nil
	}

	store, err := localstore.Open(path)
	if err != nil {
		return nil, nil, err
	}
	// Store.Connect is already a pool.DialFunc: it neither authenticates nor
	// opens a socket, which is the whole reason the pool did not have to
	// change to accommodate a directory.
	return store.Connect, store, nil
}

// mustExist reports a source path that is not there, in the words of the thing
// the user typed.
//
// Only absence is checked here. A path that exists but is not a directory is
// already refused by the store itself, and a second check saying so in nicer
// words is a branch no test can tell from its absence.
func mustExist(path string) error {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s does not exist", path)
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	return nil
}

// scratchStore is an empty destination that leaves nothing behind, for a dry
// run against a directory that does not exist yet.
func scratchStore() (pool.DialFunc, io.Closer, error) {
	dir, err := os.MkdirTemp("", "imapsync-go-dryrun-")
	if err != nil {
		return nil, nil, fmt.Errorf("preparing a scratch destination for the dry run: %w", err)
	}
	store, err := localstore.Open(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	return store.Connect, closerFunc(func() error {
		err := store.Close()
		if rmErr := os.RemoveAll(dir); err == nil {
			err = rmErr
		}
		return err
	}), nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// closeStore settles a local store and says so if it cannot.
//
// A failure here loses no mail — every message is already on disk — but it
// leaves the store in a state a backup cannot safely copy, and that is exactly
// the kind of thing that stays silent until the restore.
func closeStore(c io.Closer, side string) {
	if c == nil {
		return
	}
	if err := c.Close(); err != nil {
		slog.Error("could not settle the local store; a backup taken now may copy it mid-write",
			"side", side, "error", err)
	}
}

func dialer(ep config.Endpoint, f syncFlags, insecure bool, trace io.Writer) pool.DialFunc {
	var (
		once    sync.Once
		addr    config.Address
		cred    imapx.Credential
		resolve error
	)
	return func(ctx context.Context) (imapx.Conn, error) {
		// One credential for the whole pool, not one per dial. A token that
		// each connection resolved for itself would be minted once per
		// connection, and an expiry would be met by every worker separately.
		once.Do(func() {
			if addr, resolve = ep.Address(); resolve != nil {
				return
			}
			cred, resolve = imapx.CredentialFor(ctx, ep)
		})
		if resolve != nil {
			return nil, resolve
		}
		return imapx.Dial(ctx, imapx.DialOptions{
			Addr:               addr,
			Credential:         cred,
			DebugWriter:        trace,
			Timeout:            f.dialTimeout,
			InsecureSkipVerify: insecure,
		})
	}
}

// shrinkLogger reports a pool giving up connections.
//
// Being refused is not a failure and nothing is lost by it, but it is the only
// moment at which a server says what it will actually hold, and a run that
// silently ran at half the requested width would look identical to a slow
// server. So it is said out loud, once per adjustment.
func shrinkLogger(side string) func(from, to int, cause error) {
	return func(from, to int, cause error) {
		slog.Info("server refused another connection, using fewer",
			"side", side, "from", from, "to", to, "cause", cause)
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

// parseAge reads an age written the way people write ages.
//
// Go's own duration syntax stops at hours, because a day is not a fixed number
// of them once daylight saving is involved. That objection does not apply here:
// these bounds are compared against message dates months or years old, where
// an hour either way is noise, and the unit imapsync takes — and therefore the
// unit anybody arriving from it will type — is days. So "30d" is accepted, and
// so is anything time.ParseDuration understands.
func parseAge(s string) (time.Duration, error) {
	t := strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(t, "d"); ok {
		n, err := strconv.ParseFloat(strings.TrimSpace(days), 64)
		if err == nil {
			if n <= 0 {
				return 0, fmt.Errorf("%q must be greater than zero", s)
			}
			return time.Duration(n * float64(selection.Day)), nil
		}
	}
	d, err := time.ParseDuration(t)
	if err != nil {
		return 0, fmt.Errorf("%q is not an age such as 30d", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q must be greater than zero", s)
	}
	return d, nil
}

// messageFilter builds the message selection from the flags that describe it.
func messageFilter(f syncFlags) (selection.Filter, error) {
	var sel selection.Filter
	var err error

	size := func(flag, value string) int64 {
		if err != nil || value == "" {
			return 0
		}
		var n int64
		if n, err = parseBytes(value); err != nil {
			err = fmt.Errorf("invalid --%s: %w", flag, err)
		}
		return n
	}
	age := func(flag, value string) time.Duration {
		if err != nil || value == "" {
			return 0
		}
		var d time.Duration
		if d, err = parseAge(value); err != nil {
			err = fmt.Errorf("invalid --%s: %w", flag, err)
		}
		return d
	}

	sel.MaxSize = size("max-size", f.maxSize)
	sel.MinSize = size("min-size", f.minSize)
	sel.MaxAge = age("max-age", f.maxAge)
	sel.MinAge = age("min-age", f.minAge)
	if err != nil {
		return selection.Filter{}, err
	}

	switch f.ageBasis {
	case "", "sent":
		sel.Basis = selection.BasisSent
	case "internal":
		sel.Basis = selection.BasisInternal
	default:
		return selection.Filter{}, fmt.Errorf("invalid --age-basis %q: want %q or %q", f.ageBasis, "sent", "internal")
	}

	return sel, sel.Validate()
}

// searchFlags parses both IMAP SEARCH options.
func searchFlags(f syncFlags) (source, dest searchkey.Key, err error) {
	if source, err = searchFlag("source-search", f.sourceSearch); err != nil {
		return source, dest, err
	}
	if dest, err = searchFlag("dest-search", f.destSearch); err != nil {
		return source, dest, err
	}
	if !dest.IsZero() && !f.delete2 {
		// Not an error, because --search sets both sides at once and its
		// source half is doing real work. Not silent either: unlike imapsync,
		// where --search2 narrows the whole destination view, here it narrows
		// nothing but the deletion candidates, so without --delete2 it has no
		// effect at all, and the user should hear that from us rather than
		// infer it from a result that looks the same either way.
		slog.Warn("--dest-search had no effect because this run does not delete anything; it only narrows what --delete2 would remove")
	}
	return source, dest, err
}

// searchFlag parses one of the IMAP SEARCH options, or returns the zero key
// when it was not given.
//
// Parsing here rather than at the first SELECT is the point of parsing at all:
// a search this tool cannot express is a mistake worth hearing about before
// any connection is opened, rather than once per folder in the middle of a
// run.
func searchFlag(flag, value string) (searchkey.Key, error) {
	if value == "" {
		return searchkey.Key{}, nil
	}
	key, err := searchkey.Parse(value)
	if err != nil {
		return searchkey.Key{}, fmt.Errorf("invalid --%s: %w", flag, err)
	}
	return key, nil
}

// derivePairID names this migration in the state database.
//
// The name has to depend on both endpoints. One database holding two migrations
// that share a name would treat one's progress as the other's and skip messages
// it never copied.
func derivePairID(source, dest config.Endpoint) (string, error) {
	src, err := source.Describe()
	if err != nil {
		return "", err
	}
	dst, err := dest.Describe()
	if err != nil {
		return "", err
	}
	return src + ">" + dst, nil
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

// writeConnectionNote reports the width each side settled on.
//
// The width a run settles on is worth more than the run: it is the only
// measurement of what a server will actually hold, and it is what the next
// run's connection flag should say. Putting it here means nobody has to infer
// it from a throughput number.
//
// Both outcomes are reported, not just the narrowing one. A run of 776,791
// messages was unable to answer whether the pool ever shrinks at a realistic
// width, because it did not shrink and therefore said nothing at all — leaving
// "held its width" and "said nothing" indistinguishable from the outside. A
// side that kept every connection it asked for is evidence that the server
// tolerated that many, which is precisely the question, and it is worth as much
// as the shrink.
func writeConnectionNote(p *printer, conns connections) {
	for _, c := range conns {
		switch {
		case c.got < c.asked:
			p.printf("\nThe %s server would not hold %d connections; the run ended on %d.\nThat is where the width finished rather than a fixed limit: it narrows when the\nserver refuses and climbs back while it does not. Pass --%s=%d next time to\nstart nearer and skip the opening refusals.\n",
				c.side, c.asked, c.got, c.flag, c.got)
		default:
			p.printf("\nThe %s server held all %d connections.\n", c.side, c.got)
		}
	}
}

// connection is what one side of a run asked for and what it ended up with.
type connection struct {
	side  string
	flag  string
	asked int
	got   int
}

type connections []connection

func writeSyncReport(out io.Writer, report syncer.Report, elapsed time.Duration, dryRun bool, conns connections) error {
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
	if filtered := report.Filtered(); filtered > 0 {
		goneWord += fmt.Sprintf("%d filtered out, ", filtered)
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

	writeUncopiedNotes(p, report)
	writeMissingNote(p, report, dryRun)
	writeDuplicatesNote(p, report)
	writeHeaderlessNote(p, report)
	writeConnectionNote(p, conns)

	// A refusal is the one thing here that asks the reader to do something, so
	// it says which folders and what to do about it rather than leaving a
	// number in a column to be noticed.
	if refused > 0 {
		p.printf("\nREFUSED to delete %d %s. That is a larger share of the destination folder\nthan --delete2-ceiling allows to go in one run, and the usual cause is a source\nthat answered a listing with less than the truth.\n",
			refused, plural(refused, "message"))
		for _, fr := range report.Folders {
			if fr.Refused > 0 {
				p.printf("  %s: %d of %d\n", fr.Dest, fr.Refused, fr.DestMessages)
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
//
// The label has to carry that qualification, because the number sits beside the
// adoption count and cannot be read without it. A run that settled 11,770
// messages in 37 seconds by adopting all but 19 of them reports 0.5, and "0.5
// messages/second" next to "11751 adopted" reads as a stall rather than as the
// fastest outcome a re-run has.
func rate(copied int, elapsed time.Duration, dryRun bool) string {
	if dryRun || copied == 0 || elapsed <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%.1f copied/second)", float64(copied)/elapsed.Seconds())
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
