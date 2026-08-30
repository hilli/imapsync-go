package localstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/searchkey"
)

// Conn is one handle on a local store, presented as an IMAP connection.
//
// It satisfies imapx.Conn, which is the whole architecture: the pools, the
// connection governor, the state database, the identity and adoption logic,
// the filters and the run report are all defined against that interface and
// work here unchanged.
type Conn struct {
	store    *Store
	sel      *folder
	readOnly bool
}

var _ imapx.Conn = (*Conn)(nil)

// ErrNoMailbox is returned by an operation issued before a mailbox is
// selected.
var ErrNoMailbox = errors.New("no mailbox selected")

// Caps reports what the store can do.
//
// UIDPLUS is honest rather than optimistic: an append here knows exactly where
// the message landed, which is what the extension promises. The absences
// matter more — no CONDSTORE and no QRESYNC means nothing ever asks for a
// modification sequence the store did not hand out.
func (c *Conn) Caps() imapx.Caps {
	return imapx.Caps{
		UIDPlus:   true,
		Namespace: true,
		Raw:       []string{"IMAP4rev1", "UIDPLUS", "NAMESPACE"},
	}
}

// Namespaces reports the store's hierarchy separator. The store keeps folder
// structure as directories rather than as a character inside a name, so what
// it reports here is its own convention and not an inherited one.
func (c *Conn) Namespaces(context.Context) (imapx.Namespaces, error) {
	return imapx.Namespaces{PersonalPrefix: "", Delim: Delim, Supported: true}, nil
}

// ListFolders reports every folder in the store.
func (c *Conn) ListFolders(ctx context.Context, _ imapx.ListOptions) ([]imapx.Folder, error) {
	return c.store.list(ctx)
}

// Select opens a folder, reconciling it against its directory on the way in.
func (c *Conn) Select(ctx context.Context, mailbox string, opts imapx.SelectOptions) (imapx.Mailbox, error) {
	f, err := c.store.open(ctx, mailbox)
	if err != nil {
		return imapx.Mailbox{}, err
	}
	c.sel, c.readOnly = f, opts.ReadOnly

	uids, err := f.uids()
	if err != nil {
		return imapx.Mailbox{}, err
	}
	f.mu.Lock()
	next, validity := f.uidNext, f.uidValidity
	f.mu.Unlock()

	// A folder cannot hold more messages than there are UIDs to number them
	// with, and a UID is a uint32.
	count := uint32(len(uids)) //nolint:gosec // bounded by the UID space itself

	return imapx.Mailbox{
		Name:        f.name,
		NumMessages: count,
		UIDNext:     next,
		UIDValidity: validity,
		ReadOnly:    opts.ReadOnly,
	}, nil
}

// CreateFolder makes a folder if it is not already there.
func (c *Conn) CreateFolder(ctx context.Context, name string) error {
	return c.store.create(ctx, name)
}

// SubscribeFolder records a folder as subscribed, so a restore can put the
// subscription back.
func (c *Conn) SubscribeFolder(ctx context.Context, name string) error {
	f, err := c.store.open(ctx, name)
	if err != nil {
		return err
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE folder SET subscribed = 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("subscribing %q: %w", name, err)
	}
	f.mu.Lock()
	f.subscribed = true
	f.mu.Unlock()
	return nil
}

// AllUIDs lists the messages in the selected folder, by reading the directory.
func (c *Conn) AllUIDs(context.Context) ([]uint32, error) {
	if c.sel == nil {
		return nil, ErrNoMailbox
	}
	return c.sel.uids()
}

// FetchMeta returns everything about the named messages except their bodies.
func (c *Conn) FetchMeta(ctx context.Context, uids []uint32, fields []string) ([]imapx.MessageMeta, error) {
	if c.sel == nil {
		return nil, ErrNoMailbox
	}
	if len(uids) == 0 {
		return nil, nil
	}
	records, err := c.sel.records(ctx, uids)
	if err != nil {
		return nil, err
	}

	out := make([]imapx.MessageMeta, 0, len(records))
	for _, r := range records {
		m := imapx.MessageMeta{
			UID:          r.uid,
			Flags:        r.flags,
			InternalDate: r.date,
			Size:         r.size,
		}
		if len(fields) > 0 {
			header, err := readHeader(c.sel.path(r.uid))
			if err != nil {
				return nil, fmt.Errorf("reading message %d: %w", r.uid, err)
			}
			m.Header = selectFields(header, fields)
		}
		out = append(out, m)
	}
	return out, nil
}

