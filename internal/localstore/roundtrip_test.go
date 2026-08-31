package localstore_test

import (
	"bytes"
	"context"
	"fmt"
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
	"github.com/hilli/imapsync-go/internal/localstore"
)

// startMemServer runs go-imap's in-memory server, so the round trip is a real
// protocol conversation on both ends rather than a mock of our assumptions.
func startMemServer(t *testing.T) string {
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
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		Logger:       discardLogger{},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return ln.Addr().String()
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

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
		Credential: imapx.StaticPassword(testPassword),
	})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// held is one message as the destination must receive it.
type held struct {
	body  []byte
	flags []string
	date  time.Time
}

// drain copies every folder and message out of one connection, which is the
// part of a sync that reads. Doing it by hand here keeps the round trip a
// statement about the store rather than about the syncer.
//
// Folders are keyed by their path joined with "/", which is the delimiter both
// ends of this test use, so a key is a folder's shape rather than its name.
func drain(t *testing.T, c imapx.Conn) map[string][]held {
	t.Helper()
	ctx := t.Context()

	folders, err := c.ListFolders(ctx, imapx.ListOptions{})
	if err != nil {
		t.Fatalf("ListFolders() error = %v", err)
	}

	out := make(map[string][]held, len(folders))
	for _, f := range folders {
		// Name the folder by its path, not by whatever character the server
		// happens to separate with.
		key := strings.Join(strings.Split(f.Name, f.Delim), "/")
		if _, err := c.Select(ctx, f.Name, imapx.SelectOptions{}); err != nil {
			t.Fatalf("Select(%q) error = %v", f.Name, err)
		}
		uids, err := c.AllUIDs(ctx)
		if err != nil {
			t.Fatalf("AllUIDs(%q) error = %v", f.Name, err)
		}
		out[key] = nil
		if len(uids) == 0 {
			continue
		}
		metas, err := c.FetchMeta(ctx, uids, nil)
		if err != nil {
			t.Fatalf("FetchMeta(%q) error = %v", f.Name, err)
		}
		for _, m := range metas {
			var body bytes.Buffer
			if _, err := c.FetchBody(ctx, m.UID, &body); err != nil {
				t.Fatalf("FetchBody(%q, %d) error = %v", f.Name, m.UID, err)
			}
			flags := slices.Clone(m.Flags)
			slices.Sort(flags)
			out[key] = append(out[key], held{body: body.Bytes(), flags: flags, date: m.InternalDate.UTC()})
		}
		slices.SortFunc(out[key], func(a, b held) int { return bytes.Compare(a.body, b.body) })
	}
	return out
}

// fill writes a drained account into a connection, which is the part that
// writes.
func fill(t *testing.T, c imapx.Conn, want map[string][]held, delim string) {
	t.Helper()
	ctx := t.Context()

	for _, key := range slices.Sorted(maps(want)) {
		name := strings.Join(strings.Split(key, "/"), delim)
		if !strings.EqualFold(name, "INBOX") {
			if err := c.CreateFolder(ctx, name); err != nil {
				t.Fatalf("CreateFolder(%q) error = %v", name, err)
			}
		}
		for _, m := range want[key] {
			if _, err := c.Append(ctx, name, imapx.AppendMessage{
				Size:         int64(len(m.body)),
				Flags:        m.flags,
				InternalDate: m.date,
				Body:         bytes.NewReader(m.body),
			}); err != nil {
				t.Fatalf("Append(%q) error = %v", name, err)
			}
		}
	}
}

func maps(m map[string][]held) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// TestAnAccountSurvivesTheRoundTripThroughDisk is what the store is for: mail
// goes out to a directory and comes back to a different server with its bytes,
// flags, dates and folder tree intact. Backing up is only half a backup.
func TestAnAccountSurvivesTheRoundTripThroughDisk(t *testing.T) {
	t.Parallel()

	source := dialMem(t, startMemServer(t))
	folders := map[string][]held{
		"INBOX": {
			{body: testMessage("First", "1@example.test"), flags: []string{"\\Seen"}, date: time.Date(2011, 3, 14, 9, 26, 53, 0, time.UTC)},
			{body: testMessage("Second", "2@example.test"), flags: nil, date: time.Date(2019, 7, 1, 6, 0, 0, 0, time.UTC)},
		},
		"Arkiv":        {{body: testMessage("Gammel", "3@example.test"), flags: []string{"\\Answered", "\\Flagged"}, date: time.Date(2004, 1, 2, 3, 4, 5, 0, time.UTC)}},
		"Sendt/2024":   {{body: testMessage("Sendt", "4@example.test"), flags: []string{"\\Seen"}, date: time.Date(2024, 12, 24, 18, 30, 0, 0, time.UTC)}},
		"Slettet post": nil,
	}
	fill(t, source, folders, "/")
	want := drain(t, source)

	// Out to disk.
	root := t.TempDir()
	store := openStore(t, root)
	local := connect(t, store)
	fill(t, local, want, localstore.Delim)

	// Close the store, as the end of a run does, and open it again: what is
	// restored must come off the disk rather than out of a live process.
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened := openStore(t, root)
	back := drain(t, connect(t, reopened))

	// And back into a second, empty server.
	dest := dialMem(t, startMemServer(t))
	fill(t, dest, back, "/")
	got := drain(t, dest)

	if len(got) != len(want) {
		t.Fatalf("restored %d folders, want %d:\ngot  %v\nwant %v", len(got), len(want), keys(got), keys(want))
	}
	for name, wantMsgs := range want {
		gotMsgs, ok := got[name]
		if !ok {
			t.Errorf("folder %q did not survive; got %v", name, keys(got))
			continue
		}
		if len(gotMsgs) != len(wantMsgs) {
			t.Errorf("folder %q came back with %d messages, want %d", name, len(gotMsgs), len(wantMsgs))
			continue
		}
		for i := range wantMsgs {
			if !bytes.Equal(gotMsgs[i].body, wantMsgs[i].body) {
				t.Errorf("folder %q message %d changed:\ngot  %q\nwant %q", name, i, gotMsgs[i].body, wantMsgs[i].body)
			}
			if !slices.Equal(gotMsgs[i].flags, wantMsgs[i].flags) {
				t.Errorf("folder %q message %d flags = %v, want %v", name, i, gotMsgs[i].flags, wantMsgs[i].flags)
			}
			if !gotMsgs[i].date.Equal(wantMsgs[i].date) {
				t.Errorf("folder %q message %d INTERNALDATE = %v, want %v", name, i, gotMsgs[i].date, wantMsgs[i].date)
			}
		}
	}
}

func keys(m map[string][]held) []string { return slices.Sorted(maps(m)) }
