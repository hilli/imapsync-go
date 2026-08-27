// Package imapx is a narrow façade over the underlying IMAP client library.
//
// Everything the rest of imapsync-go needs from IMAP passes through this
// package. go-imap/v2 is still beta and is missing extensions we care about
// (QRESYNC, MULTIAPPEND, COMPRESS), so isolating it here keeps the cost of
// forking, extending or replacing it contained.
//
// A Conn is not safe for concurrent use. The connection pool owns exactly one
// goroutine per Conn, which makes the underlying library's single-command
// constraint structurally impossible to violate.
package imapx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/hilli/imapsync-go/internal/config"
)

// DefaultDialTimeout bounds connection establishment.
const DefaultDialTimeout = 30 * time.Second

// Caps is the subset of server capabilities that affects synchronisation
// strategy, plus the raw list for diagnostics.
type Caps struct {
	IMAP4rev2    bool `json:"imap4rev2"`
	CondStore    bool `json:"condstore"`
	QResync      bool `json:"qresync"`
	UIDPlus      bool `json:"uidplus"`
	MultiAppend  bool `json:"multiappend"`
	SpecialUse   bool `json:"special_use"`
	Move         bool `json:"move"`
	ESearch      bool `json:"esearch"`
	Idle         bool `json:"idle"`
	Unselect     bool `json:"unselect"`
	Namespace    bool `json:"namespace"`
	ListExtended bool `json:"list_extended"`
	ListStatus   bool `json:"list_status"`
	LiteralPlus  bool `json:"literal_plus"`
	ObjectID     bool `json:"objectid"`

	AuthMechanisms []string `json:"auth_mechanisms,omitempty"`
	AppendLimit    *uint32  `json:"append_limit,omitempty"`
	Raw            []string `json:"raw"`
}

// Folder is a mailbox as reported by LIST, optionally enriched by STATUS.
type Folder struct {
	Name       string   `json:"name"`
	Delim      string   `json:"delim"`
	Attrs      []string `json:"attrs,omitempty"`
	SpecialUse string   `json:"special_use,omitempty"`
	Selectable bool     `json:"selectable"`

	NumMessages   *uint32  `json:"num_messages,omitempty"`
	Size          *int64   `json:"size,omitempty"`
	UIDValidity   uint32   `json:"uidvalidity,omitempty"`
	UIDNext       imap.UID `json:"uidnext,omitempty"`
	HighestModSeq uint64   `json:"highest_modseq,omitempty"`
}

// Namespaces reports the personal namespace prefix and hierarchy delimiter.
type Namespaces struct {
	PersonalPrefix string `json:"personal_prefix"`
	Delim          string `json:"delim"`
	Supported      bool   `json:"supported"`
}

// ListOptions controls how much work ListFolders asks the server to do.
type ListOptions struct {
	// WithStatus requests message counts and UIDVALIDITY per folder. This is
	// cheap with LIST-STATUS and one round trip per folder without it.
	WithStatus bool
}

// Conn is a single authenticated IMAP connection.
type Conn interface {
	Caps() Caps
	Namespaces(ctx context.Context) (Namespaces, error)
	ListFolders(ctx context.Context, opts ListOptions) ([]Folder, error)
	Logout(ctx context.Context) error
	Close() error
}

// DialOptions describes how to establish and authenticate one connection.
type DialOptions struct {
	Addr     config.Address
	Password string

	// DebugWriter receives the raw protocol conversation for diagnosing server
	// quirks. Credentials are redacted before they reach it. Nil disables
	// tracing.
	DebugWriter io.Writer

	// Timeout bounds connection establishment. Zero uses DefaultDialTimeout.
	Timeout time.Duration

	// InsecureSkipVerify disables certificate verification. Test use only.
	InsecureSkipVerify bool
}

type conn struct {
	c *imapclient.Client
}

var _ Conn = (*conn)(nil)

// Dial establishes a connection, negotiates TLS according to the address, and
// authenticates.
func Dial(ctx context.Context, opts DialOptions) (Conn, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultDialTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tlsConfig := &tls.Config{
		ServerName:         opts.Addr.Host,
		InsecureSkipVerify: opts.InsecureSkipVerify, //nolint:gosec // opt-in, test use only
		MinVersion:         tls.VersionTLS12,
	}
	clientOpts := &imapclient.Options{
		TLSConfig:   tlsConfig,
		DebugWriter: newTraceWriter(opts.DebugWriter),
	}

	addr := opts.Addr.HostPort()
	raw, err := dialRaw(dialCtx, opts.Addr, tlsConfig, timeout)
	if err != nil {
		return nil, err
	}

	// Everything past the TCP connect is raced against the context rather than
	// bounded by a socket deadline: go-imap runs its own read loop and resets
	// any deadline we set. Without this, a server that accepts a connection and
	// then goes quiet, or acknowledges STARTTLS without negotiating TLS, stalls
	// the caller for as long as it likes. Racing the context also makes Dial
	// answer to cancellation, which Login().Wait() does not.
	type result struct {
		client *imapclient.Client
		err    error
	}
	done := make(chan result, 1)
	go func() {
		client, err := authenticate(raw, clientOpts, opts)
		done <- result{client, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			_ = raw.Close()
			return nil, r.err
		}
		return &conn{c: r.client}, nil

	case <-dialCtx.Done():
		// Closing the socket unblocks the goroutine; collect whatever it
		// produces so neither the client nor its read loop is left running.
		_ = raw.Close()
		go func() {
			if r := <-done; r.client != nil {
				_ = r.client.Close()
			}
		}()
		return nil, fmt.Errorf("establishing session with %s: %w", addr, dialCtx.Err())
	}
}

