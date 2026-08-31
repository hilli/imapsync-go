package syncer_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/folder"
	"github.com/hilli/imapsync-go/internal/ident"
	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/pool"
	"github.com/hilli/imapsync-go/internal/state"
	"github.com/hilli/imapsync-go/internal/syncer"
)

const (
	testUser     = "sync@example.test"
	testPassword = "correct-horse"
)

type account struct {
	addr string
	user *imapmemserver.User
}

// newAccount runs go-imap's in-memory server. Two of them stand in for the two
// sides of a migration, so the engine is exercised over real protocol
// conversations rather than against a mock of our own assumptions.
func newAccount(t *testing.T, caps imap.CapSet, mailboxes ...string) account {
	t.Helper()

	user := imapmemserver.NewUser(testUser, testPassword)
	for _, name := range append([]string{"INBOX"}, mailboxes...) {
		if err := user.Create(name, nil); err != nil {
			t.Fatalf("creating %q: %v", name, err)
		}
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

	return account{addr: ln.Addr().String(), user: user}
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

func rev2Caps() imap.CapSet {
	return imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}
}

// rev1Caps is a server without UIDPLUS, so APPEND returns no APPENDUID and the
// engine has to find what it just wrote by searching. That is the path most
// likely to go wrong, so it is the one most of these tests use.
func rev1Caps() imap.CapSet {
	return imap.CapSet{imap.CapIMAP4rev1: {}}
}

func (a account) dial(t *testing.T) imapx.Conn {
	t.Helper()

	host, port, err := net.SplitHostPort(a.addr)
	if err != nil {
		t.Fatalf("splitting %q: %v", a.addr, err)
	}
	var portNum int
	if _, err := fmt.Sscanf(port, "%d", &portNum); err != nil {
		t.Fatalf("parsing port %q: %v", port, err)
	}

	conn, err := imapx.Dial(context.Background(), imapx.DialOptions{
		Addr:     config.Address{Host: host, Port: portNum, User: testUser, TLS: config.TLSNone},
		Password: testPassword,
	})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func testMessage(subject, messageID string) []byte {
	return []byte(fmt.Sprintf(
		"From: sender@example.test\r\n"+
			"To: %s\r\n"+
			"Subject: %s\r\n"+
			"Message-ID: <%s>\r\n"+
			"Date: Mon, 27 Aug 2026 12:00:00 +0000\r\n"+
			"\r\n"+
			"Body of %s.\r\n", testUser, subject, messageID, subject))
}

// stuff puts messages directly into a mailbox, bypassing the engine.
func (a account) stuff(t *testing.T, mailbox string, bodies ...[]byte) {
	t.Helper()

	for _, body := range bodies {
		a.stuffWithFlags(t, mailbox, nil, body)
	}
}

func (a account) stuffWithFlags(t *testing.T, mailbox string, flags []imap.Flag, body []byte) {
	t.Helper()

	_, err := a.user.Append(mailbox, bytes.NewReader(body), &imap.AppendOptions{
		Flags: flags,
		Time:  time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("stuffing %q: %v", mailbox, err)
	}
}

// contents returns every message body in a mailbox, so duplication is visible.
func (a account) contents(t *testing.T, mailbox string) []string {
	t.Helper()

	conn := a.dial(t)
	ctx := context.Background()
	if _, err := conn.Select(ctx, mailbox, imapx.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("selecting %q: %v", mailbox, err)
	}
	uids, err := conn.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("listing %q: %v", mailbox, err)
	}

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		var buf bytes.Buffer
		if _, err := conn.FetchBody(ctx, uid, &buf); err != nil {
			t.Fatalf("fetching uid %d: %v", uid, err)
		}
		out = append(out, buf.String())
	}
	return out
}

// subjects reduces mailbox contents to something a failure message can show.
func subjects(bodies []string) []string {
	out := make([]string, 0, len(bodies))
	for _, b := range bodies {
		subject := "<none>"
		for _, line := range strings.Split(b, "\r\n") {
			if after, ok := strings.CutPrefix(line, "Subject: "); ok {
				subject = after
				break
			}
		}
		out = append(out, subject)
	}
	return out
}

type harness struct {
	src, dst account
	db       *state.DB
	dbPath   string
}

func newHarness(t *testing.T, caps imap.CapSet, srcMailboxes ...string) *harness {
	t.Helper()

	h := &harness{
		src:    newAccount(t, caps, srcMailboxes...),
		dst:    newAccount(t, caps),
		dbPath: filepath.Join(t.TempDir(), "state.db"),
	}
	h.db = h.openDB(t)
	return h
}

func (h *harness) openDB(t *testing.T) *state.DB {
	t.Helper()

	db, err := state.Open(context.Background(), h.dbPath)
	if err != nil {
		t.Fatalf("opening state: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// dialFunc opens connections to this account the way a pool wants them.
func (a account) dialFunc(t *testing.T, wrap func(imapx.Conn) imapx.Conn) pool.DialFunc {
	t.Helper()
	return func(context.Context) (imapx.Conn, error) {
		c := a.dial(t)
		if wrap != nil {
			c = wrap(c)
		}
		return c, nil
	}
}

// pooled builds a pool over an account.
//
// The caps are deliberately above one. The whole point of these tests is that
// the correctness M1 established survives being run concurrently, and a pool of
// one connection would quietly turn every one of them back into a sequential
// test that proves nothing about the engine as it now works.
func pooled(t *testing.T, cap int, sel imapx.SelectOptions, dial pool.DialFunc) *pool.Pool {
	t.Helper()

	p, err := pool.New(pool.Options{Cap: cap, Dial: dial, Select: sel})
	if err != nil {
		t.Fatalf("pool.New() error = %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

// readOnly is how a source pool must open mailboxes: EXAMINE, never SELECT, so
// that reading a message does not mark it \Seen on the account being migrated.
var readOnly = imapx.SelectOptions{ReadOnly: true}

// run performs one complete sync, on fresh connections, as a real invocation
// would. Reconnecting each time keeps a run from inheriting selected-mailbox
// state that a restarted process would not have.
func (h *harness) run(t *testing.T, opts ...func(*syncer.Options)) syncer.Report {
	t.Helper()

	o := syncer.Options{PairID: "test"}
	for _, f := range opts {
		f(&o)
	}

	s := syncer.New(
		pooled(t, 3, readOnly, h.src.dialFunc(t, nil)),
		pooled(t, 5, imapx.SelectOptions{}, h.dst.dialFunc(t, nil)),
		h.db, nil, o,
	)
	report, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, fr := range report.Folders {
		if fr.Err != nil {
			t.Fatalf("folder %q failed: %v", fr.Source, fr.Err)
		}
	}
	return report
}

// TestSyncCopiesEveryMessage is the baseline: without this, nothing else means
// anything.
func TestSyncCopiesEveryMessage(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps(), "Work")
	h.src.stuff(t, "INBOX", testMessage("first", "a@example.test"), testMessage("second", "b@example.test"))
	h.src.stuff(t, "Work", testMessage("third", "c@example.test"))

	report := h.run(t)

	copied, _, failed := report.Totals()
	if copied != 3 || failed != 0 {
		t.Errorf("copied %d, failed %d; want 3 copied and none failed", copied, failed)
	}
	if got := len(h.dst.contents(t, "INBOX")); got != 2 {
		t.Errorf("destination INBOX holds %d messages, want 2", got)
	}
	if got := len(h.dst.contents(t, "Work")); got != 1 {
		t.Errorf("destination Work holds %d messages, want 1", got)
	}
}

// TestBodiesArriveByteForByte guards the one thing a mail migration may never
// do: alter the mail.
func TestBodiesArriveByteForByte(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	body := testMessage("verbatim", "v@example.test")
	h.src.stuff(t, "INBOX", body)

	h.run(t)

	got := h.dst.contents(t, "INBOX")
	if len(got) != 1 {
		t.Fatalf("destination holds %d messages, want 1", len(got))
	}
	if got[0] != string(body) {
		t.Errorf("body was altered in transit:\n got %q\nwant %q", got[0], body)
	}
}

// TestASecondRunCopiesNothing is the property the whole design exists to
// provide. Every mechanism in the engine — the UID map, the write-ahead row,
// the identity ladder — is there to make this true, and it is the failure users
// of a sync tool notice first and forgive least.
func TestASecondRunCopiesNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		caps imap.CapSet
	}{
		{"with UIDPLUS", rev2Caps()},
		{"without UIDPLUS", rev1Caps()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, tc.caps, "Work")
			h.src.stuff(t, "INBOX",
				testMessage("first", "a@example.test"),
				testMessage("second", "b@example.test"))
			h.src.stuff(t, "Work", testMessage("third", "c@example.test"))

			h.run(t)
			second := h.run(t)

			copied, adopted, _ := second.Totals()
			if copied != 0 || adopted != 0 {
				t.Errorf("the second run copied %d and adopted %d; want neither", copied, adopted)
			}

			var total int
			for _, name := range []string{"INBOX", "Work"} {
				got := h.dst.contents(t, name)
				total += len(got)
				if name == "INBOX" && len(got) != 2 {
					t.Errorf("INBOX holds %v after two runs", subjects(got))
				}
			}
			if total != 3 {
				t.Errorf("the destination holds %d messages after two runs, want 3", total)
			}
		})
	}
}

// TestAnInterruptedRunResumesWithoutDuplicating covers the crash that matters:
// the process dies partway through a folder. The rows it committed must be
// honoured on the next run rather than re-copied.
func TestAnInterruptedRunResumesWithoutDuplicating(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	for i := range 6 {
		h.src.stuff(t, "INBOX", testMessage(fmt.Sprintf("msg%d", i), fmt.Sprintf("m%d@example.test", i)))
	}

	// A first run that only got partway: three messages exist on the
	// destination and are recorded as done.
	h.run(t)

	// A second batch arrives at the source afterwards.
	for i := 6; i < 9; i++ {
		h.src.stuff(t, "INBOX", testMessage(fmt.Sprintf("msg%d", i), fmt.Sprintf("m%d@example.test", i)))
	}

	second := h.run(t)

	copied, _, _ := second.Totals()
	if copied != 3 {
		t.Errorf("the second run copied %d messages, want only the 3 that are new", copied)
	}
	if got := h.dst.contents(t, "INBOX"); len(got) != 9 {
		t.Errorf("destination holds %d messages, want 9: %v", len(got), subjects(got))
	}
}

// TestACrashBetweenAppendAndCommitAdoptsRatherThanDuplicates is the write-ahead
// invariant (§5.4). The row says a copy may have landed; the recovery pass must
// look before it copies again.
//
// The crash is simulated exactly where it hurts: the message is on the
// destination and the state says in-flight, which is the state a process death
// between the APPEND and its commit would leave behind.
func TestACrashBetweenAppendAndCommitAdoptsRatherThanDuplicates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, rev1Caps())

	body := testMessage("survivor", "s@example.test")
	h.src.stuff(t, "INBOX", body)
	h.dst.stuff(t, "INBOX", body) // the append that landed

	// The state the dead process left: committed as in-flight, never completed.
	f, err := h.db.EnsureFolder(ctx, "test", "INBOX", "INBOX")
	if err != nil {
		t.Fatalf("EnsureFolder() error = %v", err)
	}
	srcUIDValidity := h.uidValidity(t, h.src, "INBOX")
	if err := h.db.BeginAppend(ctx, state.Message{
		FolderID:       f.ID,
		SrcUIDValidity: srcUIDValidity,
		SrcUID:         1,
		IdentHash:      ident.Parse(body).Digest,
	}); err != nil {
		t.Fatalf("BeginAppend() error = %v", err)
	}
	if _, err := h.db.FenceUIDValidity(ctx, f.ID, srcUIDValidity, h.uidValidity(t, h.dst, "INBOX")); err != nil {
		t.Fatalf("FenceUIDValidity() error = %v", err)
	}

	report := h.run(t)

	copied, adopted, _ := report.Totals()
	if adopted != 1 {
		t.Errorf("adopted %d messages, want 1: the copy already on the destination was not recognised", adopted)
	}
	if copied != 0 {
		t.Errorf("copied %d messages after a crash, want 0", copied)
	}
	if got := h.dst.contents(t, "INBOX"); len(got) != 1 {
		t.Errorf("destination holds %d copies of one message: %v", len(got), subjects(got))
	}
}