// FetchBody copies a message out and reports its size.
func (c *Conn) FetchBody(_ context.Context, uid uint32, w io.Writer) (int64, error) {
	if c.sel == nil {
		return 0, ErrNoMailbox
	}
	f, err := os.Open(c.sel.path(uid))
	if err != nil {
		return 0, fmt.Errorf("reading message %d: %w", uid, err)
	}
	defer func() { _ = f.Close() }()

	n, err := io.Copy(w, f)
	if err != nil {
		return n, fmt.Errorf("reading message %d: %w", uid, err)
	}
	return n, nil
}

// Append writes a message into a folder.
//
// The message goes to a staging directory on the same filesystem, is flushed,
// given its timestamps, and only then renamed into place. POSIX rename is
// atomic, so a half-written message can never be observed — which makes a
// local append safer than the IMAP one it stands in for. A crash between the
// rename and the database row leaves a message that reconciliation adopts,
// with the right date, on the next run.
func (c *Conn) Append(ctx context.Context, mailbox string, msg imapx.AppendMessage) (imapx.AppendResult, error) {
	f, err := c.store.open(ctx, mailbox)
	if err != nil {
		return imapx.AppendResult{}, err
	}
	if c.readOnly && c.sel == f {
		return imapx.AppendResult{}, fmt.Errorf("appending to %q: mailbox is open read-only", mailbox)
	}

	tmp, err := os.CreateTemp(filepath.Join(f.dir, tmpName), "append-*")
	if err != nil {
		return imapx.AppendResult{}, fmt.Errorf("appending to %q: %w", mailbox, err)
	}
	staged := tmp.Name()
	defer func() { _ = os.Remove(staged) }()

	size, err := io.Copy(tmp, msg.Body)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return imapx.AppendResult{}, fmt.Errorf("appending to %q: %w", mailbox, err)
	}

	date := msg.InternalDate
	if date.IsZero() {
		date = time.Now()
	}
	if err := setTimes(staged, date); err != nil {
		return imapx.AppendResult{}, fmt.Errorf("appending to %q: %w", mailbox, err)
	}

	uid, err := f.claim(ctx, 0)
	if err != nil {
		return imapx.AppendResult{}, fmt.Errorf("appending to %q: %w", mailbox, err)
	}
	dst := f.path(uid)
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return imapx.AppendResult{}, fmt.Errorf("appending to %q: %w", mailbox, err)
	}
	if err := os.Rename(staged, dst); err != nil {
		return imapx.AppendResult{}, fmt.Errorf("appending to %q: %w", mailbox, err)
	}

	if _, err := f.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO messages (uid, flags, internaldate, size) VALUES (?, ?, ?, ?)`,
		uid, joinFlags(msg.Flags), date.Unix(), size); err != nil {
		return imapx.AppendResult{}, fmt.Errorf("appending to %q: %w", mailbox, err)
	}

	f.mu.Lock()
	validity := f.uidValidity
	f.mu.Unlock()
	return imapx.AppendResult{UID: uid, UIDValidity: validity}, nil
}

// SearchHeader finds messages whose header field contains a value.
//
// This is a linear scan, and it is nearly never reached: the store reports
// UIDPLUS, so an append already knows where its message landed and adoption
// has nothing left to look for.
func (c *Conn) SearchHeader(ctx context.Context, field, value string) ([]uint32, error) {
	if c.sel == nil {
		return nil, ErrNoMailbox
	}
	uids, err := c.sel.uids()
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, uid := range uids {
		header, err := readHeader(c.sel.path(uid))
		if err != nil {
			continue // deleted from under us
		}
		if headerContains(header, field, value) {
			out = append(out, uid)
		}
	}
	_ = ctx
	return out, nil
}

// Search evaluates a search key against the folder.
func (c *Conn) Search(ctx context.Context, key searchkey.Key) ([]uint32, error) {
	if c.sel == nil {
		return nil, ErrNoMailbox
	}
	if key.IsZero() {
		return c.sel.uids()
	}
	records, err := c.sel.records(ctx, nil)
	if err != nil {
		return nil, err
	}
	return evaluate(key.Criteria(), records, c.sel.path)
}

// FetchFlags returns the flags of every message in the selected folder.
//
// changedSince is ignored, which is correct rather than lazy: the store
// reports no CONDSTORE, so it is never asked for a modification sequence it
// did not give out.
func (c *Conn) FetchFlags(ctx context.Context, _ uint64) ([]imapx.FlagSet, error) {
	if c.sel == nil {
		return nil, ErrNoMailbox
	}
	records, err := c.sel.records(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]imapx.FlagSet, 0, len(records))
	for _, r := range records {
		out = append(out, imapx.FlagSet{UID: r.uid, Flags: r.flags})
	}
	return out, nil
}

// StoreFlags replaces a message's flags. The message file is not touched:
// written once and never again is what keeps an incremental backup of the
// store proportional to the mail that actually arrived.
func (c *Conn) StoreFlags(ctx context.Context, uid uint32, flags []string) error {
	if c.sel == nil {
		return ErrNoMailbox
	}
	if c.readOnly {
		return fmt.Errorf("storing flags on %d: mailbox is open read-only", uid)
	}
	_, err := c.sel.db.ExecContext(ctx,
		`UPDATE messages SET flags = ? WHERE uid = ?`, joinFlags(flags), uid)
	if err != nil {
		return fmt.Errorf("storing flags on %d: %w", uid, err)
	}
	return nil
}

// DeleteMessages removes messages from the store.
func (c *Conn) DeleteMessages(ctx context.Context, uids []uint32) error {
	if c.sel == nil {
		return ErrNoMailbox
	}
	if c.readOnly {
		return errors.New("deleting messages: mailbox is open read-only")
	}
	for _, uid := range uids {
		if err := os.Remove(c.sel.path(uid)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("deleting message %d: %w", uid, err)
		}
		if _, err := c.sel.db.ExecContext(ctx, `DELETE FROM messages WHERE uid = ?`, uid); err != nil {
			return fmt.Errorf("deleting message %d: %w", uid, err)
		}
	}
	return nil
}

// Logout is a no-op: there is no session to end.
func (c *Conn) Logout(context.Context) error { return nil }

// Close releases the handle. The folder databases belong to the store and stay
// open for other connections; Store.Close checkpoints and closes them.
func (c *Conn) Close() error {
	c.sel = nil
	return nil
}

// setTimes gives a file the message's own date.
//
// Both timestamps, not just the modification time: Finder shows "Date Created"
// from st_birthtime, so a store that set only mtime would list every message
// as created on the day of the backup.
func setTimes(path string, t time.Time) error {
	if err := setBirthtime(path, t); err != nil {
		return fmt.Errorf("setting creation time on %s: %w", path, err)
	}
	if err := os.Chtimes(path, t, t); err != nil {
		return fmt.Errorf("setting modification time on %s: %w", path, err)
	}
	return nil
}

// maxHeader bounds how much of a message is read looking for the blank line
// that ends its header. A message without one is not a message.
const maxHeader = 1 << 20

// readHeader reads a message's header and stops at the blank line.
func readHeader(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the path is one this package built from a UID
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 0, 8192)
	chunk := make([]byte, 8192)
	for len(buf) < maxHeader {
		n, err := f.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if i := endOfHeader(buf); i >= 0 {
			return buf[:i], nil
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

func endOfHeader(b []byte) int {
	if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
		return i + 2
	}
	if i := bytes.Index(b, []byte("\n\n")); i >= 0 {
		return i + 1
	}
	return -1
}

// selectFields returns only the named header fields, keeping their original
// bytes and their folding.
func selectFields(header []byte, fields []string) []byte {
	want := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		want[strings.ToLower(f)] = struct{}{}
	}

	var out bytes.Buffer
	keeping := false
	for _, line := range splitLines(header) {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if keeping {
				out.Write(line)
			}
			continue
		}
		name, _, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			keeping = false
			continue
		}
		_, keeping = want[strings.ToLower(strings.TrimSpace(string(name)))]
		if keeping {
			out.Write(line)
		}
	}
	out.WriteString("\r\n")
	return out.Bytes()
}

// splitLines splits a header into lines, keeping their terminators.
func splitLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, b)
			break
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

// headerContains reports whether a header field holds a value. An empty value
// asks only whether the field is present, which is what IMAP HEADER means.
func headerContains(header []byte, field, value string) bool {
	for _, v := range headerValues(header, field) {
		if value == "" || strings.Contains(strings.ToLower(v), strings.ToLower(value)) {
			return true
		}
	}
	return false
}

// headerValues returns every value of a header field, unfolded.
func headerValues(header []byte, field string) []string {
	var (
		out     []string
		current strings.Builder
		keeping bool
	)
	flush := func() {
		if keeping {
			out = append(out, current.String())
			current.Reset()
		}
	}
	for _, line := range splitLines(header) {
		text := strings.TrimRight(string(line), "\r\n")
		if strings.HasPrefix(text, " ") || strings.HasPrefix(text, "\t") {
			if keeping {
				current.WriteString(" " + strings.TrimSpace(text))
			}
			continue
		}
		flush()
		name, value, ok := strings.Cut(text, ":")
		keeping = ok && strings.EqualFold(strings.TrimSpace(name), field)
		if keeping {
			current.WriteString(strings.TrimSpace(value))
		}
	}
	flush()
	return out
}
