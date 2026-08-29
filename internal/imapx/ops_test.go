package imapx_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/imapx"
)

const (
	testUser     = "sync@example.test"
	testPassword = "correct-horse"
)

// startMemServer runs go-imap's in-memory server so the operations are
// exercised over a real protocol conversation rather than against a mock of our
// own assumptions. It is already a module dependency, so this costs nothing.
func startMemServer(t *testing.T, caps imap.CapSet) (string, *imapmemserver.User) {
	t.Helper()

	user := imapmemserver.NewUser(testUser, testPassword)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("creating INBOX: %v", err)
	}

	mem := imapmemserver.New()
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         caps,
		Logger:       discardLogger{},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return ln.Addr().String(), user
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

func rev2Caps() imap.CapSet {
	return imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}
}

func dialMem(t *testing.T, addr string) imapx.Conn {
	t.Helper()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("splitting %q: %v", addr, err)
	}
	var portNum int
	if _, err := fmt.Sscanf(port, "%d", &portNum); err != nil {
		t.Fatalf("parsing port %q: %v", port, err)
	}

	conn, err := imapx.Dial(context.Background(), imapx.DialOptions{
		Addr: config.Address{
			Host: host,
			Port: portNum,
			User: testUser,
			TLS:  config.TLSNone,
		},
		Password: testPassword,
	})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func testMessage(subject, messageID string) []byte {
	return []byte(strings.ReplaceAll(fmt.Sprintf(
		"From: sender@example.test\r\n"+
			"To: %s\r\n"+
			"Subject: %s\r\n"+
			"Message-ID: <%s>\r\n"+
			"Date: Mon, 27 Aug 2026 12:00:00 +0000\r\n"+
			"\r\n"+
			"Body of %s.\r\n", testUser, subject, messageID, subject), "\n\n", "\n"))
}

// TestAppendRoundTripsExactBytes is the property the whole tool rests on: what
// arrives at the destination must be byte-for-byte what left the source.
func TestAppendRoundTripsExactBytes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	body := testMessage("round trip", "rt@example.test")
	when := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	res, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{
		Size:         int64(len(body)),
		Flags:        []string{"\\Seen"},
		InternalDate: when,
		Body:         bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if !res.Assigned() {
		t.Fatalf("Append() returned no APPENDUID from a UIDPLUS server: %+v", res)
	}

	mbox, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if mbox.UIDValidity != res.UIDValidity {
		t.Errorf("APPENDUID validity %d does not match the mailbox's %d", res.UIDValidity, mbox.UIDValidity)
	}
	if mbox.NumMessages != 1 {
		t.Fatalf("NumMessages = %d, want 1", mbox.NumMessages)
	}

	var got bytes.Buffer
	n, err := conn.FetchBody(ctx, res.UID, &got)
	if err != nil {
		t.Fatalf("FetchBody() error = %v", err)
	}
	if n != int64(got.Len()) {
		t.Errorf("FetchBody returned %d but wrote %d bytes", n, got.Len())
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Errorf("round trip altered the message:\n got %q\nwant %q", got.String(), body)
	}
}

// TestAppendPreservesFlagsAndInternalDate covers the metadata imapsync is
// expected to carry across, which is invisible in a body comparison.
func TestAppendPreservesFlagsAndInternalDate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	body := testMessage("metadata", "meta@example.test")
	when := time.Date(2019, 3, 14, 15, 9, 26, 0, time.UTC)

	res, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{
		Size:         int64(len(body)),
		Flags:        []string{"\\Seen", "\\Flagged"},
		InternalDate: when,
		Body:         bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	metas, err := conn.FetchMeta(ctx, []uint32{res.UID}, nil)
	if err != nil {
		t.Fatalf("FetchMeta() error = %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d metadata rows, want 1", len(metas))
	}
	got := metas[0]

	if !got.InternalDate.Equal(when) {
		t.Errorf("InternalDate = %v, want %v", got.InternalDate, when)
	}
	for _, want := range []string{"\\Seen", "\\Flagged"} {
		if !slices.Contains(got.Flags, want) {
			t.Errorf("flags %v are missing %s", got.Flags, want)
		}
	}
	if got.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", got.Size, len(body))
	}
}