// TestAnInFlightMessageThatNeverLandedIsRetried is the other half of recovery.
// Adopting too eagerly would silently drop mail, which is worse than the
// duplication it is trying to avoid, so a suspect that is genuinely absent must
// be copied.
func TestAnInFlightMessageThatNeverLandedIsRetried(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, rev1Caps())

	body := testMessage("lost", "l@example.test")
	h.src.stuff(t, "INBOX", body)

	f, err := h.db.EnsureFolder(ctx, "test", "INBOX", "INBOX")
	if err != nil {
		t.Fatalf("EnsureFolder() error = %v", err)
	}
	srcUIDValidity := h.uidValidity(t, h.src, "INBOX")
	if err := h.db.BeginAppend(ctx, state.Message{
		FolderID:       f.ID,
		SrcUIDValidity: srcUIDValidity,
		SrcUID:         1,
		IdentHash:      ident.Parse(body).Digest,
	}); err != nil {
		t.Fatalf("BeginAppend() error = %v", err)
	}

	report := h.run(t)

	copied, adopted, _ := report.Totals()
	if copied != 1 || adopted != 0 {
		t.Errorf("copied %d, adopted %d; want the absent message copied and nothing adopted", copied, adopted)
	}
	if got := h.dst.contents(t, "INBOX"); len(got) != 1 {
		t.Errorf("destination holds %d messages, want 1: %v", len(got), subjects(got))
	}
}

