package imapx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/config"
)

func TestListOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		caps       Caps
		withStatus bool

		wantNil        bool
		wantSpecialUse bool
		wantStatus     bool
	}{
		{
			// The regression that started this: iCloud answers BAD to any LIST
			// carrying return options, because it never advertised
			// LIST-EXTENDED. A nil options value keeps the command to RFC 3501.
			name:    "no LIST-EXTENDED sends a plain LIST",
			caps:    Caps{SpecialUse: true, ListStatus: true},
			wantNil: true,
		},
		{
			name:       "no LIST-EXTENDED stays plain even when status is wanted",
			caps:       Caps{SpecialUse: true, ListStatus: true},
			withStatus: true,
			wantNil:    true,
		},
		{
			name:           "LIST-EXTENDED without SPECIAL-USE omits that option",
			caps:           Caps{ListExtended: true},
			wantSpecialUse: false,
		},
		{
			name:           "LIST-EXTENDED with SPECIAL-USE requests it",
			caps:           Caps{ListExtended: true, SpecialUse: true},
			wantSpecialUse: true,
		},
		{
			name:       "status is only inlined with LIST-STATUS",
			caps:       Caps{ListExtended: true},
			withStatus: true,
			wantStatus: false,
		},
		{
			name:       "LIST-STATUS inlines status",
			caps:       Caps{ListExtended: true, ListStatus: true},
			withStatus: true,
			wantStatus: true,
		},
		{
			name:       "LIST-STATUS is not used unless status was asked for",
			caps:       Caps{ListExtended: true, ListStatus: true},
			withStatus: false,
			wantStatus: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := listOptions(tt.caps, ListOptions{WithStatus: tt.withStatus})
			if tt.wantNil {
				if got != nil {
					t.Fatalf("listOptions() = %+v, want nil so no return options are sent", got)
				}
				return
			}
			if got == nil {
				t.Fatal("listOptions() = nil, want options")
			}
			if got.ReturnSpecialUse != tt.wantSpecialUse {
				t.Errorf("ReturnSpecialUse = %v, want %v", got.ReturnSpecialUse, tt.wantSpecialUse)
			}
			if hasStatus := got.ReturnStatus != nil; hasStatus != tt.wantStatus {
				t.Errorf("ReturnStatus present = %v, want %v", hasStatus, tt.wantStatus)
			}
		})
	}
}

