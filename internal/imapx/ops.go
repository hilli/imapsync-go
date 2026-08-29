package imapx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/hilli/imapsync-go/internal/searchkey"
)

// Mailbox is the state of a selected mailbox.
type Mailbox struct {
	Name          string
	NumMessages   uint32
	UIDNext       uint32
	UIDValidity   uint32
	HighestModSeq uint64
	ReadOnly      bool
}

// SelectOptions controls how a mailbox is opened.
type SelectOptions struct {
	// ReadOnly issues EXAMINE instead of SELECT. The source side uses it so a
	// sync cannot alter the mailbox it is reading.
	ReadOnly bool
}

// MessageMeta is everything about a message except its body.
type MessageMeta struct {
	UID          uint32
	Flags        []string
	InternalDate time.Time
	// Size is the server's RFC822.SIZE. It is a claim, not a measurement: the
	// byte count APPEND needs must come from the body actually fetched.
	Size int64
	// Header is the raw bytes of the selected header fields, for digesting.
	Header []byte
	ModSeq uint64
}

// AppendMessage is one message to be written to a mailbox.
type AppendMessage struct {
	// Size must equal the number of bytes Body yields. APPEND declares the
	// literal length before sending it, so a wrong value desynchronises the
	// protocol stream rather than merely failing the command.
	Size         int64
	Flags        []string
	InternalDate time.Time
	Body         io.Reader
}

// AppendResult reports where an appended message landed. Both fields are zero
// unless the server supports UIDPLUS, in which case the pair identifies the
// message exactly and no search is needed to find it again.
type AppendResult struct {
	UID         uint32
	UIDValidity uint32
}

// Assigned reports whether the server told us where the message landed.
func (r AppendResult) Assigned() bool { return r.UID != 0 && r.UIDValidity != 0 }

// SyncOps is the set of operations a sync performs on one connection.
//
// Every method takes a context but honours it only between commands, never
// during one: go-imap's command API predates context and offers no way to
// abandon a command in flight. Checking on entry is what stops a cancelled run
// from issuing the next few thousand commands anyway.
type SyncOps interface {
	Select(ctx context.Context, mailbox string, opts SelectOptions) (Mailbox, error)
	CreateFolder(ctx context.Context, name string) error
	SubscribeFolder(ctx context.Context, name string) error
	AllUIDs(ctx context.Context) ([]uint32, error)
	FetchMeta(ctx context.Context, uids []uint32, headerFields []string) ([]MessageMeta, error)
	FetchBody(ctx context.Context, uid uint32, w io.Writer) (int64, error)
	Append(ctx context.Context, mailbox string, msg AppendMessage) (AppendResult, error)
	SearchHeader(ctx context.Context, field, value string) ([]uint32, error)
	Search(ctx context.Context, key searchkey.Key) ([]uint32, error)
	FetchFlags(ctx context.Context, changedSince uint64) ([]FlagSet, error)
	StoreFlags(ctx context.Context, uid uint32, flags []string) error
	DeleteMessages(ctx context.Context, uids []uint32) error
}

// FlagSet is one message's flags, as the server currently has them.
type FlagSet struct {
	UID   uint32
	Flags []string
}

var _ SyncOps = (*conn)(nil)

// Select opens a mailbox and reports the identifiers a sync is fenced by.
func (c *conn) Select(ctx context.Context, mailbox string, opts SelectOptions) (Mailbox, error) {
	caps := c.Caps()
	selectOpts := &imap.SelectOptions{
		ReadOnly: opts.ReadOnly,
		// Asking for CONDSTORE is what makes HIGHESTMODSEQ appear; without it
		// the server may omit the response entirely even though it supports the
		// extension.
		CondStore: caps.CondStore,
	}

	if err := ctx.Err(); err != nil {
		return Mailbox{}, fmt.Errorf("selecting %q: %w", mailbox, err)
	}

	data, err := c.c.Select(mailbox, selectOpts).Wait()
	if err != nil {
		return Mailbox{}, fmt.Errorf("selecting %q: %w", mailbox, err)
	}
	if data == nil {
		return Mailbox{}, fmt.Errorf("selecting %q: server returned no mailbox data", mailbox)
	}
	if data.UIDValidity == 0 {
		// Without UIDVALIDITY there is no way to tell a renumbered mailbox from
		// an unchanged one, so every recorded UID becomes untrustworthy. That is
		// worse than refusing to sync the folder.
		return Mailbox{}, fmt.Errorf("selecting %q: server reported no UIDVALIDITY", mailbox)
	}

	return Mailbox{
		Name:          mailbox,
		NumMessages:   data.NumMessages,
		UIDNext:       uint32(data.UIDNext),
		UIDValidity:   data.UIDValidity,
		HighestModSeq: data.HighestModSeq,
		ReadOnly:      opts.ReadOnly,
	}, nil
}