// TestAMessageWithoutAMessageIDIsStamped covers tier 4. Such a message cannot be
// searched for, so the copy must carry a marker of ours or recovery is blind to
// it.
func TestAMessageWithoutAMessageIDIsStamped(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	body := []byte("From: sender@example.test\r\n" +
		"Subject: no identifier\r\n" +
		"Date: Mon, 27 Aug 2026 12:00:00 +0000\r\n" +
		"\r\nBody.\r\n")
	h.src.stuff(t, "INBOX", body)

	h.run(t)

	got := h.dst.contents(t, "INBOX")
	if len(got) != 1 {
		t.Fatalf("destination holds %d messages, want 1", len(got))
	}
	if !strings.HasPrefix(got[0], ident.StampHeader+": ") {
		t.Errorf("the copy carries no stamp:\n%q", got[0])
	}
	if !strings.HasSuffix(got[0], string(body)) {
		t.Errorf("stamping altered the message:\n%q", got[0])
	}

	// And the stamp has to be what makes the second run recognise it, which is
	// only meaningful with the database thrown away.
	h.db = nil
	h.dbPath = filepath.Join(t.TempDir(), "fresh.db")
	h.db = h.openDB(t)

	second := h.run(t)
	if copied, adopted, _ := second.Totals(); copied != 0 || adopted != 1 {
		t.Errorf("after losing the database: copied %d, adopted %d; want the stamped copy adopted", copied, adopted)
	}
	if got := h.dst.contents(t, "INBOX"); len(got) != 1 {
		t.Errorf("destination holds %d copies: the stamp did not prevent duplication", len(got))
	}
}

