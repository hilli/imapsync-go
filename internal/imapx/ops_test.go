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
}
