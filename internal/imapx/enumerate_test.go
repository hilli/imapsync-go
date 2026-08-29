package imapx_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"testing"

	"github.com/hilli/imapsync-go/internal/imapx"
)

// lyingProxy sits between the client and an honest server and rewrites the
// SEARCH response, leaving every other command alone.
//
// This is how iCloud behaves. Its SEARCH index retains expunged messages, so a
// mailbox holding 487 reports 487 EXISTS and answers SEARCH ALL with 100,184
// UIDs — while FETCH, on the same connection, tells the truth. No in-memory
// server can be made to contradict itself that way, and a wholly scripted one
// would be testing the script. Corrupting one response on the wire leaves
// everything else genuine, which is the shape of the bug.
func lyingProxy(t *testing.T, upstream string, rewrite func(line string) string) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = client.Close() }()
				server, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()

				go func() { _, _ = io.Copy(server, client) }()

				r := bufio.NewReaderSize(server, 1<<20)
				for {
					line, err := r.ReadString('\n')
					if line != "" {
						if _, werr := client.Write([]byte(rewrite(line))); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()

	return ln.Addr().String()
}

// addPhantoms appends UIDs the mailbox does not contain to a SEARCH response,
// covering both the IMAP4rev1 and the ESEARCH spelling.
func addPhantoms(extra ...uint32) func(string) string {
	return func(line string) string {
		body, eol, found := strings.Cut(line, "\r\n")
		if !found {
			return line
		}
		switch {
		case strings.HasPrefix(body, "* SEARCH"):
			var b strings.Builder
			b.WriteString(body)
			for _, u := range extra {
				fmt.Fprintf(&b, " %d", u)
			}
			return b.String() + "\r\n" + eol
		case strings.HasPrefix(body, "* ESEARCH"):
			var b strings.Builder
			b.WriteString(body)
			for _, u := range extra {
				fmt.Fprintf(&b, ",%d", u)
			}
			return b.String() + "\r\n" + eol
		}
		return line
	}
}

// dropFirstUID removes the first UID from a SEARCH response, so the server
// under-reports rather than over-reports.
func dropFirstUID(line string) string {
	body, eol, found := strings.Cut(line, "\r\n")
	if !found {
		return line
	}
	if strings.HasPrefix(body, "* SEARCH") {
		fields := strings.Fields(body)
		if len(fields) > 2 {
			return strings.Join(append(fields[:2:2], fields[3:]...), " ") + "\r\n" + eol
		}
	}
	if strings.HasPrefix(body, "* ESEARCH") {
		i := strings.LastIndex(body, " ")
		if i < 0 {
			return line
		}
		set := body[i+1:]
		if _, rest, ok := strings.Cut(set, ","); ok {
			return body[:i+1] + rest + "\r\n" + eol
		}
	}
	return line
}

func seedMailbox(t *testing.T, conn imapx.Conn, n int) []uint32 {
	t.Helper()

	uids := make([]uint32, 0, n)
	for i := range n {
		uids = append(uids, appendTo(t, conn, fmt.Sprintf("seed-%d", i)))
	}
	return uids
}

// TestAllUIDsRejectsASearchThatOverCountsTheMailbox is the regression test for
// the iCloud stale-index bug. Believing SEARCH there put 89,830 messages that
// do not exist onto INBOX's copy list.
func TestAllUIDsRejectsASearchThatOverCountsTheMailbox(t *testing.T) {
	addr, _ := startMemServer(t, rev2Caps())
	want := seedMailbox(t, dialMem(t, addr), 3)

	proxy := lyingProxy(t, addr, addPhantoms(900, 901, 902))
	conn := dialMem(t, proxy)

	ctx := context.Background()
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("SelectFolder() error = %v", err)
	}

	got, err := conn.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("AllUIDs() returned %d UIDs (%v), want %d (%v); the phantoms were believed",
			len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllUIDs()[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

// TestAllUIDsRejectsASearchThatUnderCountsTheMailbox covers the other
// direction. A search that hides a message would silently skip copying it,
// which is worse than the over-count and would not be noticed.
func TestAllUIDsRejectsASearchThatUnderCountsTheMailbox(t *testing.T) {
	addr, _ := startMemServer(t, rev2Caps())
	want := seedMailbox(t, dialMem(t, addr), 3)

	proxy := lyingProxy(t, addr, dropFirstUID)
	conn := dialMem(t, proxy)

	ctx := context.Background()
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("SelectFolder() error = %v", err)
	}

	got, err := conn.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("AllUIDs() returned %d UIDs (%v), want %d (%v); the truncation was believed",
			len(got), got, len(want), want)
	}
}

// TestAllUIDsBelievesASearchThatAgrees guards the fast path. The fallback costs
// one response per message, so a correct server must never pay for it.
func TestAllUIDsBelievesASearchThatAgrees(t *testing.T) {
	addr, _ := startMemServer(t, rev2Caps())
	want := seedMailbox(t, dialMem(t, addr), 3)

	var traffic traceBuf
	conn := dialMemWithTrace(t, addr, &traffic)

	ctx := context.Background()
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("SelectFolder() error = %v", err)
	}
	if _, err := conn.AllUIDs(ctx); err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}

	for _, line := range strings.Split(traffic.String(), "\n") {
		if strings.Contains(line, "FETCH 1:*") {
			t.Errorf("an agreeing server was made to pay for the fallback walk: %q", line)
		}
	}
	if len(want) != 3 {
		t.Fatalf("seeding produced %d UIDs, want 3", len(want))
	}
}