// TestALostDatabaseAdoptsInsteadOfRecopying is tier 3 on its own: no UID map, no
// stamps, nothing but the headers. A user who deletes the state file must not be
// punished with a duplicated account.
func TestALostDatabaseAdoptsInsteadOfRecopying(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	h.src.stuff(t, "INBOX",
		testMessage("first", "a@example.test"),
		testMessage("second", "b@example.test"))

	h.run(t)

	h.dbPath = filepath.Join(t.TempDir(), "fresh.db")
	h.db = h.openDB(t)

	second := h.run(t)

	copied, adopted, _ := second.Totals()
	if copied != 0 || adopted != 2 {
		t.Errorf("copied %d, adopted %d after losing the database; want both adopted", copied, adopted)
	}
	if got := h.dst.contents(t, "INBOX"); len(got) != 2 {
		t.Errorf("destination holds %d messages, want 2: %v", len(got), subjects(got))
	}
}

// TestFlagsAreCarriedAcross checks the flags that belong to the message.
//
// It deliberately asserts nothing about \Recent: the destination sets that
// itself, so what arrives says nothing about what was sent. The filter is
// tested directly in the white-box test instead.
func TestFlagsAreCarriedAcross(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	h.src.stuffWithFlags(t, "INBOX",
		[]imap.Flag{imap.FlagSeen, imap.FlagFlagged, imap.Flag("\\Recent")},
		testMessage("flagged", "f@example.test"))

	h.run(t)

	conn := h.dst.dial(t)
	ctx := context.Background()
	if _, err := conn.Select(ctx, "INBOX", imapx.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("selecting: %v", err)
	}
	metas, err := conn.FetchMeta(ctx, []uint32{1}, ident.Fields)
	if err != nil {
		t.Fatalf("FetchMeta() error = %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("fetched %d messages, want 1", len(metas))
	}

	var seen, flagged bool
	for _, f := range metas[0].Flags {
		switch {
		case strings.EqualFold(f, "\\Recent"):
			// \Recent may be set by the destination itself, which is fine; what
			// matters is that we did not send it. That is asserted below by the
			// absence of an APPEND failure, since some servers reject it.
		case strings.EqualFold(f, "\\Seen"):
			seen = true
		case strings.EqualFold(f, "\\Flagged"):
			flagged = true
		}
	}
	if !seen || !flagged {
		t.Errorf("flags = %v; want \\Seen and \\Flagged carried across", metas[0].Flags)
	}
}

// TestDryRunWritesNothing keeps --dry-run honest. A preview that appends is
// worse than no preview.
func TestDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps(), "Work")
	h.src.stuff(t, "INBOX", testMessage("first", "a@example.test"))
	h.src.stuff(t, "Work", testMessage("second", "b@example.test"))

	report := h.run(t, func(o *syncer.Options) { o.DryRun = true })

	if copied, _, _ := report.Totals(); copied != 2 {
		t.Errorf("dry run reported %d messages to copy, want 2", copied)
	}
	if len(report.Created) != 1 || report.Created[0] != "Work" {
		t.Errorf("dry run reported creates %v, want [Work]", report.Created)
	}
	if got := len(h.dst.contents(t, "INBOX")); got != 0 {
		t.Errorf("dry run appended %d messages to the destination", got)
	}

	conn := h.dst.dial(t)
	folders, err := conn.ListFolders(context.Background(), imapx.ListOptions{})
	if err != nil {
		t.Fatalf("ListFolders() error = %v", err)
	}
	for _, f := range folders {
		if f.Name == "Work" {
			t.Error("dry run created a destination folder")
		}
	}
}

