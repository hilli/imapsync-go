package main

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
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
		Addr:       config.Address{Host: host, Port: portNum, User: cliUser, TLS: config.TLSNone},
		Credential: imapx.StaticPassword(cliPassword),
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

// --age-basis picks which date --max-age and --min-age read. The default is the
// Date: header, matching imapsync, whose --noabletosearch selects the other one.
func TestTheAgeBasisDefaultsToTheSentDate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		basis string
		want  selection.Basis
	}{
		{"", selection.BasisSent},
		{"sent", selection.BasisSent},
		{"internal", selection.BasisInternal},
	} {
		got, err := messageFilter(syncFlags{maxAge: "30d", ageBasis: tc.basis})
		if err != nil {
			t.Fatalf("--age-basis %q: %v", tc.basis, err)
		}
		if got.Basis != tc.want {
			t.Errorf("--age-basis %q gave basis %v, want %v", tc.basis, got.Basis, tc.want)
		}
	}
}

// An unrecognised basis is refused rather than silently treated as the default,
// which would measure ages from a date the user did not ask for.
func TestAnUnknownAgeBasisIsRefused(t *testing.T) {
	t.Parallel()

	_, err := messageFilter(syncFlags{maxAge: "30d", ageBasis: "arrival"})
	if err == nil {
		t.Fatal("--age-basis arrival was accepted")
	}
	for _, want := range []string{"age-basis", "arrival", "sent", "internal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestABadSearchIsRefusedBeforeAnythingConnects.
//
// Parsing the search key here rather than at the first SELECT is the whole
// justification for parsing it at all: a key this tool cannot express is a
// typing mistake, and a typing mistake should cost a command line rather than
// a connection, a login and one error per folder in the middle of a run.
//
// The flag has to be named, because a run can carry two searches and "invalid
// search" says nothing about which one was wrong.
func TestABadSearchIsRefusedBeforeAnythingConnects(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ flag, key string }{
		{"source-search", "RECENT"},
		{"dest-search", "NOT"},
		{"source-search", "SINCE yesterday"},
		{"dest-search", "1:5"},
	} {
		_, err := searchFlag(tc.flag, tc.key)
		if err == nil {
			t.Errorf("--%s %q was accepted", tc.flag, tc.key)
			continue
		}
		if !strings.Contains(err.Error(), "--"+tc.flag) {
			t.Errorf("error %q does not name --%s", err, tc.flag)
		}
	}
}

// An absent search is the zero key, and the zero key means "no search" rather
// than "search for everything" — the difference between listing a folder and
// asking the server a question it was never given.
func TestAnAbsentSearchIsTheZeroKey(t *testing.T) {
	t.Parallel()

	got, err := searchFlag("source-search", "")
	if err != nil {
		t.Fatalf("an empty search was refused: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("an empty search parsed to %q, want the zero key", got)
	}

	got, err = searchFlag("source-search", "UNSEEN")
	if err != nil {
		t.Fatalf("UNSEEN was refused: %v", err)
	}
	if got.IsZero() {
		t.Error("UNSEEN parsed to the zero key, which would search for nothing")
	}
}

// TestConfigConcurrencyIsActuallyUsed.
//
// The `concurrency:` block was parsed, validated, defaulted — and then dropped
// on the floor by syncEndpoints, which returned four of the pair's five fields.
// Every run this tool has ever done used the flag defaults: a 776,791-message
// migration asked for "auto" on both sides and got 4 and 8, against servers
// measured at 48 and 30. The knob was not merely ineffective, it was
// misleading, because the report then printed the number nobody had chosen.
func TestConfigConcurrencyIsActuallyUsed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                string
		conc                config.Concurrency
		flags               syncFlags
		wantSrc, wantDst    int
		wantInflightInBytes int64
		wantBytesPerSecond  int64
		wantMessagesPerSec  float64
	}{{
		name: "config numbers are obeyed",
		conc: config.Concurrency{
			Source: 40, Dest: 24, MaxInflight: 512 << 20,
			MaxBytesPerSecond: 2 << 20, MaxMessagesPerSecond: 12,
		},
		flags:               syncFlags{srcConns: 4, dstConns: 8, memoryLimit: "256MiB"},
		wantSrc:             40,
		wantDst:             24,
		wantInflightInBytes: 512 << 20,
		wantBytesPerSecond:  2 << 20,
		wantMessagesPerSec:  12,
	}, {
		name:                "auto falls back to the built-in width",
		conc:                config.Concurrency{Source: 0, Dest: 0, MaxInflight: 512 << 20},
		flags:               syncFlags{srcConns: 4, dstConns: 8, memoryLimit: "256MiB"},
		wantSrc:             autoConnections,
		wantDst:             autoConnections,
		wantInflightInBytes: 512 << 20,
	}, {
		name: "a flag named on the command line beats the config",
		conc: config.Concurrency{
			Source: 40, Dest: 24, MaxInflight: 512 << 20,
			MaxBytesPerSecond: 2 << 20, MaxMessagesPerSecond: 12,
		},
		flags: syncFlags{
			srcConns: 3, srcConnsSet: true,
			dstConns:    8,
			memoryLimit: "32MiB", memoryLimitSet: true,
			maxBytesPerSecond: "512KiB", maxBytesPerSecondSet: true,
			maxMessagesPerSecond: 3, maxMessagesPerSecondSet: true,
		},
		wantSrc:             3,
		wantDst:             24,
		wantInflightInBytes: 32 << 20,
		wantBytesPerSecond:  512 << 10,
		wantMessagesPerSec:  3,
	}, {
		name:                "no config at all still works",
		conc:                config.Concurrency{},
		flags:               syncFlags{srcConns: 4, dstConns: 8, memoryLimit: "256MiB"},
		wantSrc:             autoConnections,
		wantDst:             autoConnections,
		wantInflightInBytes: 256 << 20,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveConcurrency(tc.flags, tc.conc)
			if err != nil {
				t.Fatalf("resolveConcurrency: %v", err)
			}
			if got.src != tc.wantSrc || got.dst != tc.wantDst {
				t.Errorf("resolved %d source and %d destination connections, want %d and %d",
					got.src, got.dst, tc.wantSrc, tc.wantDst)
			}
			if got.inflight != tc.wantInflightInBytes {
				t.Errorf("resolved a budget of %d bytes, want %d", got.inflight, tc.wantInflightInBytes)
			}

			rate := got.rate.Stats()
			if rate.BytesPerSec != tc.wantBytesPerSecond {
				t.Errorf("resolved a rate of %d bytes/second, want %d", rate.BytesPerSec, tc.wantBytesPerSecond)
			}
			if rate.MessagesPerSec != tc.wantMessagesPerSec {
				t.Errorf("resolved a rate of %v messages/second, want %v", rate.MessagesPerSec, tc.wantMessagesPerSec)
			}
		})
	}
}