func TestIsProtocolRejection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"BAD is a rejection", &imap.Error{Type: imap.StatusResponseTypeBad, Text: "Parse Error"}, true},
		{"NO is a rejection", &imap.Error{Type: imap.StatusResponseTypeNo, Text: "nope"}, true},
		{"OK is not", &imap.Error{Type: imap.StatusResponseTypeOK}, false},
		{"a network error is not", net.ErrClosed, false},
		{"nil is not", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isProtocolRejection(tt.err); got != tt.want {
				t.Errorf("isProtocolRejection(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestListFoldersPlainServer covers a server that never advertised
// LIST-EXTENDED. Sending return options to it is a hard failure, so the command
// must go out in its plain form and the per-folder STATUS must still happen.
func TestListFoldersPlainServer(t *testing.T) {
	t.Parallel()

	srv := startFakeServer(t, fakeServerOptions{
		caps: "IMAP4rev1 NAMESPACE UIDPLUS",
	})

	conn := dialFake(t, srv)
	defer func() { _ = conn.Close() }()

	folders, err := conn.ListFolders(context.Background(), ListOptions{WithStatus: true})
	if err != nil {
		t.Fatalf("ListFolders() error = %v", err)
	}
	if len(folders) != 2 {
		t.Fatalf("got %d folders, want 2: %+v", len(folders), folders)
	}

	list := srv.commandsMatching("LIST")
	if len(list) != 1 {
		t.Fatalf("got %d LIST commands, want 1: %v", len(list), list)
	}
	if strings.Contains(strings.ToUpper(list[0]), "RETURN") {
		t.Errorf("LIST carried return options against a server without LIST-EXTENDED: %q", list[0])
	}

	if got := len(srv.commandsMatching("STATUS")); got != 2 {
		t.Errorf("got %d STATUS commands, want one per folder", got)
	}
	for _, f := range folders {
		if f.NumMessages == nil {
			t.Errorf("folder %q has no message count", f.Name)
		}
		if f.UIDValidity == 0 {
			t.Errorf("folder %q has no UIDVALIDITY", f.Name)
		}
	}
}

// TestDialBoundsEstablishment covers a server that accepts the connection and
// then says nothing. The dial timeout has to cover the whole handshake, not
// just the TCP connect, or a single unresponsive server stalls a migration
// indefinitely.
func TestDialBoundsEstablishment(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Hold the connection open and silent until the test finishes.
		t.Cleanup(func() { _ = conn.Close() })
	}()

	host, port := splitHostPortForTest(t, ln.Addr().String())

	done := make(chan error, 1)
	go func() {
		_, err := Dial(context.Background(), DialOptions{
			Addr:     config.Address{Host: host, Port: port, User: "user", TLS: config.TLSNone},
			Password: "pass",
			Timeout:  500 * time.Millisecond,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Dial() succeeded against a server that never spoke")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Dial() hung on a silent server instead of honouring the timeout")
	}
}

// TestListFoldersFallsBackOnRejection covers a server that advertises
// LIST-EXTENDED and then refuses to honour it. Capability strings are claims,
// not guarantees, so a refusal must degrade rather than abort the run.
func TestListFoldersFallsBackOnRejection(t *testing.T) {
	t.Parallel()

	srv := startFakeServer(t, fakeServerOptions{
		caps:               "IMAP4rev1 LIST-EXTENDED LIST-STATUS SPECIAL-USE NAMESPACE",
		rejectExtendedList: true,
	})

	conn := dialFake(t, srv)
	defer func() { _ = conn.Close() }()

	folders, err := conn.ListFolders(context.Background(), ListOptions{WithStatus: true})
	if err != nil {
		t.Fatalf("ListFolders() error = %v, want a successful retry", err)
	}
	if len(folders) != 2 {
		t.Fatalf("got %d folders, want 2", len(folders))
	}

	list := srv.commandsMatching("LIST")
	if len(list) != 2 {
		t.Fatalf("got %d LIST commands, want the rejected one plus a retry: %v", len(list), list)
	}
	if !strings.Contains(strings.ToUpper(list[0]), "RETURN") {
		t.Errorf("first LIST should have used the advertised extension: %q", list[0])
	}
	if strings.Contains(strings.ToUpper(list[1]), "RETURN") {
		t.Errorf("retry repeated the rejected syntax: %q", list[1])
	}

	// The retry loses inline status, so it must be recovered per folder.
	if got := len(srv.commandsMatching("STATUS")); got != 2 {
		t.Errorf("got %d STATUS commands, want one per folder after losing LIST-STATUS", got)
	}
}

func TestCapsBackfillsSpecialUseForIMAP4rev2(t *testing.T) {
	t.Parallel()

	srv := startFakeServer(t, fakeServerOptions{caps: "IMAP4rev2"})
	conn := dialFake(t, srv)
	defer func() { _ = conn.Close() }()

	caps := conn.Caps()
	for name, got := range map[string]bool{
		"IMAP4rev2":     caps.IMAP4rev2,
		"SPECIAL-USE":   caps.SpecialUse,
		"LIST-EXTENDED": caps.ListExtended,
		"LIST-STATUS":   caps.ListStatus,
		"UIDPLUS":       caps.UIDPlus,
		"MOVE":          caps.Move,
	} {
		if !got {
			t.Errorf("%s should be implied by IMAP4rev2", name)
		}
	}
	if caps.CondStore {
		t.Error("CONDSTORE is not implied by IMAP4rev2 and must not be assumed")
	}
}

// TestTracedSessionNeverLeaksPassword exercises the tracer through a real
// connection. go-imap feeds its debug writer from the read goroutine and from
// whichever goroutine sent the command, so this is the only test that puts the
// tracer under genuine concurrency; run with -race it also covers the writer's
// internal state.
func TestTracedSessionNeverLeaksPassword(t *testing.T) {
	t.Parallel()

	const password = "correct-horse-battery-staple"

	srv := startFakeServer(t, fakeServerOptions{caps: "IMAP4rev1 NAMESPACE"})

	var trace lockedBuffer
	conn := dialFakeWithTrace(t, srv, &trace, password)
	defer func() { _ = conn.Close() }()

	if _, err := conn.ListFolders(context.Background(), ListOptions{WithStatus: true}); err != nil {
		t.Fatalf("ListFolders() error = %v", err)
	}
	if _, err := conn.Namespaces(context.Background()); err != nil {
		t.Fatalf("Namespaces() error = %v", err)
	}

	got := trace.String()
	if strings.Contains(got, password) {
		t.Fatalf("password reached the trace output:\n%s", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("trace never redacted anything, so it probably captured nothing:\n%s", got)
	}
	if !strings.Contains(got, "LIST") {
		t.Errorf("trace is missing the commands it exists to show:\n%s", got)
	}
}

// lockedBuffer makes reading the trace safe while go-imap may still be writing.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func dialFake(t *testing.T, srv *fakeServer) Conn {
	t.Helper()
	return dialFakeWithTrace(t, srv, nil, "pass")
}

func dialFakeWithTrace(t *testing.T, srv *fakeServer, trace io.Writer, password string) Conn {
	t.Helper()

	host, port := splitHostPortForTest(t, srv.addr())
	conn, err := Dial(context.Background(), DialOptions{
		Addr: config.Address{
			Host: host,
			Port: port,
			User: "user",
			TLS:  config.TLSNone,
		},
		Password:    password,
		DebugWriter: trace,
	})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	return conn
}

func splitHostPortForTest(t *testing.T, addr string) (string, int) {
	t.Helper()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q) error = %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port %q: %v", portStr, err)
	}
	return host, port
}

type fakeServerOptions struct {
	caps string

	// rejectExtendedList makes the server answer BAD to a LIST carrying return
	// options, imitating a server whose capability list overpromises.
	rejectExtendedList bool

	// selectWithoutUIDValidity imitates a server that completes SELECT without
	// telling the client how to detect renumbering.
	selectWithoutUIDValidity bool
}

// fakeServer is a hand-rolled IMAP server that answers just enough to exercise
// LIST negotiation. A real server implementation would parse the return options
// rather than reject them, which is precisely the behaviour under test.
type fakeServer struct {
	ln   net.Listener
	opts fakeServerOptions

	mu       sync.Mutex
	commands []string
}

func startFakeServer(t *testing.T, opts fakeServerOptions) *fakeServer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := &fakeServer{ln: ln, opts: opts}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serve(conn)
		}
	}()

	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return srv
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) commandsMatching(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string
	for _, c := range s.commands {
		fields := strings.Fields(c)
		if len(fields) >= 2 && strings.EqualFold(fields[1], name) {
			out = append(out, c)
		}
	}
	return out
}