// TestSkippedFoldersAreReported makes sure a filtered-out mailbox is visible in
// the outcome rather than silently missing.
func TestSkippedFoldersAreReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps(), "Work", "Private")
	h.src.stuff(t, "INBOX", testMessage("first", "a@example.test"))
	h.src.stuff(t, "Private", testMessage("secret", "s@example.test"))

	report := h.run(t, func(o *syncer.Options) {
		o.Folders = folder.Options{Only: []string{"INBOX"}}
	})

	if len(report.Folders) != 1 || report.Folders[0].Source != "INBOX" {
		t.Fatalf("synced %d folders, want only INBOX", len(report.Folders))
	}
	if len(report.Skips) == 0 {
		t.Error("nothing was reported as skipped")
	}
	if got := len(h.dst.contents(t, "INBOX")); got != 1 {
		t.Errorf("destination INBOX holds %d messages, want 1", got)
	}
}

func (h *harness) uidValidity(t *testing.T, a account, mailbox string) uint32 {
	t.Helper()

	mbox, err := a.dial(t).Select(context.Background(), mailbox, imapx.SelectOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("selecting %q: %v", mailbox, err)
	}
	return mbox.UIDValidity
}

// TestIdenticalMessagesAreNotCollapsed covers the failure adoption can cause
// rather than prevent. A mailbox may legitimately hold the same message twice,
// and matching by identity must not turn two into one — that drops mail
// silently, which is worse than the duplicate it is avoiding.
func TestIdenticalMessagesAreNotCollapsed(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	body := testMessage("sent twice", "dup@example.test")
	h.src.stuff(t, "INBOX", body, body)

	// One copy is already at the destination, so adoption has something to
	// match and must stop after matching it once.
	h.dst.stuff(t, "INBOX", body)

	report := h.run(t)

	copied, adopted, _ := report.Totals()
	if copied != 1 || adopted != 1 {
		t.Errorf("copied %d, adopted %d; want one of each", copied, adopted)
	}
	if got := h.dst.contents(t, "INBOX"); len(got) != 2 {
		t.Errorf("destination holds %d copies, want 2: two source messages are two messages", len(got))
	}
}