// TestConfigConcurrencyReachesTheRunItself is the end-to-end half.
//
// resolveConcurrency being right is worth nothing if syncEndpoints goes back to
// discarding the block, which is the bug that actually happened. This drives a
// real config file through the CLI and counts the connections the servers see.
func TestConfigConcurrencyReachesTheRunItself(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)

	srcAddr, srcUser, srcConns := startCountedAccount(t, "Work")
	dstAddr, _, dstConns := startCountedAccount(t)
	for i := range 40 {
		body := cliMessage(fmt.Sprintf("subject-%03d", i), fmt.Sprintf("m%d@example.test", i))
		if _, err := srcUser.Append("Work", bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	const wantSrc, wantDst = 3, 5
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "imapsync.yaml")
	cfg := fmt.Sprintf(`pairs:
  - name: counted
    source:
      url: imap+insecure://%s@%s
      password: {env: TEST_IMAP_PASSWORD}
    dest:
      url: imap+insecure://%s@%s
      password: {env: TEST_IMAP_PASSWORD}
    concurrency:
      source: %d
      dest: %d
`, cliUser, srcAddr, cliUser, dstAddr, wantSrc, wantDst)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	out := runCLI(t, []string{
		"sync",
		"--config", cfgPath,
		"--state", filepath.Join(dir, "state.db"),
		"--log-level", "error",
	})

	for _, side := range []struct {
		name string
		got  int
		want int
	}{
		{"source", int(srcConns.n.Load()), wantSrc},
		{"destination", int(dstConns.n.Load()), wantDst},
	} {
		if side.got > side.want {
			t.Errorf("the %s server saw %d connections, more than the config's %d:\n%s",
				side.name, side.got, side.want, out)
		}
	}

	// The report must name what was asked for, not the flag default that used
	// to be printed regardless of the config.
	if !strings.Contains(out, fmt.Sprintf("held all %d connections", wantSrc)) {
		t.Errorf("the report does not name the configured source width:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("held all %d connections", wantDst)) {
		t.Errorf("the report does not name the configured destination width:\n%s", out)
	}
}

// TestConfigDelete2IsActuallyUsed.
//
// `delete2:` was the second field syncEndpoints dropped, and the one that
// destroys mail. It failed safe — a config asking for deletion got none — but
// safe is not the same as correct: somebody pruning a destination from a config
// file was told it was happening and it was not.
//
// Both directions are tested. The config must be able to turn deletion on, and
// `--delete2=false` must be able to turn it off again, which is why the flag is
// consulted for whether it was *given* rather than for its value.
func TestConfigDelete2IsActuallyUsed(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)

	for _, tc := range []struct {
		name       string
		configSays bool
		extraFlags []string
		wantGone   bool
	}{
		{name: "config turns deletion on", configSays: true, wantGone: true},
		{name: "config leaves deletion off", configSays: false, wantGone: false},
		{
			name:       "an explicit flag overrides a config that says yes",
			configSays: true,
			extraFlags: []string{"--delete2=false"},
			wantGone:   false,
		},
		{
			name:       "an explicit flag overrides a config that says no",
			configSays: false,
			extraFlags: []string{"--delete2"},
			wantGone:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srcAddr, srcUser := startAccount(t)
			dstAddr, dstUser := startAccount(t)

			// One message on both sides, and a second only on the
			// destination: a stranger, which is what --delete2 removes.
			body := cliMessage("keeper", "keep@example.test")
			for _, u := range []*imapmemserver.User{srcUser, dstUser} {
				if _, err := u.Append("INBOX", bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
					t.Fatalf("seeding: %v", err)
				}
			}
			stranger := cliMessage("stranger", "gone@example.test")
			if _, err := dstUser.Append("INBOX", bytes.NewReader(stranger), &imap.AppendOptions{}); err != nil {
				t.Fatalf("seeding the stranger: %v", err)
			}

			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "imapsync.yaml")
			cfg := fmt.Sprintf(`pairs:
  - name: deleter
    source:
      url: imap+insecure://%s@%s
      password: {env: TEST_IMAP_PASSWORD}
    dest:
      url: imap+insecure://%s@%s
      password: {env: TEST_IMAP_PASSWORD}
    delete2: %t
`, cliUser, srcAddr, cliUser, dstAddr, tc.configSays)
			if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
				t.Fatalf("writing config: %v", err)
			}

			args := append([]string{
				"sync",
				"--config", cfgPath,
				"--state", filepath.Join(dir, "state.db"),
				// The stranger is half the folder, well over the 10% ceiling
				// that would otherwise refuse the deletion on its own.
				"--force",
				"--log-level", "error",
			}, tc.extraFlags...)
			out := runCLI(t, args)

			// The server's own count, not the report's claim about it: a
			// deletion test that trusted the listing would be trusting the
			// very thing --delete2's ceiling exists to distrust.
			want := uint32(2)
			if tc.wantGone {
				want = 1
			}
			if got := countOn(t, dstUser, "INBOX"); got != want {
				t.Errorf("destination holds %d messages, want %d:\n%s", got, want, out)
			}
		})
	}
}