// dialRaw opens the transport, honouring the context for both the TCP connect
// and, for implicit TLS, the handshake.
func dialRaw(ctx context.Context, endpoint config.Address, tlsConfig *tls.Config, timeout time.Duration) (net.Conn, error) {
	netDialer := &net.Dialer{Timeout: timeout}
	addr := endpoint.HostPort()

	switch endpoint.TLS {
	case config.TLSImplicit:
		raw, err := (&tls.Dialer{NetDialer: netDialer, Config: tlsConfig}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dialling %s over TLS: %w", addr, err)
		}
		return raw, nil

	case config.TLSStartTLS, config.TLSNone:
		raw, err := netDialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dialling %s: %w", addr, err)
		}
		return raw, nil

	default:
		return nil, fmt.Errorf("unknown TLS mode %q", endpoint.TLS)
	}
}

// authenticate upgrades the transport if needed and logs in. It blocks until
// the server answers, so Dial runs it under a context race.
func authenticate(raw net.Conn, clientOpts *imapclient.Options, opts DialOptions) (*imapclient.Client, error) {
	addr := opts.Addr.HostPort()

	var client *imapclient.Client
	if opts.Addr.TLS == config.TLSStartTLS {
		var err error
		client, err = imapclient.NewStartTLS(raw, clientOpts)
		if err != nil {
			return nil, fmt.Errorf("negotiating STARTTLS with %s: %w", addr, err)
		}
	} else {
		client = imapclient.New(raw, clientOpts)
	}

	if err := client.Login(opts.Addr.User, opts.Password).Wait(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("authenticating as %s: %w", opts.Addr.User, err)
	}
	return client, nil
}

func (c *conn) Caps() Caps {
	set := c.c.Caps()

	raw := make([]string, 0, len(set))
	for cap := range set {
		raw = append(raw, string(cap))
	}
	sort.Strings(raw)

	caps := Caps{
		IMAP4rev2:      set.Has(imap.CapIMAP4rev2),
		CondStore:      set.Has(imap.CapCondStore),
		QResync:        set.Has(imap.CapQResync),
		UIDPlus:        set.Has(imap.CapUIDPlus),
		MultiAppend:    set.Has(imap.CapMultiAppend),
		SpecialUse:     set.Has(imap.CapSpecialUse),
		Move:           set.Has(imap.CapMove),
		ESearch:        set.Has(imap.CapESearch),
		Idle:           set.Has(imap.CapIdle),
		Unselect:       set.Has(imap.CapUnselect),
		Namespace:      set.Has(imap.CapNamespace),
		ListExtended:   set.Has(imap.CapListExtended),
		ListStatus:     set.Has(imap.CapListStatus),
		LiteralPlus:    set.Has(imap.CapLiteralPlus),
		ObjectID:       set.Has(imap.CapObjectID),
		AuthMechanisms: set.AuthMechanisms(),
		Raw:            raw,
	}
	if limit, ok := set.AppendLimit(); ok {
		caps.AppendLimit = limit
	}

	// go-imap's CapSet.Has already resolves the extensions IMAP4rev2 subsumes,
	// but SPECIAL-USE is missing from its table even though RFC 9051 folds the
	// attributes into the base LIST response.
	if caps.IMAP4rev2 {
		caps.SpecialUse = true
	}

	return caps
}

func (c *conn) Namespaces(ctx context.Context) (Namespaces, error) {
	if !c.Caps().Namespace {
		return Namespaces{Supported: false}, nil
	}

	data, err := c.c.Namespace().Wait()
	if err != nil {
		return Namespaces{}, fmt.Errorf("querying namespaces: %w", err)
	}
	if data == nil || len(data.Personal) == 0 {
		return Namespaces{Supported: true}, nil
	}

	personal := data.Personal[0]
	ns := Namespaces{PersonalPrefix: personal.Prefix, Supported: true}
	if personal.Delim != 0 {
		ns.Delim = string(personal.Delim)
	}
	return ns, nil
}