// CreateFolder creates a mailbox, treating "it already exists" as success.
//
// Servers disagree about how to say that: RFC 5530 servers send ALREADYEXISTS,
// older ones send a bare NO with prose. Both mean the postcondition the caller
// asked for already holds.
func (c *conn) CreateFolder(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("creating mailbox %q: %w", name, err)
	}

	err := c.c.Create(name, nil).Wait()
	if err == nil {
		return nil
	}

	var imapErr *imap.Error
	if errors.As(err, &imapErr) && imapErr.Code == imap.ResponseCodeAlreadyExists {
		return nil
	}
	return fmt.Errorf("creating mailbox %q: %w", name, err)
}

// SubscribeFolder adds a mailbox to the subscription list.
//
// Subscription is how a mailbox becomes visible in clients that browse by LSUB
// rather than LIST, which is most of them by default. A folder this tool
// created but left unsubscribed exists and holds mail that the owner cannot
// see, which is indistinguishable from not having copied it.
func (c *conn) SubscribeFolder(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("subscribing to mailbox %q: %w", name, err)
	}
	if err := c.c.Subscribe(name).Wait(); err != nil {
		return fmt.Errorf("subscribing to mailbox %q: %w", name, err)
	}
	return nil
}

// AllUIDs returns every UID in the selected mailbox, in ascending order.
func (c *conn) AllUIDs(ctx context.Context) ([]uint32, error) {
	// SEARCH ALL rather than FETCH 1:* UID: the response is a bare UID list
	// instead of one untagged FETCH per message, which matters in a mailbox
	// with four hundred thousand of them.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("searching for all UIDs: %w", err)
	}

	data, err := c.c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("searching for all UIDs: %w", err)
	}
	if data == nil {
		return nil, errors.New("searching for all UIDs: server returned no search data")
	}
	uids := toUint32s(data.AllUIDs())

	// A SEARCH index can disagree with the mailbox it indexes. iCloud's
	// retains expunged messages: its Trash reports 487 EXISTS and answers
	// SEARCH ALL with 100,184 UIDs, of which 99,697 fetch as nothing. INBOX
	// answers 503,763 for 413,933 messages. Both were confirmed against an
	// unrelated client, so this is the server and not our parsing of it.
	//
	// Trusting the larger number is not merely wasteful. Every phantom enters
	// the copy list, is fetched, comes back empty, and is written to the state
	// database as gone — a fifth of the work on that account, repeated every
	// run. So the count is checked against EXISTS, which the same server got
	// right, and a disagreement of any size demotes the answer.
	mbox := c.c.Mailbox()
	if mbox == nil || len(uids) == int(mbox.NumMessages) {
		return uids, nil
	}
	return c.enumerateUIDs(ctx)
}

// enumerateUIDs lists the selected mailbox by walking it rather than searching
// it, for servers whose SEARCH cannot be believed.
//
// It costs one untagged response per message where SEARCH costs one line in
// total — 29 seconds against 2 on a mailbox of 414,000 — which is why it is the
// fallback and not the rule. Against the hours such a mailbox takes to copy it
// is not a cost worth avoiding, and it is only ever paid by a server that has
// already been caught contradicting itself.
//
// The walk is by sequence number. UID FETCH 1:* returns the same messages but
// makes the server cross the whole UID space to find them, which on the mailbox
// above took three times as long.
func (c *conn) enumerateUIDs(ctx context.Context) ([]uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("enumerating UIDs: %w", err)
	}

	all := imap.SeqSet{}
	all.AddRange(1, 0)

	// Streamed rather than collected: the mailboxes that need this are the
	// large ones, and holding a buffer per message is how a fix for a bug
	// becomes a memory-limit breach.
	cmd := c.c.Fetch(all, &imap.FetchOptions{UID: true})
	var uids []uint32
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			if u, ok := item.(imapclient.FetchItemDataUID); ok {
				uids = append(uids, uint32(u.UID))
			}
		}
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("enumerating UIDs: %w", err)
	}

	// Sequence order is UID order on every server that follows the spec, but
	// the caller was promised ascending and this path exists precisely because
	// a server was not following the spec.
	slices.Sort(uids)
	return uids, nil
}