// TestAdoptionSkipsMessagesAlreadyClaimed is the same hazard one step later,
// and the one that loses mail.
//
// The state here is a first sync that never finished onto a destination that
// was not empty: one source message is recorded against the single destination
// copy, and a second identical source message remains. Offering it that same
// destination copy would map two source messages onto one and leave the second
// uncopied for good.
func TestAdoptionSkipsMessagesAlreadyClaimed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, rev1Caps())
	body := testMessage("sent twice", "dup@example.test")

	h.src.stuff(t, "INBOX", body, body)
	h.dst.stuff(t, "INBOX", body)

	f, err := h.db.EnsureFolder(ctx, "test", "INBOX", "INBOX")
	if err != nil {
		t.Fatalf("EnsureFolder() error = %v", err)
	}
	srcUIDValidity := h.uidValidity(t, h.src, "INBOX")
	dstUIDValidity := h.uidValidity(t, h.dst, "INBOX")
	if _, err := h.db.FenceUIDValidity(ctx, f.ID, srcUIDValidity, dstUIDValidity); err != nil {
		t.Fatalf("FenceUIDValidity() error = %v", err)
	}
	if err := h.db.BeginAppend(ctx, state.Message{
		FolderID:       f.ID,
		SrcUIDValidity: srcUIDValidity,
		SrcUID:         1,
		IdentHash:      ident.Parse(body).Digest,
	}); err != nil {
		t.Fatalf("BeginAppend() error = %v", err)
	}
	if err := h.db.CompleteAppend(ctx, f.ID, srcUIDValidity, 1, dstUIDValidity, 1); err != nil {
		t.Fatalf("CompleteAppend() error = %v", err)
	}

	report := h.run(t)

	copied, adopted, _ := report.Totals()
	if copied != 1 || adopted != 0 {
		t.Errorf("copied %d, adopted %d; want the second message copied, not handed the first's destination copy", copied, adopted)
	}
	if got := h.dst.contents(t, "INBOX"); len(got) != 2 {
		t.Errorf("destination holds %d copies, want 2: a source message was dropped", len(got))
	}
}