// TestEveryConcurrencyFieldIsConsumed is the test that would have caught both
// of the bugs above, and it is here because they were not one bug twice.
//
// `concurrency:` lost its widths and its memory limit, silently, for the life
// of the tool; `delete2:` was lost by the same function on the same day it was
// found. Each was fixed by remembering to read one more field, which is a fix
// that works exactly until the next field is added.
//
// So this asserts the shape rather than the values: every exported field of
// config.Concurrency must be named somewhere inside resolveConcurrency. It
// fails when a sixth field is added and nobody wires it up, which is the moment
// the knowledge is lost rather than the moment somebody notices their config
// did nothing.
//
// The check is scoped to that function's body, parsed rather than grepped. A
// test that asserts a name appears somewhere in a file is nearly worthless: the
// name would still be there if the line that used it moved into a comment, and
// this file mentions every one of these fields in its table tests already.
func TestEveryConcurrencyFieldIsConsumed(t *testing.T) {
	t.Parallel()

	const fn = "resolveConcurrency"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sync.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing sync.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == fn {
			body = fd.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("sync.go has no %s; this test is asserting something that no longer exists", fn)
	}

	read := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			read[sel.Sel.Name] = true
		}
		return true
	})

	fields := reflect.TypeOf(config.Concurrency{})
	for i := range fields.NumField() {
		name := fields.Field(i).Name
		if !read[name] {
			t.Errorf("%s never reads config.Concurrency.%s (yaml: %s).\n"+
				"A field parsed, validated and then dropped is worse than no field at all:\n"+
				"it is a setting people tune, measure against, and believe.",
				fn, name, fields.Field(i).Tag.Get("yaml"))
		}
	}
}