// FetchMeta returns metadata for the given UIDs, plus the raw bytes of the
// requested header fields for identity digesting.
//
// An empty headerFields asks for no header at all. It is emphatically not a
// request for the whole of it: BODY.PEEK[HEADER] downloads every header byte of
// every message named, which is the most expensive thing this function can do,
// and a caller that named no fields is a caller that wants none. The one caller
// in that position — a dry run counting what a filter would leave out — needs
// only sizes and dates, and used to pay for complete headers to get them.
//
// The fetch is a peek: a synchronisation must not mark the source as read.
func (c *conn) FetchMeta(ctx context.Context, uids []uint32, headerFields []string) ([]MessageMeta, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("fetching metadata: %w", err)
	}

	opts := &imap.FetchOptions{
		UID:          true,
		Flags:        true,
		InternalDate: true,
		RFC822Size:   true,
		ModSeq:       c.Caps().CondStore,
	}
	if len(headerFields) > 0 {
		opts.BodySection = []*imap.FetchItemBodySection{{
			Specifier:    imap.PartSpecifierHeader,
			Peek:         true,
			HeaderFields: headerFields,
		}}
	}

	buffers, err := c.c.Fetch(toUIDSet(uids), opts).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetching metadata for %d messages: %w", len(uids), err)
	}

	out := make([]MessageMeta, 0, len(buffers))
	for _, buf := range buffers {
		m := MessageMeta{
			UID:          uint32(buf.UID),
			InternalDate: buf.InternalDate,
			Size:         buf.RFC822Size,
			ModSeq:       buf.ModSeq,
		}
		for _, f := range buf.Flags {
			m.Flags = append(m.Flags, string(f))
		}
		if len(buf.BodySection) > 0 {
			m.Header = buf.BodySection[0].Bytes
		}
		out = append(out, m)
	}
	return out, nil
}

// FetchFlags returns the flags of every message in the selected mailbox, or —
// when changedSince is non-zero and the server supports CONDSTORE — only those
// whose flags have changed since that modification sequence.
//
// The distinction is the difference between a command and a download. iCloud's
// INBOX holds 414k messages, and asking for all of their flags on every run to
// discover that three of them changed is the cost CONDSTORE exists to remove.
// A caller that passes a modification sequence to a server that does not
// support the extension would have it silently ignored and get everything, so
// the option is only sent when the capability is there.
func (c *conn) FetchFlags(ctx context.Context, changedSince uint64) ([]FlagSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("fetching flags: %w", err)
	}

	opts := &imap.FetchOptions{UID: true, Flags: true}
	if changedSince != 0 && c.Caps().CondStore {
		opts.ChangedSince = changedSince
	}

	all := imap.UIDSet{}
	all.AddRange(1, 0)

	buffers, err := c.c.Fetch(all, opts).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetching flags: %w", err)
	}

	out := make([]FlagSet, 0, len(buffers))
	for _, buf := range buffers {
		fs := FlagSet{UID: uint32(buf.UID), Flags: make([]string, 0, len(buf.Flags))}
		for _, f := range buf.Flags {
			fs.Flags = append(fs.Flags, string(f))
		}
		out = append(out, fs)
	}
	return out, nil
}