func (s *fakeServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	w := bufio.NewWriter(conn)
	send := func(format string, a ...any) {
		_, _ = fmt.Fprintf(w, format+"\r\n", a...)
		_ = w.Flush()
	}

	send("* OK [CAPABILITY %s] fake server ready", s.opts.caps)

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()

		s.mu.Lock()
		s.commands = append(s.commands, line)
		s.mu.Unlock()

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tag, cmd := fields[0], strings.ToUpper(fields[1])

		switch cmd {
		case "CAPABILITY":
			send("* CAPABILITY %s", s.opts.caps)
			send("%s OK done", tag)

		case "LOGIN":
			send("%s OK logged in", tag)

		case "NAMESPACE":
			send(`* NAMESPACE (("" "/")) NIL NIL`)
			send("%s OK done", tag)

		case "LIST":
			if s.opts.rejectExtendedList && strings.Contains(strings.ToUpper(line), "RETURN") {
				send("%s BAD Parse Error", tag)
				continue
			}
			send(`* LIST (\HasNoChildren) "/" "INBOX"`)
			send(`* LIST (\HasNoChildren \Sent) "/" "Sent Messages"`)
			send("%s OK done", tag)

		case "SELECT", "EXAMINE":
			if s.opts.selectWithoutUIDValidity {
				send("* 3 EXISTS")
				send("%s OK [READ-WRITE] done", tag)
				continue
			}
			send("* 3 EXISTS")
			send("* OK [UIDVALIDITY 42] uids valid")
			send("* OK [UIDNEXT 9] predicted next")
			if strings.Contains(strings.ToUpper(s.opts.caps), "CONDSTORE") {
				send("* OK [HIGHESTMODSEQ 406622125881845] highest")
			}
			send("%s OK [READ-WRITE] done", tag)

		case "STATUS":
			send(`* STATUS "%s" (MESSAGES 3 UIDNEXT 4 UIDVALIDITY 1)`, commandMailbox(line, fields))
			send("%s OK done", tag)

		case "LOGOUT":
			send("* BYE bye")
			send("%s OK done", tag)
			return

		default:
			send("%s OK done", tag)
		}
	}
}

