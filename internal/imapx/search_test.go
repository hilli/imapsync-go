package imapx_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/searchkey"
)

// traceBuf collects the protocol conversation. The client writes to it from
// its own goroutines, so it needs a lock of its own.
type traceBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *traceBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *traceBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func dialMemWithTrace(t *testing.T, addr string, trace *traceBuf) imapx.Conn {
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
		Addr:        config.Address{Host: host, Port: portNum, User: testUser, TLS: config.TLSNone},
		Password:    testPassword,
		DebugWriter: trace,
	})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// searchCommand pulls the one UID SEARCH the client sent out of the trace, so
// a test can assert on what actually went over the wire rather than on what
// the parser believes it built.
//
// The trace carries both directions with no marker for which is which, so the
// command is recognised by the tag sitting immediately before it. That also
// rules out the server's own "OK UID SEARCH completed", which quotes the
// command name back and would otherwise be counted as a second one.
var clientSearch = regexp.MustCompile(`^[^ ]+ (UID SEARCH .+)$`)

func searchCommand(t *testing.T, trace *traceBuf) string {
	t.Helper()

	var found []string
	for _, line := range strings.Split(trace.String(), "\n") {
		if m := clientSearch.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			found = append(found, m[1])
		}
	}
	if len(found) != 1 {
		t.Fatalf("trace holds %d UID SEARCH commands, want exactly 1:\n%s", len(found), trace.String())
	}
	return found[0]
}

// TestSearchSendsTheKeyAsItWasWritten is the test the whole parser exists to
// pass. Every other test here asks what the parser built; this one asks what
// the server was told, which is the only thing that decides which messages a
// run copies.
//
// A search rewritten on its way to the wire — a bound dropped, an OR flattened,
// a date shifted — would still parse, still run, and still silently copy the
// wrong set of messages.
func TestSearchSendsTheKeyAsItWasWritten(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"ALL", "UID SEARCH ALL"},
		{"UNSEEN", "UID SEARCH UNSEEN"},
		{"unseen", "UID SEARCH UNSEEN"},
		{"SEEN UNDELETED", "UID SEARCH SEEN UNDELETED"},
		{"SMALLER 1000", "UID SEARCH SMALLER 1000"},
		// The bound must survive the key after it; go-imap's own intersection
		// would have dropped it here.
		{"SMALLER 1000 UNSEEN", "UID SEARCH UNSEEN SMALLER 1000"},
		{"LARGER 10 SMALLER 1000", "UID SEARCH LARGER 10 SMALLER 1000"},
		{"SINCE 1-Feb-2020", `UID SEARCH SINCE "1-Feb-2020"`},
		{"SENTBEFORE 1-Feb-2020", `UID SEARCH SENTBEFORE "1-Feb-2020"`},
		// ON has no field of its own and travels as a half-open day, so this
		// is the proof that it arrives as ON rather than as two bounds.
		{"ON 1-Feb-2020", `UID SEARCH ON "1-Feb-2020"`},
		{"SENTON 1-Feb-2020", `UID SEARCH SENTON "1-Feb-2020"`},
		{"SUBJECT invoice", `UID SEARCH SUBJECT "invoice"`},
		{`SUBJECT "two words"`, `UID SEARCH SUBJECT "two words"`},
		{"HEADER X-Spam-Flag YES", `UID SEARCH HEADER "X-Spam-Flag" "YES"`},
		{"KEYWORD $Junk", `UID SEARCH KEYWORD $Junk`},
		{"NOT SEEN", "UID SEARCH NOT (SEEN)"},
		{"OR SEEN FLAGGED", "UID SEARCH OR (SEEN) (FLAGGED)"},
		{"OR (SEEN FLAGGED) DELETED", "UID SEARCH OR (SEEN FLAGGED) (DELETED)"},
		{"UID 1:100", "UID SEARCH UID 1:100"},
		{"UID 5,9,12:*", "UID SEARCH UID 5,9,12:*"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()

			key, err := searchkey.Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tc.in, err)
			}

			addr, _ := startMemServer(t, rev2Caps())
			trace := &traceBuf{}
			conn := dialMemWithTrace(t, addr, trace)
			if _, err := conn.Select(context.Background(), "INBOX", imapx.SelectOptions{}); err != nil {
				t.Fatalf("Select() error = %v", err)
			}
			if _, err := conn.Search(context.Background(), key); err != nil {
				t.Fatalf("Search(%q) error = %v", tc.in, err)
			}

			if got := searchCommand(t, trace); got != tc.want {
				t.Errorf("Parse(%q) went out as\n  %s\nwant\n  %s", tc.in, got, tc.want)
			}
		})
	}
}