// StoreFlags replaces one message's flags with the given set.
//
// Replacement rather than a computed add and remove: the destination is a copy
// of the source, so what the source says is the answer, and a difference
// arrived at by two commands is a difference that can be half-applied.
func (c *conn) StoreFlags(ctx context.Context, uid uint32, flags []string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("storing flags on UID %d: %w", uid, err)
	}

	set := make([]imap.Flag, 0, len(flags))
	for _, f := range flags {
		set = append(set, imap.Flag(f))
	}

	store := &imap.StoreFlags{Op: imap.StoreFlagsSet, Silent: true, Flags: set}
	if err := c.c.Store(toUIDSet([]uint32{uid}), store, nil).Close(); err != nil {
		return fmt.Errorf("storing flags on UID %d: %w", uid, err)
	}
	return nil
}

// ErrMessageGone means the server returned no data for a UID that was expected
// to exist, which happens when a message is deleted between enumeration and
// fetch. It is normal, not a failure of the run.
var ErrMessageGone = errors.New("message no longer exists")

// FetchBody streams one message to w and returns the number of bytes written.
//
// The count is the whole point: APPEND must declare a literal length up front,
// and RFC822.SIZE is a claim servers get wrong. Only what was actually received
// can be trusted. The body is streamed rather than collected so a large message
// costs a buffer, not its own size in memory.
func (c *conn) FetchBody(ctx context.Context, uid uint32, w io.Writer) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("fetching body of UID %d: %w", uid, err)
	}

	opts := &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}
	cmd := c.c.Fetch(toUIDSet([]uint32{uid}), opts)

	var (
		written int64
		found   bool
		copyErr error
	)
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			section, ok := item.(imapclient.FetchItemDataBodySection)
			if !ok || section.Literal == nil {
				continue
			}
			found = true
			n, err := io.Copy(w, section.Literal)
			written += n
			if err != nil && copyErr == nil {
				copyErr = err
			}
		}
	}

	// Close drains the command and returns the tagged completion. It must run
	// even when the copy failed, or the connection is left mid-response.
	if err := cmd.Close(); err != nil {
		return written, fmt.Errorf("fetching body of UID %d: %w", uid, err)
	}
	if copyErr != nil {
		return written, fmt.Errorf("fetching body of UID %d: %w", uid, copyErr)
	}
	if !found {
		return 0, fmt.Errorf("fetching body of UID %d: %w", uid, ErrMessageGone)
	}
	return written, nil
}

// ErrConnectionBroken means the connection was destroyed mid-command and cannot
// be reused. It is returned when an APPEND literal could not be completed: the
// declared length is already on the wire, so the server goes on reading message
// data as if it were commands and no tagged response will ever arrive.
var ErrConnectionBroken = errors.New("connection desynchronised and closed")

// ErrAtCapacity means a connection was refused because the server already holds
// as many as it will allow.
//
// It is a judgement rather than something a server says. A server at its limit
// may answer with NO [LIMIT] or a "too many connections" text, both of which
// speak for themselves — but it may equally just hang up during authentication,
// which arrives as an unexpected EOF and is byte-identical to a dropped network
// connection. Nothing in the error distinguishes them, so whoever wraps this
// has to have looked at the surroundings; see pool.Pool.
var ErrAtCapacity = errors.New("server is at its connection limit")

// Append writes a message to a mailbox and reports where it landed.
//
// msg.Size must be exact. IMAP declares a literal's length before its bytes, and
// the protocol offers no way to take that back, so a body that does not match is
// unrecoverable: the connection is closed rather than left to hang waiting for a
// response the server will never send.
func (c *conn) Append(ctx context.Context, mailbox string, msg AppendMessage) (AppendResult, error) {
	if err := ctx.Err(); err != nil {
		return AppendResult{}, fmt.Errorf("appending to %q: %w", mailbox, err)
	}

	opts := &imap.AppendOptions{Time: msg.InternalDate}
	for _, f := range msg.Flags {
		opts.Flags = append(opts.Flags, imap.Flag(f))
	}

	cmd := c.c.Append(mailbox, msg.Size, opts)

	written, copyErr := io.Copy(cmd, msg.Body)
	closeErr := cmd.Close()

	if written != msg.Size || closeErr != nil {
		// Only part of the literal reached the server. Waiting for a completion
		// would block until the process dies, so give up the connection instead
		// and let the caller open a fresh one.
		_ = c.c.Close()
		reason := fmt.Sprintf("declared %d bytes but sent %d", msg.Size, written)
		if copyErr != nil {
			reason = fmt.Sprintf("%s: %v", reason, copyErr)
		}
		return AppendResult{}, fmt.Errorf("appending to %q: %s: %w", mailbox, reason, ErrConnectionBroken)
	}

	data, err := cmd.Wait()
	if copyErr != nil {
		// The literal is complete, so the connection is still usable, but the
		// body was not read cleanly and the message must not count as copied.
		return AppendResult{}, fmt.Errorf("appending to %q: reading message body: %w", mailbox, copyErr)
	}
	if err != nil {
		return AppendResult{}, fmt.Errorf("appending to %q: %w", mailbox, err)
	}
	if data == nil {
		// No APPENDUID. The message may well have been stored; the caller has to
		// find it by digest rather than assume either way.
		return AppendResult{}, nil
	}
	return AppendResult{UID: uint32(data.UID), UIDValidity: data.UIDValidity}, nil
}