// TestFetchingDoesNotMarkTheSourceSeen pins the peek. A sync reads the source;
// it must not change it. Without BODY.PEEK the first run would silently mark
// every unread message on the source as read.
func TestFetchingDoesNotMarkTheSourceSeen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	body := testMessage("unread", "unread@example.test")
	res, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{
		Size: int64(len(body)),
		Body: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	if _, err := conn.FetchMeta(ctx, []uint32{res.UID}, []string{"Message-ID"}); err != nil {
		t.Fatalf("FetchMeta() error = %v", err)
	}
	if _, err := conn.FetchBody(ctx, res.UID, io.Discard); err != nil {
		t.Fatalf("FetchBody() error = %v", err)
	}

	metas, err := conn.FetchMeta(ctx, []uint32{res.UID}, nil)
	if err != nil {
		t.Fatalf("FetchMeta() error = %v", err)
	}
	if slices.Contains(metas[0].Flags, "\\Seen") {
		t.Errorf("reading the message marked it \\Seen: flags = %v", metas[0].Flags)
	}
}

func TestFetchMetaReturnsHeadersForDigesting(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	body := testMessage("digest me", "digest@example.test")
	res, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{
		Size: int64(len(body)),
		Body: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	metas, err := conn.FetchMeta(ctx, []uint32{res.UID}, []string{"Message-ID", "Subject"})
	if err != nil {
		t.Fatalf("FetchMeta() error = %v", err)
	}
	header := string(metas[0].Header)
	if !strings.Contains(header, "digest@example.test") {
		t.Errorf("header %q is missing the requested Message-ID", header)
	}
	if strings.Contains(header, "Body of digest me") {
		t.Errorf("header %q leaked the body; the field list was ignored", header)
	}
}

// TestFetchMetaAsksForNoHeaderWhenNoFieldsAreNamed.
//
// Naming no fields means wanting no header, not wanting all of it. The
// distinction is worth a test because the expensive reading is the plausible
// one: BODY.PEEK[HEADER] is what an empty field list used to produce, so the
// caller that wanted least — a dry run counting what a filter excludes — paid
// for every header byte of every message it previewed.
func TestFetchMetaAsksForNoHeaderWhenNoFieldsAreNamed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	body := testMessage("no header wanted", "nohdr@example.test")
	res, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{
		Size: int64(len(body)),
		Body: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	metas, err := conn.FetchMeta(ctx, []uint32{res.UID}, nil)
	if err != nil {
		t.Fatalf("FetchMeta() error = %v", err)
	}
	if len(metas[0].Header) != 0 {
		t.Errorf("asked for no header fields but got %d bytes: %q", len(metas[0].Header), metas[0].Header)
	}
	// The rest of the metadata still has to arrive, or the caller gains
	// nothing by asking for less.
	if metas[0].Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", metas[0].Size, len(body))
	}
	if metas[0].InternalDate.IsZero() {
		t.Error("InternalDate was not fetched")
	}
}

// TestAppendRejectsAWrongDeclaredSize guards the protocol stream. The literal
// length goes out before the bytes do, and IMAP has no way to retract it, so a
// mismatch leaves the server reading message content as commands. There is no
// recovery: the only correct outcome is a closed connection and a clear error,
// never a wait for a response that will never come.
func TestAppendRejectsAWrongDeclaredSize(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())

	body := testMessage("short", "short@example.test")

	for _, tc := range []struct {
		name    string
		declare int64
	}{
		{"body shorter than declared", int64(len(body)) + 10},
		{"body longer than declared", int64(len(body)) - 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := dialMem(t, addr)

			done := make(chan error, 1)
			go func() {
				_, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{
					Size: tc.declare,
					Body: bytes.NewReader(body),
				})
				done <- err
			}()

			select {
			case err := <-done:
				if !errors.Is(err, imapx.ErrConnectionBroken) {
					t.Errorf("Append() error = %v, want ErrConnectionBroken", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Append() hung waiting for a response the server will never send")
			}
		})
	}
}

func TestCreateFolderIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	if err := conn.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	// A second run of the same sync must not fail on folders it made last time.
	if err := conn.CreateFolder(ctx, "Archive"); err != nil {
		t.Errorf("CreateFolder() on an existing mailbox error = %v, want nil", err)
	}

	folders, err := conn.ListFolders(ctx, imapx.ListOptions{})
	if err != nil {
		t.Fatalf("ListFolders() error = %v", err)
	}
	var names []string
	for _, f := range folders {
		names = append(names, f.Name)
	}
	if !slices.Contains(names, "Archive") {
		t.Errorf("folders %v do not include the created mailbox", names)
	}
}

