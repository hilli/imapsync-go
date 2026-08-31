// Package probe inspects an IMAP endpoint and reports what synchronisation
// strategy it can support.
//
// Probing exists because server behaviour, especially connection limits, is not
// discoverable from capabilities alone. Running probe against a real account
// produces the numbers the adaptive governor would otherwise have to learn the
// hard way, mid-migration.
package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/imapx"
)

// Options controls a probe run.
type Options struct {
	Endpoint config.Endpoint

	// MaxConnections caps the connection ceiling search. Zero skips the search
	// entirely, which is the polite choice against a production account.
	MaxConnections int

	// WithStatus requests per-folder message counts, which costs a round trip
	// per folder on servers lacking LIST-STATUS.
	WithStatus bool

	DialTimeout        time.Duration
	InsecureSkipVerify bool

	// Trace receives the raw protocol conversation, with credentials redacted.
	// Nil disables tracing.
	Trace io.Writer
}

// Report is the result of probing one endpoint.
type Report struct {
	Account string `json:"account"`
	Server  string `json:"server"`
	TLS     string `json:"tls"`

	Caps       imapx.Caps       `json:"capabilities"`
	Namespaces imapx.Namespaces `json:"namespaces"`
	Folders    []imapx.Folder   `json:"folders"`

	// SpecialUse maps a SPECIAL-USE attribute such as "\\Sent" to the folder
	// carrying it.
	SpecialUse map[string]string `json:"special_use"`

	// MaxConnections is the highest number of simultaneous authenticated
	// connections observed to succeed. Zero means the search was not run.
	MaxConnections int `json:"max_connections"`

	// CeilingLimitedBy records why the search stopped, either the configured
	// cap or the server error that ended it.
	CeilingLimitedBy string `json:"ceiling_limited_by,omitempty"`

	// Refused says the search ended because the server declined a connection,
	// which is the only case in which MaxConnections is a real ceiling rather
	// than a number we chose. Without it a probe that stopped at its own cap
	// reads exactly like one that found a wall, and the advice that follows is
	// the opposite in each case.
	Refused bool `json:"refused"`

	Elapsed time.Duration `json:"elapsed"`
}

// TotalMessages sums known per-folder message counts.
func (r *Report) TotalMessages() uint32 {
	var total uint32
	for _, f := range r.Folders {
		if f.NumMessages != nil {
			total += *f.NumMessages
		}
	}
	return total
}

// SuggestedConcurrency proposes a connection count for this endpoint.
//
// Where the server refused, the suggestion stays one below the wall so a
// competing client does not push the sync over it. Where the search only ran
// out of its own budget there is no wall to stay clear of, and subtracting one
// would be advising against a limit nobody has observed.
func (r *Report) SuggestedConcurrency() int {
	switch {
	case r.MaxConnections <= 0:
		return 0 // unknown: leave it to the pool to find out
	case !r.Refused:
		return r.MaxConnections
	case r.MaxConnections <= 2:
		return 1
	default:
		return r.MaxConnections - 1
	}
}

// Run probes the endpoint.
func Run(ctx context.Context, opts Options) (*Report, error) {
	started := time.Now()

	addr, err := opts.Endpoint.Address()
	if err != nil {
		return nil, err
	}
	password, err := opts.Endpoint.Password.Resolve(ctx)
	if err != nil {
		return nil, err
	}

	dialOpts := imapx.DialOptions{
		Addr:               addr,
		Credential:         imapx.StaticPassword(password),
		Timeout:            opts.DialTimeout,
		InsecureSkipVerify: opts.InsecureSkipVerify,
		DebugWriter:        opts.Trace,
	}

	conn, err := imapx.Dial(ctx, dialOpts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	report := &Report{
		Account:    addr.User,
		Server:     addr.HostPort(),
		TLS:        string(addr.TLS),
		Caps:       conn.Caps(),
		SpecialUse: map[string]string{},
	}

	if report.Namespaces, err = conn.Namespaces(ctx); err != nil {
		return nil, err
	}

	folders, err := conn.ListFolders(ctx, imapx.ListOptions{WithStatus: opts.WithStatus})
	if err != nil {
		return nil, err
	}
	report.Folders = folders
	for _, f := range folders {
		if f.SpecialUse != "" {
			report.SpecialUse[f.SpecialUse] = f.Name
		}
	}

	if opts.MaxConnections > 0 {
		report.MaxConnections, report.CeilingLimitedBy, report.Refused = measureCeiling(ctx, dialOpts, opts.MaxConnections)
	}

	report.Elapsed = time.Since(started)
	return report, nil
}

// measureCeiling opens connections one at a time, holding each open, until the
// server refuses or the cap is reached. It returns the highest count that
// succeeded, including the connection the caller already holds.
func measureCeiling(ctx context.Context, dialOpts imapx.DialOptions, limit int) (held int, why string, refused bool) {
	// The ceiling search is the same login repeated; tracing it would bury the
	// interesting conversation in noise.
	dialOpts.DebugWriter = nil

	open := make([]imapx.Conn, 0, limit)
	defer func() {
		for _, c := range open {
			_ = c.Close()
		}
	}()

	// The caller's own connection counts toward the server's limit.
	const alreadyOpen = 1

	for len(open)+alreadyOpen < limit {
		if err := ctx.Err(); err != nil {
			// A cancelled probe found nothing: it stopped because we stopped it.
			return len(open) + alreadyOpen, "probe cancelled", false
		}
		c, err := imapx.Dial(ctx, dialOpts)
		if err != nil {
			return len(open) + alreadyOpen, refusalReason(err), true
		}
		open = append(open, c)
	}
	return limit, fmt.Sprintf("reached configured cap of %d, server may allow more", limit), false
}

func refusalReason(err error) string {
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "server stopped responding (timeout)"
	}
	return err.Error()
}
