package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/probe"
)

type probeFlags struct {
	configPath string
	pair       string
	side       string

	url              string
	passwordEnv      string
	passwordFile     string
	passwordKeychain string

	maxConnections int
	withStatus     bool
	dialTimeout    time.Duration
	insecure       bool
	asJSON         bool
	trace          bool
}

func newProbeCmd() *cobra.Command {
	var f probeFlags

	cmd := &cobra.Command{
		Use:   "probe",
		Short: "Inspect an IMAP server's capabilities, folders and connection ceiling",
		Long: `Probe connects to an IMAP endpoint and reports what it can support:
negotiated capabilities, namespace and hierarchy delimiter, the folder list with
SPECIAL-USE attributes, and optionally the practical simultaneous connection
limit.

The connection ceiling is measured by opening connections until the server
refuses. It is off by default because it is intrusive; enable it with
--max-connections.`,
		Example: `  # Straight from flags, no config file needed
  imapsync-go probe --url imaps://you@imap.mail.me.com --password-env ICLOUD_APP_PW

  # Find the practical connection limit and per-folder counts
  imapsync-go probe --url imaps://you@mox.example.net --password-keychain mox-imap \
      --max-connections 16 --status

  # From a config file
  imapsync-go probe --config imapsync.yaml --pair icloud-to-mox --side both`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProbe(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	cmd.Flags().StringVar(&f.configPath, "config", "", "path to a configuration file")
	cmd.Flags().StringVar(&f.pair, "pair", "", "pair name within the configuration file")
	cmd.Flags().StringVar(&f.side, "side", "both", "which endpoint of the pair to probe: source, dest or both")

	cmd.Flags().StringVar(&f.url, "url", "", "endpoint URL, for example imaps://user@host:993")
	cmd.Flags().StringVar(&f.passwordEnv, "password-env", "", "environment variable holding the password")
	cmd.Flags().StringVar(&f.passwordFile, "password-file", "", "file holding the password")
	cmd.Flags().StringVar(&f.passwordKeychain, "password-keychain", "", "macOS keychain service name holding the password")

	cmd.Flags().IntVar(&f.maxConnections, "max-connections", 0, "measure the connection ceiling, opening at most this many connections (0 disables)")
	cmd.Flags().BoolVar(&f.withStatus, "status", false, "include per-folder message counts and UIDVALIDITY")
	cmd.Flags().DurationVar(&f.dialTimeout, "dial-timeout", 30*time.Second, "connection establishment timeout")
	cmd.Flags().BoolVar(&f.insecure, "insecure", false, "skip TLS certificate verification (test use only)")
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "emit the report as JSON")
	cmd.Flags().BoolVar(&f.trace, "trace", false, "print the raw IMAP conversation to stderr, with credentials redacted")

	cmd.MarkFlagsMutuallyExclusive("config", "url")
	cmd.MarkFlagsMutuallyExclusive("password-env", "password-file", "password-keychain")

	return cmd
}

type probeTarget struct {
	label    string
	endpoint config.Endpoint
}

func runProbe(ctx context.Context, out io.Writer, f probeFlags) error {
	targets, err := probeTargets(f)
	if err != nil {
		return err
	}

	reports := make(map[string]*probe.Report, len(targets))
	order := make([]string, 0, len(targets))

	var trace io.Writer
	if f.trace {
		trace = os.Stderr
	}

	for _, t := range targets {
		report, err := probe.Run(ctx, probe.Options{
			Endpoint:           t.endpoint,
			MaxConnections:     f.maxConnections,
			WithStatus:         f.withStatus,
			DialTimeout:        f.dialTimeout,
			InsecureSkipVerify: f.insecure,
			Trace:              trace,
		})
		if err != nil {
			return fmt.Errorf("probing %s: %w", t.label, err)
		}
		reports[t.label] = report
		order = append(order, t.label)
	}

	if f.asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(reports)
	}

	p := printer{w: out}
	for i, label := range order {
		if i > 0 {
			p.println()
		}
		writeReport(&p, label, reports[label])
	}
	return p.err
}

func probeTargets(f probeFlags) ([]probeTarget, error) {
	if f.configPath != "" {
		cfg, err := config.Load(f.configPath)
		if err != nil {
			return nil, err
		}

		pair := &cfg.Pairs[0]
		if f.pair != "" {
			if pair, err = cfg.Pair(f.pair); err != nil {
				return nil, err
			}
		} else if len(cfg.Pairs) > 1 {
			return nil, fmt.Errorf("config defines %d pairs, select one with --pair", len(cfg.Pairs))
		}

		switch strings.ToLower(f.side) {
		case "source":
			return []probeTarget{{"source", pair.Source}}, nil
		case "dest":
			return []probeTarget{{"dest", pair.Dest}}, nil
		case "both":
			return []probeTarget{{"source", pair.Source}, {"dest", pair.Dest}}, nil
		default:
			return nil, fmt.Errorf("invalid --side %q, want source, dest or both", f.side)
		}
	}

	if f.url == "" {
		return nil, errors.New("provide either --config or --url")
	}
	ep := config.Endpoint{
		URL: f.url,
		Password: config.Secret{
			Env:      f.passwordEnv,
			File:     f.passwordFile,
			Keychain: f.passwordKeychain,
		},
	}
	if err := ep.Password.Validate(); err != nil {
		return nil, fmt.Errorf("password: %w", err)
	}
	if _, err := ep.Address(); err != nil {
		return nil, fmt.Errorf("--url: %w", err)
	}
	return []probeTarget{{"endpoint", ep}}, nil
}