// TestConfigRateLimitReachesTheRunItself.
//
// Two assertions, and both are needed. The report would name the limit even if
// nothing ever charged it, because it reads the limiter that was built rather
// than the work that was done — so the elapsed time is what proves the copy
// path actually waits. And the elapsed time alone would not prove the number
// came from the config rather than from a flag default.
//
// The timing assertion is a lower bound, deliberately. rate.Limiter starts full,
// so n messages at r a second with a burst of r take at least (n-r)/r seconds;
// load can only make that longer. An upper bound here would be the flaky kind.
func TestConfigRateLimitReachesTheRunItself(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)

	const (
		messages  = 10
		perSecond = 6
	)
	// Ten messages against a bucket that starts holding six, refilling at six a
	// second.
	const atLeast = (messages - perSecond) * time.Second / perSecond

	srcAddr, srcUser := startAccount(t, "Work")
	dstAddr, _ := startAccount(t)
	for i := range messages {
		body := cliMessage(fmt.Sprintf("subject-%03d", i), fmt.Sprintf("m%d@example.test", i))
		if _, err := srcUser.Append("Work", bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "imapsync.yaml")
	cfg := fmt.Sprintf(`pairs:
  - name: throttled
    source:
      url: imap+insecure://%s@%s
      password: {env: TEST_IMAP_PASSWORD}
    dest:
      url: imap+insecure://%s@%s
      password: {env: TEST_IMAP_PASSWORD}
    concurrency:
      max_messages_per_second: %d
`, cliUser, srcAddr, cliUser, dstAddr, perSecond)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	started := time.Now()
	out := runCLI(t, []string{
		"sync",
		"--config", cfgPath,
		"--state", filepath.Join(dir, "state.db"),
		"--log-level", "error",
	})
	elapsed := time.Since(started)

	if elapsed < atLeast {
		t.Errorf("copied %d messages in %s under a configured limit of %d a second;\n"+
			"that is at least %s of work, so the config's rate limit is not reaching the copy path:\n%s",
			messages, elapsed, perSecond, atLeast, out)
	}
	if want := fmt.Sprintf("Rate limited to %d messages/second", perSecond); !strings.Contains(out, want) {
		t.Errorf("the report does not name the configured rate limit %q:\n%s", want, out)
	}
	if !strings.Contains(out, "Workers waited") {
		t.Errorf("the report does not say how long the run waited on its own brake:\n%s", out)
	}
}

// TestTheRateLimitIsSharedAcrossFolders.
//
// The limiter is held by the Syncer and folders run concurrently, so the
// allowance has to cover the whole run rather than each folder. Five folders of
// four messages each, against the same limit as the single-folder case above,
// must take the same time — a per-folder allowance would finish five times
// faster because each folder's twenty messages would fit inside its own burst.
func TestTheRateLimitIsSharedAcrossFolders(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)

	const (
		folders   = 5
		each      = 4
		perSecond = 6
	)
	const atLeast = (folders*each - perSecond) * time.Second / perSecond

	names := make([]string, folders)
	for i := range names {
		names[i] = fmt.Sprintf("Box%d", i)
	}
	srcAddr, srcUser := startAccount(t, names...)
	dstAddr, _ := startAccount(t)
	for _, name := range names {
		for i := range each {
			body := cliMessage(fmt.Sprintf("%s-%03d", name, i), fmt.Sprintf("%s-%d@example.test", name, i))
			if _, err := srcUser.Append(name, bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
				t.Fatalf("seeding %s: %v", name, err)
			}
		}
	}

	dir := t.TempDir()
	started := time.Now()
	out := runCLI(t, []string{
		"sync",
		"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
		"--source-password-env", "TEST_IMAP_PASSWORD",
		"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", filepath.Join(dir, "state.db"),
		"--max-messages-per-second", fmt.Sprint(perSecond),
		"--log-level", "error",
	})
	elapsed := time.Since(started)

	if elapsed < atLeast {
		t.Errorf("copied %d messages across %d folders in %s under a limit of %d a second;\n"+
			"that is at least %s of work, so each folder is being given its own allowance:\n%s",
			folders*each, folders, elapsed, perSecond, atLeast, out)
	}
}

