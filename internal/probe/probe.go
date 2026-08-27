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

// SuggestedConcurrency proposes a connection count for this endpoint, staying
// one below the observed ceiling so a competing client does not push us over.
func (r *Report) SuggestedConcurrency() int {
	switch {
	case r.MaxConnections <= 0:
		return 0 // unknown: leave it to the adaptive governor
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
	password, err := opts.Endpoint.Password.Resolve()
	if err != nil {
		return nil, err
	}

	dialOpts := imapx.DialOptions{
		Addr:               addr,
		Password:           password,
		Timeout:            opts.DialTimeout,
		InsecureSkipVerify: opts.InsecureSkipVerify,
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
		report.MaxConnections, report.CeilingLimitedBy = measureCeiling(ctx, dialOpts, opts.MaxConnections)
	}

	report.Elapsed = time.Since(started)
	return report, nil
}

// measureCeiling opens connections one at a time, holding each open, until the
// server refuses or the cap is reached. It returns the highest count that
// succeeded, including the connection the caller already holds.
func measureCeiling(ctx context.Context, dialOpts imapx.DialOptions, max int) (int, string) {
	held := make([]imapx.Conn, 0, max)
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()

	// The caller's own connection counts toward the server's limit.
	const alreadyOpen = 1

	for len(held)+alreadyOpen < max {
		if err := ctx.Err(); err != nil {
			return len(held) + alreadyOpen, "probe cancelled"
		}
		c, err := imapx.Dial(ctx, dialOpts)
		if err != nil {
			return len(held) + alreadyOpen, refusalReason(err)
		}
		held = append(held, c)
	}
	return max, fmt.Sprintf("reached configured cap of %d, server may allow more", max)
}

func refusalReason(err error) string {
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "server stopped responding (timeout)"
	}
	return err.Error()
}