func TestAllUIDsEnumeratesTheMailbox(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	var want []uint32
	for i := range 5 {
		body := testMessage(fmt.Sprintf("msg %d", i), fmt.Sprintf("m%d@example.test", i))
		res, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{
			Size: int64(len(body)),
			Body: bytes.NewReader(body),
		})
		if err != nil {
			t.Fatalf("Append() error = %v", err)
		}
		want = append(want, res.UID)
	}
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	got, err := conn.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("AllUIDs() = %v, want %v", got, want)
	}
}

// TestSearchHeaderFindsAnAppendedMessage is the recovery path: after a crash
// between APPEND and the state commit, this is how a message already on the
// destination is found instead of appended twice.
func TestSearchHeaderFindsAnAppendedMessage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	for _, id := range []string{"one@example.test", "two@example.test"} {
		body := testMessage(id, id)
		if _, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{
			Size: int64(len(body)),
			Body: bytes.NewReader(body),
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	got, err := conn.SearchHeader(ctx, "Message-ID", "<two@example.test>")
	if err != nil {
		t.Fatalf("SearchHeader() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("SearchHeader() = %v, want exactly the one matching message", got)
	}

	missing, err := conn.SearchHeader(ctx, "Message-ID", "<nothing@example.test>")
	if err != nil {
		t.Fatalf("SearchHeader() error = %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("SearchHeader() = %v for an absent message, want none", missing)
	}
}

func TestFetchBodyReportsAMissingMessage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	// A message deleted between enumeration and fetch is routine, so it has to
	// be distinguishable from a real failure rather than reported as zero bytes.
	_, err := conn.FetchBody(ctx, 4242, io.Discard)
	if !errors.Is(err, imapx.ErrMessageGone) {
		t.Errorf("FetchBody() error = %v, want ErrMessageGone", err)
	}
}

// TestOperationsRefuseACancelledContext pins the one thing a context can do
// here: stop a cancelled run from issuing further commands.
func TestOperationsRefuseACancelledContext(t *testing.T) {
	t.Parallel()

	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Select() error = %v, want context.Canceled", err)
	}
	if err := conn.CreateFolder(ctx, "Nope"); !errors.Is(err, context.Canceled) {
		t.Errorf("CreateFolder() error = %v, want context.Canceled", err)
	}
	if _, err := conn.AllUIDs(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("AllUIDs() error = %v, want context.Canceled", err)
	}
	if _, err := conn.FetchMeta(ctx, []uint32{1}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("FetchMeta() error = %v, want context.Canceled", err)
	}
	if _, err := conn.FetchBody(ctx, 1, io.Discard); !errors.Is(err, context.Canceled) {
		t.Errorf("FetchBody() error = %v, want context.Canceled", err)
	}
	if _, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{Body: bytes.NewReader(nil)}); !errors.Is(err, context.Canceled) {
		t.Errorf("Append() error = %v, want context.Canceled", err)
	}
	if _, err := conn.SearchHeader(ctx, "Message-ID", "<x@y>"); !errors.Is(err, context.Canceled) {
		t.Errorf("SearchHeader() error = %v, want context.Canceled", err)
	}
	if _, err := conn.FetchFlags(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("FetchFlags() error = %v, want context.Canceled", err)
	}
	if err := conn.StoreFlags(ctx, 1, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("StoreFlags() error = %v, want context.Canceled", err)
	}
}

// TestStoreFlagsReplacesTheSet.
//
// Replacement rather than a computed add and remove: the destination is a copy
// of the source, so a flag the source no longer has must come off, and a
// difference applied as two commands is a difference that can be half-applied.
func TestStoreFlagsReplacesTheSet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	body := testMessage("flagged", "flags@example.test")
	res, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{
		Size:  int64(len(body)),
		Body:  bytes.NewReader(body),
		Flags: []string{"\\Seen", "\\Answered"},
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	if err := conn.StoreFlags(ctx, res.UID, []string{"\\Flagged"}); err != nil {
		t.Fatalf("StoreFlags() error = %v", err)
	}

	got, err := conn.FetchFlags(ctx, 0)
	if err != nil {
		t.Fatalf("FetchFlags() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchFlags() returned %d messages, want 1", len(got))
	}
	if !slices.Equal(got[0].Flags, []string{"\\Flagged"}) {
		t.Errorf("flags = %v, want only \\Flagged: the old set was added to rather than replaced", got[0].Flags)
	}
}