// commandMailbox extracts the mailbox argument, which may be a quoted string
// containing spaces. Splitting on whitespace mangles "Sent Messages", and a
// STATUS reply naming the wrong mailbox is silently discarded by the client.
func commandMailbox(line string, fields []string) string {
	if i := strings.Index(line, `"`); i >= 0 {
		if j := strings.Index(line[i+1:], `"`); j >= 0 {
			return line[i+1 : i+1+j]
		}
	}
	if len(fields) >= 3 {
		return fields[2]
	}
	return "INBOX"
}

// TestSelectAsksForCondStoreOnlyWhenAdvertised applies the rule that a client
// must never send syntax the server has not offered. SELECT (CONDSTORE) is
// CONDSTORE syntax; a server without the extension answers BAD to the whole
// command, which would take the folder out of the sync entirely.
func TestSelectAsksForCondStoreOnlyWhenAdvertised(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		caps          string
		wantCondStore bool
	}{
		{"server with CONDSTORE", "IMAP4rev1 CONDSTORE", true},
		{"server without CONDSTORE", "IMAP4rev1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := startFakeServer(t, fakeServerOptions{caps: tc.caps})
			conn := dialFake(t, srv)
			defer func() { _ = conn.Close() }()

			mbox, err := conn.Select(context.Background(), "INBOX", SelectOptions{})
			if err != nil {
				t.Fatalf("Select() error = %v", err)
			}

			sent := srv.commandsMatching("SELECT")
			if len(sent) != 1 {
				t.Fatalf("got %d SELECT commands, want 1: %v", len(sent), sent)
			}
			gotCondStore := strings.Contains(strings.ToUpper(sent[0]), "CONDSTORE")
			if gotCondStore != tc.wantCondStore {
				t.Errorf("sent %q, CONDSTORE present = %v, want %v", sent[0], gotCondStore, tc.wantCondStore)
			}

			if mbox.UIDValidity != 42 {
				t.Errorf("UIDValidity = %d, want 42", mbox.UIDValidity)
			}
			if mbox.NumMessages != 3 {
				t.Errorf("NumMessages = %d, want 3", mbox.NumMessages)
			}
			if tc.wantCondStore && mbox.HighestModSeq != 406622125881845 {
				t.Errorf("HighestModSeq = %d, want the value the server reported", mbox.HighestModSeq)
			}
		})
	}
}

// TestSelectRefusesAMailboxWithoutUIDValidity covers a server that completes
// SELECT without saying how renumbering can be detected. Syncing the folder
// anyway would make every recorded UID untrustworthy, which is worse than
// leaving the folder alone and saying so.
func TestSelectRefusesAMailboxWithoutUIDValidity(t *testing.T) {
	t.Parallel()

	srv := startFakeServer(t, fakeServerOptions{
		caps:                     "IMAP4rev1",
		selectWithoutUIDValidity: true,
	})
	conn := dialFake(t, srv)
	defer func() { _ = conn.Close() }()

	_, err := conn.Select(context.Background(), "INBOX", SelectOptions{})
	if err == nil {
		t.Fatal("Select() accepted a mailbox with no UIDVALIDITY")
	}
	if !strings.Contains(err.Error(), "UIDVALIDITY") {
		t.Errorf("error = %v, want it to name the missing UIDVALIDITY", err)
	}
}
