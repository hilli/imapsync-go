package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/selection"
)

func TestParseBytes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"512", 512},
		{"1B", 1},
		{"64KiB", 64 << 10},
		{"256MiB", 256 << 20},
		{"2GiB", 2 << 30},
		{"100MB", 100e6},
		{"8M", 8 << 20},
		{" 32MiB ", 32 << 20},
	} {
		got, err := parseBytes(tc.in)
		if err != nil {
			t.Errorf("parseBytes(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	// A limit that is not a size, or is not a limit, has to be refused rather
	// than silently become something else. Zero would deadlock the run and a
	// negative number would panic the semaphore underneath it.
	for _, in := range []string{"", "lots", "0", "-1", "0MiB", "-4GiB", "1.5MiB", "MiB"} {
		if got, err := parseBytes(in); err == nil {
			t.Errorf("parseBytes(%q) = %d, want an error", in, got)
		}
	}
}

// TestReadingTheSourceDoesNotMarkMessagesSeen is a whole-command test of the
// one property a migration must never break: it must not alter the account it
// is reading.
//
// What actually protects this is BODY.PEEK — removing the peek fails this test.
// Opening the source with EXAMINE rather than SELECT is a second layer, and one
// this test cannot demonstrate, because with PEEK the two are indistinguishable
// from outside. It is asserted at the call in the engine's own tests instead.
//
// The message count is high enough to need several connections, so the pool
// does most of the selecting rather than the engine doing it once per folder.
func TestReadingTheSourceDoesNotMarkMessagesSeen(t *testing.T) {
	srcAddr, srcUser := startAccount(t, "Work")
	dstAddr, _ := startAccount(t)

	// Enough messages to need more than one connection, so the pool's own
	// selects are exercised and not just the engine's.
	for i := range 120 {
		mailbox := "INBOX"
		if i%2 == 0 {
			mailbox = "Work"
		}
		body := cliMessage(fmt.Sprintf("subject-%03d", i), fmt.Sprintf("m%d@example.test", i))
		if _, err := srcUser.Append(mailbox, bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
			t.Fatalf("seeding %q: %v", mailbox, err)
		}
	}

	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	out := runCLI(t, []string{
		"sync",
		"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
		"--source-password-env", "TEST_IMAP_PASSWORD",
		"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", filepath.Join(t.TempDir(), "state.db"),
		"--source-connections", "4",
		"--dest-connections", "6",
		"--log-level", "error",
	})
	if !strings.Contains(out, "120 copied") {
		t.Fatalf("did not copy everything:\n%s", out)
	}

	for _, mailbox := range []string{"INBOX", "Work"} {
		status, err := srcUser.Status(mailbox, &imap.StatusOptions{NumUnseen: true})
		if err != nil {
			t.Fatalf("reading %q status: %v", mailbox, err)
		}
		if status.NumUnseen == nil {
			t.Fatalf("server did not report unseen counts for %q", mailbox)
		}
		if *status.NumUnseen != 60 {
			t.Errorf("%s: only %d of 60 messages are still unread; reading the source marked them seen",
				mailbox, *status.NumUnseen)
		}
	}
}

// countingListener records how many connections a server accepts.
type countingListener struct {
	net.Listener
	n atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

// startCountedAccount is startAccount with a listener that counts connections.
func startCountedAccount(t *testing.T, mailboxes ...string) (addr string, user *imapmemserver.User, conns *countingListener) {
	t.Helper()

	user = imapmemserver.NewUser(cliUser, cliPassword)
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
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		Logger:       cliDiscardLogger{},
	})

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	conns = &countingListener{Listener: raw}
	go func() { _ = srv.Serve(conns) }()
	t.Cleanup(func() { _ = srv.Close() })

	return raw.Addr().String(), user, conns
}

// TestConnectionFlagsAreTheCeilingTheyClaimToBe checks the number that matters
// most to the person running this.
//
// iCloud counts connections against an account-wide ceiling and answers with
// authentication failures once it is passed — not to the connection that
// crossed the line, but to whatever the account is doing anywhere else, mail
// clients included. A flag that says four and opens more is worse than no flag
// at all, and one that says four and opens one turns a five-day migration back
// into a twenty-day one.
func TestConnectionFlagsAreTheCeilingTheyClaimToBe(t *testing.T) {
	const (
		srcCap   = 3
		dstCap   = 5
		messages = 400
	)

	srcAddr, srcUser, srcConns := startCountedAccount(t, "Work")
	dstAddr, _, dstConns := startCountedAccount(t)

	for i := range messages {
		mailbox := "INBOX"
		if i%2 == 0 {
			mailbox = "Work"
		}
		body := cliMessage(fmt.Sprintf("subject-%03d", i), fmt.Sprintf("m%d@example.test", i))
		if _, err := srcUser.Append(mailbox, bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
			t.Fatalf("seeding %q: %v", mailbox, err)
		}
	}

	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	out := runCLI(t, []string{
		"sync",
		"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
		"--source-password-env", "TEST_IMAP_PASSWORD",
		"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", filepath.Join(t.TempDir(), "state.db"),
		"--source-connections", fmt.Sprint(srcCap),
		"--dest-connections", fmt.Sprint(dstCap),
		"--log-level", "error",
	})
	if !strings.Contains(out, fmt.Sprintf("%d copied", messages)) {
		t.Fatalf("did not copy everything:\n%s", out)
	}

	for _, side := range []struct {
		name string
		got  int
		cap  int
	}{
		{"source", int(srcConns.n.Load()), srcCap},
		{"destination", int(dstConns.n.Load()), dstCap},
	} {
		if side.got > side.cap {
			t.Errorf("%s opened %d connections, more than the %d it was allowed", side.name, side.got, side.cap)
		}
		// There is plenty of work here for every connection. Opening one would
		// mean the flag never reached the pool.
		if side.got < 2 {
			t.Errorf("%s opened %d connections with %d messages to move; the limit is not being used",
				side.name, side.got, messages)
		}
	}
}

// TestConnectionCountsMustBePositive checks that the flags are validated rather
// than quietly turned into something else. A cap of zero would be a pool that
// can never hand out a connection: a run that hangs on its first folder.
func TestConnectionCountsMustBePositive(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	srcAddr, _ := startAccount(t)
	dstAddr, _ := startAccount(t)

	for _, flag := range []string{"--source-connections", "--dest-connections"} {
		out, err := runCLIErr(t, []string{
			"sync",
			"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
			"--source-password-env", "TEST_IMAP_PASSWORD",
			"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
			"--dest-password-env", "TEST_IMAP_PASSWORD",
			"--state", filepath.Join(t.TempDir(), "state.db"),
			flag, "0",
			"--log-level", "error",
		})
		if err == nil {
			t.Errorf("%s 0 was accepted:\n%s", flag, out)
		}
	}
}

// TestProgressCanBeSwitchedOff checks --progress-interval reaches the engine
// in both directions.
//
// Zero is the value a user reaches for to mean "be quiet", and it is also what
// an unset field looks like, so the flag has to tell those apart or asking for
// silence would hand back the default.
func TestProgressCanBeSwitchedOff(t *testing.T) {
	srcAddr, srcUser, _ := startCountedAccount(t)
	dstAddr, _, _ := startCountedAccount(t)

	for i := range 40 {
		body := cliMessage(fmt.Sprintf("subject-%03d", i), fmt.Sprintf("m%d@example.test", i))
		if _, err := srcUser.Append("INBOX", bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	run := func(interval string) string {
		return runCLILogs(t, []string{
			"sync",
			"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
			"--source-password-env", "TEST_IMAP_PASSWORD",
			"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
			"--dest-password-env", "TEST_IMAP_PASSWORD",
			"--state", filepath.Join(t.TempDir(), "state.db"),
			"--source-connections", "1",
			"--dest-connections", "1",
			"--progress-interval", interval,
			"--log-level", "info",
		})
	}

	if logs := run("1ms"); !strings.Contains(logs, "still going") {
		t.Fatalf("a run asked to report every millisecond said nothing:\n%s", logs)
	}
	if logs := run("0"); strings.Contains(logs, "still going") {
		t.Fatalf("--progress-interval=0 reported progress anyway:\n%s", logs)
	}
}

// runCLILogs runs the CLI and returns what it wrote to the log, which goes to
// stderr rather than to the command's own output.
func runCLILogs(t *testing.T, args []string) string {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "logs")
	if err != nil {
		t.Fatalf("log file: %v", err)
	}
	defer func() { _ = f.Close() }()

	saved := os.Stderr
	os.Stderr = f
	defer func() {
		os.Stderr = saved
		slog.SetDefault(slog.New(slog.DiscardHandler))
	}()

	runCLI(t, args)

	logs, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("reading logs: %v", err)
	}
	return string(logs)
}

// TestAnInterruptedRunStillSaysWhatItCopied.
//
// An interrupted run is not a failed run: it copied whatever it copied, that
// work is recorded, and the next run will not repeat it. Printing only the
// error throws away the one number worth having.
func TestAnInterruptedRunStillSaysWhatItCopied(t *testing.T) {
	srcAddr, srcUser, _ := startCountedAccount(t)
	dstAddr, _, _ := startCountedAccount(t)

	// Enough messages that no plausible machine finishes them inside the
	// deadline below, on one connection: the in-process server copies a few
	// thousand a second, so this is seconds of work cut off after a fraction
	// of one.
	for i := range 6000 {
		body := cliMessage(fmt.Sprintf("subject-%04d", i), fmt.Sprintf("m%d@example.test", i))
		if _, err := srcUser.Append("INBOX", bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"sync",
		"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
		"--source-password-env", "TEST_IMAP_PASSWORD",
		"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", filepath.Join(t.TempDir(), "state.db"),
		"--source-connections", "1",
		"--dest-connections", "1",
		"--progress-interval", "0",
		"--log-level", "error",
	})

	if err := cmd.ExecuteContext(ctx); err == nil {
		t.Fatalf("an interrupted run reported success:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "copied,") {
		t.Fatalf("an interrupted run printed no report:\n%s", out.String())
	}
}

// cliDial opens a connection to one of the in-process accounts, for the parts
// of a test that need to act on a mailbox rather than observe it.
func cliDial(t *testing.T, addr string) imapx.Conn {
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
		Addr:     config.Address{Host: host, Port: portNum, User: cliUser, TLS: config.TLSNone},
		Password: cliPassword,
	})
	if err != nil {
		t.Fatalf("dialling %q: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// unseenOn is the observable a person would use: how many messages a mailbox
// says are unread.
func unseenOn(t *testing.T, user *imapmemserver.User, mailbox string) uint32 {
	t.Helper()

	status, err := user.Status(mailbox, &imap.StatusOptions{NumUnseen: true})
	if err != nil {
		t.Fatalf("reading %q status: %v", mailbox, err)
	}
	if status.NumUnseen == nil {
		t.Fatalf("server did not report unseen counts for %q", mailbox)
	}
	return *status.NumUnseen
}

// TestFlagsFollowTheSourceUnlessToldNotTo.
//
// Two things are under test and only one of them is the engine. The other is
// that the flag reaches it: a --noresyncflags that the command accepts, prints
// in its help and then ignores is worse than no option at all, because it
// silently does the opposite of what it was asked.
func TestFlagsFollowTheSourceUnlessToldNotTo(t *testing.T) {
	srcAddr, srcUser, _ := startCountedAccount(t)
	dstAddr, dstUser, _ := startCountedAccount(t)

	for i := range 6 {
		body := cliMessage(fmt.Sprintf("subject-%03d", i), fmt.Sprintf("m%d@example.test", i))
		if _, err := srcUser.Append("INBOX", bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	statePath := filepath.Join(t.TempDir(), "state.db")
	run := func(extra ...string) string {
		t.Helper()
		return runCLI(t, append([]string{
			"sync",
			"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
			"--source-password-env", "TEST_IMAP_PASSWORD",
			"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
			"--dest-password-env", "TEST_IMAP_PASSWORD",
			"--state", statePath,
			"--log-level", "error",
		}, extra...))
	}

	if out := run(); !strings.Contains(out, "6 copied") {
		t.Fatalf("did not copy everything:\n%s", out)
	}
	if got := unseenOn(t, dstUser, "INBOX"); got != 6 {
		t.Fatalf("destination reports %d unread of 6 after the first copy", got)
	}

	// Somebody reads three messages on the source.
	ctx := context.Background()
	src := cliDial(t, srcAddr)
	if _, err := src.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("selecting source: %v", err)
	}
	uids, err := src.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("listing source UIDs: %v", err)
	}
	for _, uid := range uids[:3] {
		if err := src.StoreFlags(ctx, uid, []string{"\\Seen"}); err != nil {
			t.Fatalf("marking %d seen: %v", uid, err)
		}
	}

	run("--noresyncflags")
	if got := unseenOn(t, dstUser, "INBOX"); got != 6 {
		t.Errorf("destination reports %d unread of 6; --noresyncflags did not reach the engine", got)
	}

	run()
	if got := unseenOn(t, dstUser, "INBOX"); got != 3 {
		t.Errorf("destination reports %d unread, want 3; the flag resync did not run", got)
	}
}

func TestParseAgeReadsDaysAndDurations(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		// imapsync declares these options as floats, so half a day is legal.
		{"0.5d", 12 * time.Hour},
		{" 7d ", 7 * 24 * time.Hour},
		// Go's own syntax still works, for anyone who prefers it.
		{"720h", 720 * time.Hour},
		{"90m", 90 * time.Minute},
	} {
		got, err := parseAge(tc.in)
		if err != nil {
			t.Errorf("parseAge(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseAge(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// Zero and negative ages select nothing or everything depending on how you
	// read them, which is reason enough not to guess.
	for _, in := range []string{"", "30", "d", "0d", "-5d", "0s", "-1h", "tomorrow"} {
		if got, err := parseAge(in); err == nil {
			t.Errorf("parseAge(%q) = %v, want an error", in, got)
		}
	}
}

func TestTheMessageFilterIsBuiltFromTheFlagsThatDescribeIt(t *testing.T) {
	t.Parallel()

	f := syncFlags{maxSize: "10MiB", minSize: "1KiB", maxAge: "30d", minAge: "7d"}
	got, err := messageFilter(f)
	if err != nil {
		t.Fatalf("messageFilter() error = %v", err)
	}
	want := selection.Filter{
		MaxSize: 10 << 20, MinSize: 1 << 10,
		MaxAge: 30 * selection.Day, MinAge: 7 * selection.Day,
	}
	if got != want {
		t.Errorf("messageFilter() = %+v, want %+v", got, want)
	}

	if got, err := messageFilter(syncFlags{}); err != nil || got.Active() {
		t.Errorf("no flags gave %+v, %v; want an inactive filter and no error", got, err)
	}
}

// A bad value has to name the flag it came from. "invalid size" on a command
// line carrying both --max-size and --min-size says nothing useful.
func TestABadSelectionValueNamesItsFlag(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		flags syncFlags
		want  string
	}{
		{syncFlags{maxSize: "lots"}, "--max-size"},
		{syncFlags{minSize: "-1"}, "--min-size"},
		{syncFlags{maxAge: "soon"}, "--max-age"},
		{syncFlags{minAge: "0d"}, "--min-age"},
	} {
		_, err := messageFilter(tc.flags)
		if err == nil {
			t.Errorf("%+v was accepted", tc.flags)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error %q does not name %s", err, tc.want)
		}
	}

	// A window with no room in it is caught here rather than after a run that
	// copied nothing and reported success.
	if _, err := messageFilter(syncFlags{minSize: "10MiB", maxSize: "1MiB"}); err == nil {
		t.Error("a size window that can select nothing was accepted")
	}
}