// SearchHeader finds messages in the selected mailbox whose header field
// contains value. It is how a message appended without UIDPLUS, or one left in
// flight by a crash, is located again.
func (c *conn) SearchHeader(ctx context.Context, field, value string) ([]uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("searching for header %s: %w", field, err)
	}

	criteria := &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: field, Value: value}},
	}
	data, err := c.c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("searching for header %s: %w", field, err)
	}
	if data == nil {
		return nil, fmt.Errorf("searching for header %s: server returned no search data", field)
	}
	return toUint32s(data.AllUIDs()), nil
}

// Search runs a UID SEARCH in the selected mailbox and returns what matched.
//
// A search that matches nothing returns no UIDs and no error. That is not the
// same as a failure and must not be treated as one: "no message here matches"
// is a perfectly good answer, and the only wrong thing to do with it is to
// fall back to copying everything.
func (c *conn) Search(ctx context.Context, key searchkey.Key) ([]uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("searching for %s: %w", key, err)
	}
	if key.IsZero() {
		return nil, errors.New("searching: no search key was given")
	}

	data, err := c.c.UIDSearch(key.Criteria(), nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("searching for %s: %w", key, err)
	}
	if data == nil {
		return nil, fmt.Errorf("searching for %s: server returned no search data", key)
	}
	return toUint32s(data.AllUIDs()), nil
}

// ErrNoUIDExpunge means the server cannot remove named messages, only every
// \Deleted message in the mailbox at once.
//
// That distinction is the whole safety of deleting anything. Plain EXPUNGE is
// defined to remove every message carrying \Deleted, which includes any the
// account's owner marked themselves and has not yet purged. UIDPLUS adds UID
// EXPUNGE, which removes only the UIDs named and leaves everything else alone.
// Without it there is no way to delete our messages without volunteering to
// delete theirs, so we decline.
var ErrNoUIDExpunge = errors.New("server does not support UID EXPUNGE (UIDPLUS)")

// DeleteMessages removes the named messages from the selected mailbox.
//
// Deletion in IMAP is two steps: mark, then purge. The mark is \Deleted, which
// on its own changes nothing permanent; the purge is EXPUNGE. Doing only the
// first would leave the messages visible in most clients and would make the
// next run see them still there, so this does both.
func (c *conn) DeleteMessages(ctx context.Context, uids []uint32) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("deleting %d messages: %w", len(uids), err)
	}
	if len(uids) == 0 {
		return nil
	}
	if !c.Caps().UIDPlus {
		return ErrNoUIDExpunge
	}

	set := toUIDSet(uids)
	store := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
	if err := c.c.Store(set, store, nil).Close(); err != nil {
		return fmt.Errorf("marking %d messages deleted: %w", len(uids), err)
	}
	if err := c.c.UIDExpunge(set).Close(); err != nil {
		return fmt.Errorf("expunging %d messages: %w", len(uids), err)
	}
	return nil
}

func toUIDSet(uids []uint32) imap.UIDSet {
	var set imap.UIDSet
	for _, uid := range uids {
		set.AddNum(imap.UID(uid))
	}
	return set
}

func toUint32s(uids []imap.UID) []uint32 {
	out := make([]uint32, len(uids))
	for i, uid := range uids {
		out[i] = uint32(uid)
	}
	return out
}