// TestFetchFlagsReadsTheWholeMailbox checks the enumeration a server without
// CONDSTORE forces, which is the fallback the fast path exists to avoid.
func TestFetchFlagsReadsTheWholeMailbox(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	want := map[uint32][]string{}
	for i, flags := range [][]string{{"\\Seen"}, nil, {"\\Flagged", "\\Seen"}} {
		body := testMessage(fmt.Sprintf("msg %d", i), fmt.Sprintf("f%d@example.test", i))
		res, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{
			Size: int64(len(body)), Body: bytes.NewReader(body), Flags: flags,
		})
		if err != nil {
			t.Fatalf("Append() error = %v", err)
		}
		want[res.UID] = flags
	}
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	got, err := conn.FetchFlags(ctx, 0)
	if err != nil {
		t.Fatalf("FetchFlags() error = %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("FetchFlags() returned %d messages, want %d", len(got), len(want))
	}
	for _, fs := range got {
		wantFlags, ok := want[fs.UID]
		if !ok {
			t.Errorf("FetchFlags() returned unknown UID %d", fs.UID)
			continue
		}
		slices.Sort(fs.Flags)
		slices.Sort(wantFlags)
		if !slices.Equal(fs.Flags, wantFlags) {
			t.Errorf("UID %d flags = %v, want %v", fs.UID, fs.Flags, wantFlags)
		}
	}
}

// appendTo puts a message in the mailbox and returns the UID it landed on.
func appendTo(t *testing.T, conn imapx.Conn, subject string) uint32 {
	t.Helper()

	body := testMessage(subject, subject+"@example.test")
	res, err := conn.Append(context.Background(), "INBOX", imapx.AppendMessage{
		Size:         int64(len(body)),
		InternalDate: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
		Body:         bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("Append(%q) error = %v", subject, err)
	}
	if !res.Assigned() {
		t.Fatalf("Append(%q) returned no APPENDUID", subject)
	}
	return res.UID
}

// TestDeleteMessagesRemovesOnlyWhatItWasGiven.
//
// The message marked \Deleted by hand is the point. A plain EXPUNGE is defined
// to purge every message carrying that flag, so a tool that deleted its own
// messages that way would silently purge whatever the account's owner had
// marked and not yet got round to.
func TestDeleteMessagesRemovesOnlyWhatItWasGiven(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	doomed := appendTo(t, conn, "doomed")
	theirs := appendTo(t, conn, "marked by hand")
	keep := appendTo(t, conn, "keep")

	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("re-Select() error = %v", err)
	}
	if err := conn.StoreFlags(ctx, theirs, []string{"\\Deleted"}); err != nil {
		t.Fatalf("StoreFlags() error = %v", err)
	}

	if err := conn.DeleteMessages(ctx, []uint32{doomed}); err != nil {
		t.Fatalf("DeleteMessages() error = %v", err)
	}

	left, err := conn.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}
	if slices.Contains(left, doomed) {
		t.Errorf("UID %d survived deletion", doomed)
	}
	if !slices.Contains(left, theirs) {
		t.Errorf("UID %d was purged, but it was marked \\Deleted by someone else and never named", theirs)
	}
	if !slices.Contains(left, keep) {
		t.Errorf("UID %d was purged and was never named or marked", keep)
	}
}

// TestDeleteMessagesRefusesWithoutUIDPlus.
//
// Refusing is the only safe answer. Without UID EXPUNGE there is no way to purge
// our messages without also purging every other \Deleted message in the mailbox,
// and quietly doing the destructive thing is not a fallback.
func TestDeleteMessagesRefusesWithoutUIDPlus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	conn := dialMem(t, addr)

	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	uid := appendTo(t, conn, "should survive")
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("re-Select() error = %v", err)
	}

	err := conn.DeleteMessages(ctx, []uint32{uid})
	if !errors.Is(err, imapx.ErrNoUIDExpunge) {
		t.Fatalf("DeleteMessages() error = %v, want ErrNoUIDExpunge", err)
	}

	left, err := conn.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}
	if !slices.Contains(left, uid) {
		t.Errorf("UID %d was deleted by a refusal", uid)
	}
}

// TestDeleteMessagesOfNothingTouchesNothing guards the empty set, which is the
// common case on a mirror that is up to date. A UID set built from no UIDs is
// "1:*" in some encodings, which would purge the mailbox.
func TestDeleteMessagesOfNothingTouchesNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	appendTo(t, conn, "bystander one")
	appendTo(t, conn, "bystander two")
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("re-Select() error = %v", err)
	}

	if err := conn.DeleteMessages(ctx, nil); err != nil {
		t.Fatalf("DeleteMessages(nil) error = %v", err)
	}

	left, err := conn.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}
	if len(left) != 2 {
		t.Errorf("deleting nothing left %d of 2 messages", len(left))
	}
}
