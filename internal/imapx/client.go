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
	IMAP4rev2   bool `json:"imap4rev2"`
	CondStore   bool `json:"condstore"`
	QResync     bool `json:"qresync"`
	UIDPlus     bool `json:"uidplus"`
	MultiAppend bool `json:"multiappend"`
	SpecialUse  bool `json:"special_use"`
	Move        bool `json:"move"`
	ESearch     bool `json:"esearch"`
	Idle        bool `json:"idle"`
	Unselect    bool `json:"unselect"`
	Namespace   bool `json:"namespace"`
	ListStatus  bool `json:"list_status"`
	LiteralPlus bool `json:"literal_plus"`
	ObjectID    bool `json:"objectid"`

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

	// DebugWriter receives raw protocol traffic, including credentials. Nil
	// disables protocol tracing.
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
		DebugWriter: opts.DebugWriter,
	}

	netDialer := &net.Dialer{Timeout: timeout}
	addr := opts.Addr.HostPort()

	var (
		client *imapclient.Client
		err    error
	)
	switch opts.Addr.TLS {
	case config.TLSImplicit:
		var raw net.Conn
		raw, err = (&tls.Dialer{NetDialer: netDialer, Config: tlsConfig}).DialContext(dialCtx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dialling %s over TLS: %w", addr, err)
		}
		client = imapclient.New(raw, clientOpts)

	case config.TLSStartTLS:
		var raw net.Conn
		raw, err = netDialer.DialContext(dialCtx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dialling %s: %w", addr, err)
		}
		client, err = imapclient.NewStartTLS(raw, clientOpts)
		if err != nil {
			return nil, fmt.Errorf("negotiating STARTTLS with %s: %w", addr, err)
		}

	case config.TLSNone:
		var raw net.Conn
		raw, err = netDialer.DialContext(dialCtx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dialling %s: %w", addr, err)
		}
		client = imapclient.New(raw, clientOpts)

	default:
		return nil, fmt.Errorf("unknown TLS mode %q", opts.Addr.TLS)
	}

	if err := client.Login(opts.Addr.User, opts.Password).Wait(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("authenticating as %s: %w", opts.Addr.User, err)
	}

	return &conn{c: client}, nil
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
		ListStatus:     set.Has(imap.CapListStatus),
		LiteralPlus:    set.Has(imap.CapLiteralPlus),
		ObjectID:       set.Has(imap.CapObjectID),
		AuthMechanisms: set.AuthMechanisms(),
		Raw:            raw,
	}
	if limit, ok := set.AppendLimit(); ok {
		caps.AppendLimit = limit
	}

	// IMAP4rev2 subsumes several extensions without advertising them separately.
	if caps.IMAP4rev2 {
		caps.UIDPlus = true
		caps.Move = true
		caps.ESearch = true
		caps.Unselect = true
		caps.Namespace = true
		caps.ListStatus = true
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

	listOpts := &imap.ListOptions{ReturnSubscribed: true}
	if caps.SpecialUse {
		listOpts.ReturnSpecialUse = true
	}

	// LIST-STATUS folds STATUS into the LIST response. Without it we fall back
	// to a STATUS round trip per folder below.
	inlineStatus := opts.WithStatus && caps.ListStatus
	if inlineStatus {
		listOpts.ReturnStatus = statusOptions(caps)
	}

	entries, err := c.c.List("", "*", listOpts).Collect()
	if err != nil {
		return nil, fmt.Errorf("listing folders: %w", err)
	}

	folders := make([]Folder, 0, len(entries))
	for _, e := range entries {
		f := newFolder(e)
		if opts.WithStatus && !inlineStatus && f.Selectable {
			st, err := c.c.Status(e.Mailbox, statusOptions(caps)).Wait()
			if err != nil {
				return nil, fmt.Errorf("querying status of %q: %w", e.Mailbox, err)
			}
			applyStatus(&f, st)
		}
		folders = append(folders, f)
	}

	sort.Slice(folders, func(i, j int) bool { return folders[i].Name < folders[j].Name })
	return folders, nil
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