// TestAllUIDsSortsWhatTheFallbackWalkReturns.
//
// The walk is served in sequence order, which is UID order on any server
// following the spec — but this path only ever runs on a server already caught
// not following it, so the ascending order the caller is promised has to be
// imposed rather than assumed.
func TestAllUIDsSortsWhatTheFallbackWalkReturns(t *testing.T) {
	addr, _ := startMemServer(t, rev2Caps())
	seedMailbox(t, dialMem(t, addr), 3)

	// Phantoms force the fallback; the descending rewrite is what it walks.
	rewrite := addPhantoms(900, 901, 902)
	proxy := lyingProxy(t, addr, func(line string) string {
		return descendingUIDs(rewrite(line))
	})
	conn := dialMem(t, proxy)

	ctx := context.Background()
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	got, err := conn.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}

	want := []uint32{997, 998, 999}
	if len(got) != len(want) {
		t.Fatalf("AllUIDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllUIDs() = %v, want %v (ascending); the walk was returned unsorted", got, want)
		}
	}
}

// descendingUIDs rewrites UID n in a FETCH response to 1000-n, so a mailbox
// walked in sequence order yields UIDs in descending order.
var fetchUID = regexp.MustCompile(`^(\* \d+ FETCH \(UID )(\d+)(\))`)

func descendingUIDs(line string) string {
	m := fetchUID.FindStringSubmatch(line)
	if m == nil {
		return line
	}
	var n uint32
	if _, err := fmt.Sscanf(m[2], "%d", &n); err != nil {
		return line
	}
	return fmt.Sprintf("%s%d%s\r\n", m[1], 1000-n, m[3])
}

// TestAllUIDsOfAnEmptyMailboxIsNotAFallback: zero and zero agree, and an empty
// SEARCH response must not be mistaken for a disagreement.
func TestAllUIDsOfAnEmptyMailboxIsNotAFallback(t *testing.T) {
	addr, _ := startMemServer(t, rev2Caps())

	var traffic traceBuf
	conn := dialMemWithTrace(t, addr, &traffic)

	ctx := context.Background()
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("SelectFolder() error = %v", err)
	}
	got, err := conn.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("AllUIDs() = %v, want empty", got)
	}
	for _, line := range strings.Split(traffic.String(), "\n") {
		if strings.Contains(line, "FETCH 1:*") {
			t.Errorf("an empty mailbox was made to pay for the fallback walk: %q", line)
		}
	}
}