// printer writes formatted output and remembers the first write error, so the
// report body stays readable instead of being buried in error checks.
type printer struct {
	w   io.Writer
	err error
}

func tlsLabel(mode string) string {
	switch config.TLSMode(mode) {
	case config.TLSImplicit:
		return "implicit TLS"
	case config.TLSStartTLS:
		return "STARTTLS"
	case config.TLSNone:
		return "plaintext, no TLS"
	default:
		return mode
	}
}

func (p *printer) printf(format string, a ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, a...)
}

func (p *printer) println(a ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintln(p.w, a...)
}

// table returns a printer writing through a tabwriter, plus a flush function.
// Errors are funnelled back into the parent printer.
func (p *printer) table() (*printer, func()) {
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	child := &printer{w: tw}
	return child, func() {
		if err := tw.Flush(); err != nil && child.err == nil {
			child.err = err
		}
		if p.err == nil {
			p.err = child.err
		}
	}
}

func writeReport(p *printer, label string, r *probe.Report) {
	p.printf("%s  %s at %s  (%s, probed in %s)\n", strings.ToUpper(label), r.Account, r.Server, tlsLabel(r.TLS), r.Elapsed.Round(time.Millisecond))

	p.println("\nCapabilities that matter:")
	caps, flush := p.table()

	listStatusNote := "folder counts without a round trip each"
	if r.Caps.ListStatus && !r.Caps.ListExtended {
		// LIST-STATUS is defined as a LIST return option, so it is unreachable
		// without the extension that permits return options. iCloud advertises
		// exactly this combination; reporting it as available would suggest a
		// saving that cannot be had.
		listStatusNote = "advertised but unusable without LIST-EXTENDED"
	}

	for _, row := range []struct {
		name    string
		present bool
		note    string
	}{
		{"IMAP4rev2", r.Caps.IMAP4rev2, "subsumes UIDPLUS, MOVE, ESEARCH, SPECIAL-USE"},
		{"UIDPLUS", r.Caps.UIDPlus, "APPENDUID gives exact destination UIDs"},
		{"CONDSTORE", r.Caps.CondStore, "incremental flag sync, skip unchanged folders"},
		{"QRESYNC", r.Caps.QResync, "not yet used: unimplemented in go-imap/v2"},
		{"SPECIAL-USE", r.Caps.SpecialUse, "map folders by attribute, not by name"},
		{"LIST-EXTENDED", r.Caps.ListExtended, "LIST return options; without it, plain RFC 3501 LIST"},
		{"MOVE", r.Caps.Move, "cheap moves"},
		{"MULTIAPPEND", r.Caps.MultiAppend, "not yet used: unimplemented in go-imap/v2"},
		{"LIST-STATUS", r.Caps.ListStatus, listStatusNote},
		{"LITERAL+", r.Caps.LiteralPlus, "saves a round trip per append"},
	} {
		mark := "no"
		if row.present {
			mark = "yes"
		}
		caps.printf("  %s\t%s\t%s\n", row.name, mark, row.note)
	}
	flush()

	if r.Caps.AppendLimit != nil {
		p.printf("  APPENDLIMIT: %d bytes\n", *r.Caps.AppendLimit)
	}
	if len(r.Caps.AuthMechanisms) > 0 {
		p.printf("\nAuth mechanisms: %s\n", strings.Join(r.Caps.AuthMechanisms, ", "))
	}

	delim := r.Namespaces.Delim
	if delim == "" {
		delim = "unknown"
	}
	prefix := r.Namespaces.PersonalPrefix
	if prefix == "" {
		prefix = "(none)"
	}
	p.printf("\nNamespace: personal prefix %s, hierarchy delimiter %q\n", prefix, delim)

	if len(r.SpecialUse) > 0 {
		attrs := make([]string, 0, len(r.SpecialUse))
		for a := range r.SpecialUse {
			attrs = append(attrs, a)
		}
		sort.Strings(attrs)

		p.println("\nSpecial-use folders:")
		su, flushSU := p.table()
		for _, a := range attrs {
			su.printf("  %s\t%s\n", a, r.SpecialUse[a])
		}
		flushSU()

		if !r.Caps.SpecialUse {
			// iCloud tags \Sent and \Trash but not Drafts, Junk or Archive, and
			// never advertises the extension. Attributes seen this way are a
			// bonus, not a complete mapping, so folder matching cannot rely on
			// them alone.
			p.println("  (server does not advertise SPECIAL-USE: this mapping may be incomplete)")
		}
	}

	p.printf("\nFolders: %d", len(r.Folders))
	if total := r.TotalMessages(); total > 0 {
		p.printf(", %d messages", total)
	}
	p.println()

	list, flushList := p.table()
	for _, f := range r.Folders {
		count := "-"
		if f.NumMessages != nil {
			count = strconv.FormatUint(uint64(*f.NumMessages), 10)
		}
		notes := f.SpecialUse
		if !f.Selectable {
			notes = strings.TrimSpace(notes + " (not selectable)")
		}
		list.printf("  %s\t%s\t%s\n", f.Name, count, notes)
	}
	flushList()

	if r.MaxConnections > 0 {
		p.printf("\nConnection ceiling: %d simultaneous (%s)\n", r.MaxConnections, r.CeilingLimitedBy)
		if s := r.SuggestedConcurrency(); s > 0 {
			p.printf("Suggested concurrency: %d (one below the ceiling, leaving headroom for other clients)\n", s)
		}
	} else {
		p.println("\nConnection ceiling: not measured (pass --max-connections to probe it)")
	}
}