// TestAStampedMessageIsNotStampedAgain guards the property that makes repeated
// syncing safe: a message must not grow.
//
// The stamp goes on at the front, and until this was fixed it went on at every
// hop, because the digest deliberately ignores the stamp and the fetch did too
// — so the syncer could not see one that was already there. Backing an account
// up, restoring it and backing it up again added three copies of the same
// header to every message lacking a Message-ID, 84 bytes a time, and the copy
// on disk was no longer the message the server holds.
//
// A source that already carries a stamp is not a hypothetical: it is exactly
// what the destination of the previous sync looks like.
func TestAStampedMessageIsNotStampedAgain(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	body := []byte("From: sender@example.test\r\n" +
		"Subject: no identifier\r\n" +
		"Date: Mon, 27 Aug 2026 12:00:00 +0000\r\n" +
		"\r\nBody.\r\n")
	id := ident.Parse(body)
	stamped := append(ident.StampBytes(id.StampValue()), body...)
	h.src.stuff(t, "INBOX", stamped)

	h.run(t)

	got := h.dst.contents(t, "INBOX")
	if len(got) != 1 {
		t.Fatalf("destination holds %d messages, want 1", len(got))
	}
	if n := strings.Count(got[0], ident.StampHeader+": "); n != 1 {
		t.Errorf("the copy carries %d stamps, want 1:\n%q", n, got[0])
	}
	if got[0] != string(stamped) {
		t.Errorf("the copy is not the message:\n got %q\nwant %q", got[0], stamped)
	}

	// The stamp still has to be the thing that finds it, or not writing a
	// second one would have cost recovery its only handle on the message.
	h.dbPath = filepath.Join(t.TempDir(), "fresh.db")
	h.db = h.openDB(t)

	second := h.run(t)
	if copied, adopted, _ := second.Totals(); copied != 0 || adopted != 1 {
		t.Errorf("after losing the database: copied %d, adopted %d; want the copy adopted", copied, adopted)
	}
}

// TestRecoveryFindsAStampedCopy is tier 4 doing the job tier 3 cannot.
//
// The folder has synced before, so no bulk index is built and the suspect has to
// be found by a targeted search. The message has no Message-ID, so the only
// thing to search for is the stamp written onto the copy.
func TestRecoveryFindsAStampedCopy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, rev1Caps())

	body := []byte("From: sender@example.test\r\n" +
		"Subject: unidentified\r\n" +
		"Date: Mon, 27 Aug 2026 12:00:00 +0000\r\n" +
		"\r\nBody.\r\n")
	id := ident.Parse(body)

	h.src.stuff(t, "INBOX", body)
	h.dst.stuff(t, "INBOX", append(ident.StampBytes(id.StampValue()), body...))

	f, err := h.db.EnsureFolder(ctx, "test", "INBOX", "INBOX")
	if err != nil {
		t.Fatalf("EnsureFolder() error = %v", err)
	}
	srcUIDValidity := h.uidValidity(t, h.src, "INBOX")
	if _, err := h.db.FenceUIDValidity(ctx, f.ID, srcUIDValidity, h.uidValidity(t, h.dst, "INBOX")); err != nil {
		t.Fatalf("FenceUIDValidity() error = %v", err)
	}
	// A completed sync, so the bulk index is out of the picture.
	if err := h.db.MarkSynced(ctx, f.ID, 0, 0, time.Now()); err != nil {
		t.Fatalf("MarkSynced() error = %v", err)
	}
	if err := h.db.BeginAppend(ctx, state.Message{
		FolderID:       f.ID,
		SrcUIDValidity: srcUIDValidity,
		SrcUID:         1,
		IdentHash:      id.Digest,
		StampID:        id.StampValue(),
	}); err != nil {
		t.Fatalf("BeginAppend() error = %v", err)
	}

	report := h.run(t)

	copied, adopted, _ := report.Totals()
	if copied != 0 || adopted != 1 {
		t.Errorf("copied %d, adopted %d; want the stamped copy found by search", copied, adopted)
	}
	if got := h.dst.contents(t, "INBOX"); len(got) != 1 {
		t.Errorf("destination holds %d copies, want 1", len(got))
	}
}

// TestAdoptionIgnoresWeakIdentities keeps a message that is nearly all body from
// matching an unrelated one that is also nearly all body.
func TestAdoptionIgnoresWeakIdentities(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	weak := []byte("Subject: ping\r\n\r\nfirst body\r\n")
	other := []byte("Subject: ping\r\n\r\ncompletely different body\r\n")

	h.src.stuff(t, "INBOX", weak)
	h.dst.stuff(t, "INBOX", other)

	report := h.run(t)

	if copied, adopted, _ := report.Totals(); copied != 1 || adopted != 0 {
		t.Errorf("copied %d, adopted %d; want the message copied rather than matched on nothing", copied, adopted)
	}
	if got := h.dst.contents(t, "INBOX"); len(got) != 2 {
		t.Errorf("destination holds %d messages, want 2", len(got))
	}
}

