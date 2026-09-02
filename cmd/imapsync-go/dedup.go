package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/state"
	"github.com/hilli/imapsync-go/internal/syncer"
)

func newDedupCmd() *cobra.Command {
	var f syncFlags

	cmd := &cobra.Command{
		Use:   "dedup",
		Short: "Remove messages a destination account holds more than once",
		Long: `Dedup examines the destination account on its own and removes messages a
folder holds more than one copy of. It copies nothing, and it never connects to
the source.

Candidates are grouped by Message-ID, Date, From, To, Cc and Subject together
with the size the server reports, and nothing is removed on that alone: both
messages are fetched and compared byte for byte first. Two automated
notifications sent in the same second can agree on every header and still say
different things.

Where the state database records that a copy answers for a source message, that
copy is the one kept and the records are re-pointed at it before anything is
deleted. A folder no sync has recorded is examined all the same, which is the
usual case for an account that arrived with duplicates already in it.

The source is still named, by --config or by --source-url, because the state
database is keyed by the pair and reading the wrong pair's records would mean
deleting a copy the tool had promised to keep. It is named and not contacted.

The same safety valve as --delete2 applies: a run that would remove more than
--delete2-ceiling of a folder refuses and says so, unless --force.`,
		Example: `  # Preview, changing nothing
  imapsync-go dedup --config imapsync.yaml --pair icloud-to-mox --dry-run

  # One folder
  imapsync-go dedup --config imapsync.yaml --pair icloud-to-mox --folder Archive

  # Everything below Lists/, and go ahead
  imapsync-go dedup --config imapsync.yaml --include '^Lists/'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f.dstConnsSet = cmd.Flags().Changed("dest-connections")
			return runDedup(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	cmd.Flags().StringVar(&f.configPath, "config", "", "path to a configuration file")
	cmd.Flags().StringVar(&f.pair, "pair", "", "pair name within the configuration file")
	cmd.Flags().StringVar(&f.statePath, "state", "", "path to the state database (default: per-user application state directory)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "report what would be removed without deleting anything")

	cmd.Flags().StringSliceVar(&f.only, "folder", nil, "examine only these destination folders, by exact name (repeatable)")
	cmd.Flags().StringSliceVar(&f.include, "include", nil, "examine only destination folders matching this regular expression (repeatable)")
	cmd.Flags().StringSliceVar(&f.exclude, "exclude", nil, "skip destination folders matching this regular expression (repeatable)")

	cmd.Flags().StringVar(&f.sourceURL, "source-url", "", "source endpoint URL; named so the right state records are read, never contacted")
	cmd.Flags().StringVar(&f.destURL, "dest-url", "", "destination endpoint URL, for example imaps://user@host:993")
	cmd.Flags().StringVar(&f.destPasswordEnv, "dest-password-env", "", "environment variable holding the destination password")
	cmd.Flags().StringVar(&f.destPasswordFile, "dest-password-file", "", "file holding the destination password")
	cmd.Flags().StringVar(&f.destPasswordKeychain, "dest-password-keychain", "", "macOS keychain service name holding the destination password")

	cmd.Flags().Float64Var(&f.deleteCeiling, "delete2-ceiling", 0.10, "refuse to remove more than this fraction of a folder's messages in one run")
	cmd.Flags().BoolVar(&f.force, "force", false, "carry out removals the ceiling would otherwise refuse")

	cmd.Flags().IntVar(&f.dstConns, "dest-connections", autoConnections, "connections to open to the destination; overrides the config's concurrency.dest")
	cmd.Flags().DurationVar(&f.dialTimeout, "dial-timeout", 30*time.Second, "connection establishment timeout")
	cmd.Flags().BoolVar(&f.insecureDst, "dest-insecure", false, "skip TLS certificate verification for the destination (test use only)")
	cmd.Flags().BoolVar(&f.trace, "trace", false, "print the raw IMAP conversation to stderr, with credentials redacted")

	cmd.MarkFlagsMutuallyExclusive("config", "dest-url")
	cmd.MarkFlagsMutuallyExclusive("dest-password-env", "dest-password-file", "dest-password-keychain")

	return cmd
}

func runDedup(ctx context.Context, out io.Writer, f syncFlags) error {
	pair, err := dedupPair(f)
	if err != nil {
		return err
	}

	sel, err := dedupSelection(f)
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

	dstConns := pickWidth(f.dstConnsSet, f.dstConns, pair.Concurrency.Dest)

	dstPool, release, err := destPool(ctx, pair, f, trace, dstConns)
	if err != nil {
		return err
	}
	defer release()

	pairName := pair.Name
	if pairName == "" {
		if pairName, err = derivePairID(pair.Source, pair.Dest); err != nil {
			return err
		}
	}

	// The nil source pool and nil memory budget are the claims this command
	// makes, put where they can be enforced rather than only documented: a run
	// that reaches for either fails loudly instead of quietly opening a
	// connection to an account nobody asked it to touch. Deduplication holds
	// one message at a time and reads no source, so neither has anything to do.
	started := time.Now()
	report, err := syncer.New(nil, dstPool, db, nil, syncer.Options{
		PairID:        pairName,
		DryRun:        f.dryRun,
		DeleteCeiling: f.deleteCeiling,
		Force:         f.force,
		Logger:        slog.Default(),
	}).Dedup(ctx, sel)

	writeErr := writeDedupReport(out, report, time.Since(started), f.dryRun)
	if err != nil {
		return err
	}
	for _, fr := range report.Folders {
		if fr.Err != nil {
			return fmt.Errorf("folder %q: %w", fr.Dest, fr.Err)
		}
	}
	if _, _, refused, _ := report.Totals(); refused > 0 {
		return fmt.Errorf("refused to remove %d %s: see above", refused, plural(refused, "message"))
	}
	return writeErr
}

// dedupPair resolves the destination endpoint, and the pair identity the state
// database is keyed by.
//
// The source is read for its name and not validated, which is the difference
// between this and syncPair. Validation is where a secret source is required,
// and demanding a source password for a run that will never open a source
// connection would be asking someone to put a live credential on a command line
// to have one mailbox tidied.
//
// It is still read. The records this run may re-point are filed under the pair,
// and reading the wrong pair's records means finding no claims at all -- so
// every copy looks unclaimed, the survivor is chosen by UID, and a later sync
// finds its recorded copy gone and makes the duplicate again. The two features
// would fight and the folder would grow every time it was cleaned.
func dedupPair(f syncFlags) (config.Pair, error) {
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
		merged.Dest.OAuth = oauthFrom(merged.Dest.OAuth, f.destOAuth())
		if err := merged.Dest.Validate(); err != nil {
			return config.Pair{}, fmt.Errorf("destination endpoint: %w", err)
		}
		return merged, nil
	}

	if f.sourceURL == "" || f.destURL == "" {
		return config.Pair{}, errors.New("provide either --config or both --source-url and --dest-url; the source is named so the right records are read, and is never contacted")
	}

	pair := config.Pair{
		Source: config.Endpoint{URL: f.sourceURL},
		Dest: config.Endpoint{
			URL:      f.destURL,
			Password: config.Secret{Env: f.destPasswordEnv, File: f.destPasswordFile, Keychain: f.destPasswordKeychain},
			OAuth:    f.destOAuth(),
		},
	}
	if err := pair.Dest.Validate(); err != nil {
		return config.Pair{}, fmt.Errorf("--dest-url: %w", err)
	}
	return pair, nil
}

// dedupSelection compiles the folder patterns against destination names.
func dedupSelection(f syncFlags) (syncer.DedupSelection, error) {
	sel := syncer.DedupSelection{Only: f.only}

	var err error
	if sel.Include, err = compilePatterns(f.include, "include"); err != nil {
		return sel, err
	}
	if sel.Exclude, err = compilePatterns(f.exclude, "exclude"); err != nil {
		return sel, err
	}
	return sel, nil
}

func writeDedupReport(out io.Writer, report syncer.DedupReport, elapsed time.Duration, dryRun bool) error {
	p := &printer{w: out}

	if dryRun {
		p.println("DRY RUN — nothing was removed")
		p.println()
	}

	population, removed, refused, unequal := report.Totals()

	// Only the folders with something to say are listed. An account has
	// hundreds of folders and duplicates in a handful, and a table of zeroes is
	// a table nobody reads to the end.
	t, flush := p.table()
	removedHeading := "REMOVED"
	if dryRun {
		removedHeading = "TO REMOVE"
	}
	t.printf("FOLDER\tMESSAGES\t%s\tREFUSED\tNOT IDENTICAL\n", removedHeading)
	listed := 0
	for _, fr := range report.Folders {
		if fr.Removed == 0 && fr.Refused == 0 && fr.Unequal == 0 && fr.Err == nil {
			continue
		}
		status := ""
		if fr.Err != nil {
			status = "  ← " + fr.Err.Error()
		}
		listed++
		t.printf("%s\t%d\t%d\t%d\t%d%s\n", fr.Dest, fr.Population, fr.Removed, fr.Refused, fr.Unequal, status)
	}
	if listed == 0 {
		t.printf("(none)\t\t\t\t\n")
	}
	flush()
	p.println()

	verb := "Removed"
	if dryRun {
		verb = "Would remove"
	}
	p.printf("%s %d %s from %d %s holding %d, in %s.\n",
		verb, removed, plural(removed, "message"),
		len(report.Folders), plural(len(report.Folders), "folder"), population, elapsed.Round(time.Millisecond))

	// Reported separately from the removals because the two say opposite things
	// about the grouping. A run that keeps finding candidates it cannot confirm
	// is a run whose key is too loose, and that is worth being able to see.
	if unequal > 0 {
		p.printf("%d %s matched on headers and size but differed, and %s left alone.\n",
			unequal, plural(unequal, "message"), was(unequal))
	}
	if refused > 0 {
		p.printf("Refused to remove %d %s: more than the ceiling allows. Use --force to override.\n",
			refused, plural(refused, "message"))
	}
	if len(report.Skips) > 0 {
		p.printf("Skipped %d %s.\n", len(report.Skips), plural(len(report.Skips), "folder"))
	}
	return p.err
}