// TestAnAdoptedMessageIsNotRateLimited.
//
// The charge sits after the adoption check, so a message already on the
// destination costs nothing. Without that, re-running a settled account — which
// is the fast, common case this tool is built around, 776,802 messages in 1m27s
// — would be throttled to the copy rate despite copying nothing.
func TestAnAdoptedMessageIsNotRateLimited(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)

	const messages = 20

	srcAddr, srcUser := startAccount(t, "Work")
	dstAddr, dstUser := startAccount(t, "Work")
	for i := range messages {
		body := cliMessage(fmt.Sprintf("subject-%03d", i), fmt.Sprintf("m%d@example.test", i))
		for _, u := range []*imapmemserver.User{srcUser, dstUser} {
			if _, err := u.Append("Work", bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
				t.Fatalf("seeding: %v", err)
			}
		}
	}

	dir := t.TempDir()
	out := runCLI(t, []string{
		"sync",
		"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
		"--source-password-env", "TEST_IMAP_PASSWORD",
		"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", filepath.Join(dir, "state.db"),
		"--max-messages-per-second", "1",
		"--log-level", "error",
	})

	if !strings.Contains(out, fmt.Sprintf("%d adopted", messages)) {
		t.Fatalf("the run did not adopt all %d messages, so this proves nothing:\n%s", messages, out)
	}
	// The assertion is on the report rather than on the clock, and it is exact:
	// nothing was charged, so nothing waited. A limit of one message a second
	// would otherwise have made this take nineteen seconds, but "it was quick"
	// is a weaker claim than "it never asked".
	if !strings.Contains(out, "Nothing ever waited on it") {
		t.Errorf("adopting %d messages charged the rate allowance for bytes that never crossed the wire:\n%s",
			messages, out)
	}
}

// paddedMessage is a message of exactly size bytes, or the smallest message
// larger than that if the headers alone already exceed it.
//
// The byte allowance can only be tested with messages whose size is known, and
// known exactly: the test below asserts the volume the report names, which is
// the assertion that would have caught a charge of the wrong number.
func paddedMessage(t *testing.T, subject, messageID string, size int) []byte {
	t.Helper()

	body := cliMessage(subject, messageID)
	if len(body) >= size {
		t.Fatalf("a %d-byte message cannot be padded down to %d", len(body), size)
	}
	// Two bytes of the shortfall are the closing CRLF of the filler line.
	return append(body[:len(body)-2], append(bytes.Repeat([]byte("x"), size-len(body)), "\r\n"...)...)
}

// TestTheByteAllowanceIsChargedTheMessagesSize.
//
// Written because a mutation survived. Both rate tests above use the message
// limit, so replacing meta.Size with 0 at the charge site changed nothing they
// could see: a zero-byte message still costs one message, and the run still
// took as long. Nothing asserted that the size reaching the byte allowance was
// the message's own.
//
// That is the same shape as the bug that lost concurrency: and delete2: for the
// life of the tool — a value parsed, validated, carried most of the way, and
// then quietly not used. The flag would have been accepted, the report would
// have named the limit, and the limit would have bounded nothing.
//
// The volume assertion is exact rather than approximate, so it fails if the
// charge is the wrong number as well as if it is absent. The timing assertion
// is a lower bound for the usual reason.
func TestTheByteAllowanceIsChargedTheMessagesSize(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)

	const (
		messages = 4
		each     = 2048
		perSec   = 4096
		total    = messages * each
	)
	// A bucket that starts holding one second's worth, refilling at the same
	// rate: total bytes take at least (total-perSec)/perSec seconds.
	const atLeast = (total - perSec) * time.Second / perSec

	srcAddr, srcUser := startAccount(t, "Work")
	dstAddr, _ := startAccount(t)
	for i := range messages {
		body := paddedMessage(t, fmt.Sprintf("subject-%03d", i), fmt.Sprintf("m%d@example.test", i), each)
		if _, err := srcUser.Append("Work", bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	dir := t.TempDir()
	started := time.Now()
	out := runCLI(t, []string{
		"sync",
		"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
		"--source-password-env", "TEST_IMAP_PASSWORD",
		"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", filepath.Join(dir, "state.db"),
		"--max-bytes-per-second", fmt.Sprint(perSec),
		"--log-level", "error",
	})
	elapsed := time.Since(started)

	if elapsed < atLeast {
		t.Errorf("moved %d bytes in %s under a limit of %d a second; that is at least %s of work,\n"+
			"so the byte allowance is not being charged what the messages weigh:\n%s",
			total, elapsed, perSec, atLeast, out)
	}
	if want := "having moved\n" + humanBytes(total) + " of message data"; !strings.Contains(out, want) {
		t.Errorf("the report does not say it moved %q; the charge is not the message's size:\n%s", want, out)
	}
}