func (c *conn) ListFolders(ctx context.Context, opts ListOptions) ([]Folder, error) {
	caps := c.Caps()

	listOpts := listOptions(caps, opts)
	entries, err := c.c.List("", "*", listOpts).Collect()
	if err != nil && listOpts != nil && isProtocolRejection(err) {
		// The server advertised LIST-EXTENDED but rejected the return options
		// anyway. Retry with the plain RFC 3501 form rather than failing: the
		// data is still reachable, just at the cost of a STATUS round trip per
		// folder.
		slog.Warn("server rejected extended LIST, retrying without return options",
			"error", err, "server_advertised_list_extended", caps.ListExtended)
		listOpts = nil
		entries, err = c.c.List("", "*", nil).Collect()
	}
	if err != nil {
		return nil, fmt.Errorf("listing folders: %w", err)
	}

	// LIST-STATUS folds STATUS into the LIST response. Without it we fall back
	// to a STATUS round trip per folder below.
	inlineStatus := listOpts != nil && listOpts.ReturnStatus != nil

	folders := make([]Folder, 0, len(entries))
	for _, e := range entries {
		f := newFolder(e)
		if opts.WithStatus && !inlineStatus && f.Selectable {
			st, err := c.c.Status(e.Mailbox, statusOptions(caps)).Wait()
			if err != nil {
				return nil, fmt.Errorf("querying status of %q: %w", e.Mailbox, err)
			}
			if st == nil {
				// The server completed the command without sending the untagged
				// STATUS it promised. Reporting a folder with no UIDVALIDITY as
				// though it were fine is how a sync silently loses track of it.
				return nil, fmt.Errorf("querying status of %q: server returned no STATUS data", e.Mailbox)
			}
			applyStatus(&f, st)
		}
		folders = append(folders, f)
	}

	sort.Slice(folders, func(i, j int) bool { return folders[i].Name < folders[j].Name })
	return folders, nil
}

// listOptions decides what may be appended to a LIST command. Return options
// are LIST-EXTENDED syntax (RFC 5258); a server without that extension answers
// BAD to the whole command, so a nil result meaning "plain RFC 3501 LIST" is
// the only safe request to make of it.
func listOptions(caps Caps, opts ListOptions) *imap.ListOptions {
	if !caps.ListExtended {
		return nil
	}
	listOpts := &imap.ListOptions{
		ReturnSubscribed: true,
		ReturnSpecialUse: caps.SpecialUse,
	}
	if opts.WithStatus && caps.ListStatus {
		listOpts.ReturnStatus = statusOptions(caps)
	}
	return listOpts
}

// isProtocolRejection reports whether the server refused a command outright, as
// opposed to the connection failing. Only a refusal is worth retrying
// differently.
func isProtocolRejection(err error) bool {
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		return false
	}
	return imapErr.Type == imap.StatusResponseTypeBad || imapErr.Type == imap.StatusResponseTypeNo
}

func statusOptions(caps Caps) *imap.StatusOptions {
	opts := &imap.StatusOptions{
		NumMessages: true,
		UIDNext:     true,
		UIDValidity: true,
	}
	if caps.CondStore {
		opts.HighestModSeq = true
	}
	if caps.IMAP4rev2 {
		opts.Size = true
	}
	return opts
}

func newFolder(e *imap.ListData) Folder {
	f := Folder{
		Name:       e.Mailbox,
		Selectable: true,
	}
	if e.Delim != 0 {
		f.Delim = string(e.Delim)
	}
	for _, attr := range e.Attrs {
		f.Attrs = append(f.Attrs, string(attr))
		switch attr {
		case imap.MailboxAttrNoSelect, imap.MailboxAttrNonExistent:
			f.Selectable = false
		case imap.MailboxAttrAll, imap.MailboxAttrArchive, imap.MailboxAttrDrafts,
			imap.MailboxAttrFlagged, imap.MailboxAttrJunk, imap.MailboxAttrSent,
			imap.MailboxAttrTrash, imap.MailboxAttrImportant:
			f.SpecialUse = string(attr)
		}
	}
	applyStatus(&f, e.Status)
	return f
}

func applyStatus(f *Folder, st *imap.StatusData) {
	if st == nil {
		return
	}
	f.NumMessages = st.NumMessages
	f.Size = st.Size
	f.UIDValidity = st.UIDValidity
	f.UIDNext = st.UIDNext
	f.HighestModSeq = st.HighestModSeq
}

func (c *conn) Logout(ctx context.Context) error {
	if err := c.c.Logout().Wait(); err != nil {
		return fmt.Errorf("logging out: %w", err)
	}
	return nil
}

func (c *conn) Close() error {
	err := c.c.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("closing connection: %w", err)
	}
	return nil
}