// sizedMessage is a message of a chosen total size, so that LARGER and SMALLER
// can be tested against a number rather than against whatever a fixture
// happens to weigh.
func sizedMessage(subject string, size int) []byte {
	head := []byte(fmt.Sprintf(
		"From: sender@example.test\r\n"+
			"To: %s\r\n"+
			"Subject: %s\r\n"+
			"Message-ID: <%s@example.test>\r\n"+
			"Date: Mon, 27 Aug 2026 12:00:00 +0000\r\n"+
			"\r\n", testUser, subject, subject))
	if len(head) >= size {
		return head
	}
	return append(head, bytes.Repeat([]byte("x"), size-len(head))...)
}

// TestSearchReturnsWhatTheServerMatched runs the parsed keys against a real
// mailbox, because a command that is well-formed on the wire can still mean
// something other than what was asked for.
func TestSearchReturnsWhatTheServerMatched(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)

	messages := []struct {
		subject string
		size    int
		flags   []string
	}{
		{"alpha", 400, []string{"\\Seen"}},
		{"beta", 400, nil},
		{"gamma", 4000, []string{"\\Seen", "\\Flagged"}},
	}
	uids := map[string]uint32{}
	for _, m := range messages {
		body := sizedMessage(m.subject, m.size)
		res, err := conn.Append(ctx, "INBOX", imapx.AppendMessage{
			Size:         int64(len(body)),
			Flags:        m.flags,
			InternalDate: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
			Body:         bytes.NewReader(body),
		})
		if err != nil {
			t.Fatalf("Append(%s) error = %v", m.subject, err)
		}
		uids[m.subject] = res.UID
	}
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	tests := []struct {
		search string
		want   []string
	}{
		{"ALL", []string{"alpha", "beta", "gamma"}},
		{"SEEN", []string{"alpha", "gamma"}},
		{"UNSEEN", []string{"beta"}},
		{"NOT SEEN", []string{"beta"}},
		{"SEEN FLAGGED", []string{"gamma"}},
		{"OR UNSEEN FLAGGED", []string{"beta", "gamma"}},
		{"SUBJECT alpha", []string{"alpha"}},
		{"LARGER 1000", []string{"gamma"}},
		{"SMALLER 1000", []string{"alpha", "beta"}},
		// The intersection that go-imap's And would have widened into
		// "everything unseen", 400-byte message and all.
		{"SMALLER 1000 SEEN", []string{"alpha"}},
		{"SUBJECT nothing", nil},
	}

	for _, tc := range tests {
		t.Run(tc.search, func(t *testing.T) {
			key, err := searchkey.Parse(tc.search)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tc.search, err)
			}
			got, err := conn.Search(ctx, key)
			if err != nil {
				t.Fatalf("Search(%q) error = %v", tc.search, err)
			}

			var want []uint32
			for _, subject := range tc.want {
				want = append(want, uids[subject])
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("Search(%q) = %v, want %v (%v)", tc.search, got, want, tc.want)
			}
		})
	}
}

// A search that matches nothing is an answer, not a failure. Treating it as
// one would invite a caller to fall back to copying everything, which is the
// opposite of what was asked for.
func TestASearchMatchingNothingIsNotAnError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	key, err := searchkey.Parse("SUBJECT nothing-is-here")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	got, err := conn.Search(ctx, key)
	if err != nil {
		t.Fatalf("Search() error = %v, want no error for an empty result", err)
	}
	if len(got) != 0 {
		t.Errorf("Search() = %v, want nothing", got)
	}
}

func TestSearchingWithNoKeyIsRefused(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	if _, err := conn.Search(ctx, searchkey.Key{}); err == nil {
		t.Error("Search() with the zero key succeeded, want a refusal rather than a search for everything")
	}
}

func TestSearchHonoursACancelledContext(t *testing.T) {
	t.Parallel()

	addr, _ := startMemServer(t, rev2Caps())
	conn := dialMem(t, addr)
	if _, err := conn.Select(context.Background(), "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	key, err := searchkey.Parse("ALL")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := conn.Search(ctx, key); !strings.Contains(fmt.Sprint(err), "context canceled") {
		t.Errorf("Search() error = %v, want a cancelled context", err)
	}
}