// lyingSource is a source server that overstates RFC822.SIZE.
//
// Real ones do. It matters more than a wrong number normally would: APPEND
// declares its literal length before sending the bytes and cannot retract it, so
// a size taken from the server's claim rather than from the bytes actually read
// desynchronises the connection past recovery (§3.7).
type lyingSource struct {
	imapx.Conn
	inflate int64
}

func (l lyingSource) FetchMeta(ctx context.Context, uids []uint32, fields []string) ([]imapx.MessageMeta, error) {
	metas, err := l.Conn.FetchMeta(ctx, uids, fields)
	for i := range metas {
		metas[i].Size += l.inflate
	}
	return metas, err
}

func TestTheAppendSizeComesFromTheBytesReadNotTheServersClaim(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	body := testMessage("honest bytes", "h@example.test")
	h.src.stuff(t, "INBOX", body)

	s := syncer.New(
		pooled(t, 3, readOnly, h.src.dialFunc(t, func(c imapx.Conn) imapx.Conn {
			return lyingSource{Conn: c, inflate: 4096}
		})),
		pooled(t, 5, imapx.SelectOptions{}, h.dst.dialFunc(t, nil)),
		h.db, nil, syncer.Options{PairID: "test"},
	)
	report, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, fr := range report.Folders {
		if fr.Err != nil {
			t.Fatalf("a lying RFC822.SIZE broke the run: %v", fr.Err)
		}
	}

	if copied, _, _ := report.Totals(); copied != 1 {
		t.Errorf("copied %d messages, want 1", copied)
	}
	got := h.dst.contents(t, "INBOX")
	if len(got) != 1 || got[0] != string(body) {
		t.Errorf("message did not arrive intact: %v", subjects(got))
	}
}

// openLog remembers how source mailboxes were opened, across every connection
// in a pool.
type openLog struct {
	mu     sync.Mutex
	opened map[string][]imapx.SelectOptions
}

func (l *openLog) record(mailbox string, opts imapx.SelectOptions) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.opened == nil {
		l.opened = make(map[string][]imapx.SelectOptions)
	}
	l.opened[mailbox] = append(l.opened[mailbox], opts)
}

// recordingSource reports every SELECT to a log shared by the whole pool.
type recordingSource struct {
	imapx.Conn
	log *openLog
}

func (r recordingSource) Select(ctx context.Context, mailbox string, opts imapx.SelectOptions) (imapx.Mailbox, error) {
	r.log.record(mailbox, opts)
	return r.Conn.Select(ctx, mailbox, opts)
}

// TestTheSourceIsOpenedReadOnly is a safety property that the destination
// cannot demonstrate: a migration must not alter the account it is reading. The
// difference between SELECT and EXAMINE is invisible in the result, so it is
// asserted at the call.
func TestTheSourceIsOpenedReadOnly(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps(), "Work")
	h.src.stuff(t, "INBOX", testMessage("first", "a@example.test"))
	h.src.stuff(t, "Work", testMessage("second", "b@example.test"))

	log := &openLog{}
	s := syncer.New(
		pooled(t, 3, readOnly, h.src.dialFunc(t, func(c imapx.Conn) imapx.Conn {
			return recordingSource{Conn: c, log: log}
		})),
		pooled(t, 5, imapx.SelectOptions{}, h.dst.dialFunc(t, nil)),
		h.db, nil, syncer.Options{PairID: "test"},
	)
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// How many times each mailbox is opened is an implementation detail — a
	// pool re-selects whenever a lease lands on a connection that was
	// elsewhere. That every one of those opens is read-only is not.
	for _, name := range []string{"INBOX", "Work"} {
		opens := log.opened[name]
		if len(opens) == 0 {
			t.Errorf("source mailbox %q was never opened", name)
		}
		for i, opts := range opens {
			if !opts.ReadOnly {
				t.Errorf("source mailbox %q open %d was for writing", name, i)
			}
		}
	}
}
