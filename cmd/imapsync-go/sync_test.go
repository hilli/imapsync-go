package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/folder"
	"github.com/hilli/imapsync-go/internal/syncer"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/hilli/imapsync-go/internal/config"
)

const (
	cliUser     = "sync@example.test"
	cliPassword = "correct-horse"
)

// startAccount runs an in-memory IMAP server for the command to talk to.
func startAccount(t *testing.T, mailboxes ...string) (addr string, user *imapmemserver.User) {
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

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return ln.Addr().String(), user
}

type cliDiscardLogger struct{}

func (cliDiscardLogger) Printf(string, ...any) {}

func cliMessage(subject, messageID string) []byte {
	return []byte(fmt.Sprintf(
		"From: sender@example.test\r\n"+
			"To: %s\r\n"+
			"Subject: %s\r\n"+
			"Message-ID: <%s>\r\n"+
			"Date: Mon, 27 Aug 2026 12:00:00 +0000\r\n"+
			"\r\n"+
			"Body of %s.\r\n", cliUser, subject, messageID, subject))
}

// TestSyncCommandIsIdempotent is the milestone's headline promise, checked
// where a user would see it: run the command twice and the second run must copy
// nothing.
func TestSyncCommandIsIdempotent(t *testing.T) {
	srcAddr, srcUser := startAccount(t, "Work")
	dstAddr, _ := startAccount(t)

	for _, m := range []struct{ mailbox, subject, id string }{
		{"INBOX", "first", "a@example.test"},
		{"INBOX", "second", "b@example.test"},
		{"Work", "third", "c@example.test"},
	} {
		if _, err := srcUser.Append(m.mailbox, bytes.NewReader(cliMessage(m.subject, m.id)), &imap.AppendOptions{}); err != nil {
			t.Fatalf("seeding %q: %v", m.mailbox, err)
		}
	}

	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	dbPath := filepath.Join(t.TempDir(), "state.db")

	args := []string{
		"sync",
		"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
		"--source-password-env", "TEST_IMAP_PASSWORD",
		"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", dbPath,
		"--log-level", "error",
	}

	first := runCLI(t, args)
	if !strings.Contains(first, "2 folders, 3 copied") || !strings.Contains(first, "Created 1 destination folder:") {
		t.Errorf("first run did not copy 3 messages:\n%s", first)
	}

	second := runCLI(t, args)
	if !strings.Contains(second, "0 copied, 0 adopted") {
		t.Errorf("the second run was not a no-op:\n%s", second)
	}
}

// TestSyncCommandDryRunWritesNothing checks the preview at the command level,
// where the destination is a real server rather than a mock.
func TestSyncCommandDryRunWritesNothing(t *testing.T) {
	srcAddr, srcUser := startAccount(t, "Work")
	dstAddr, dstUser := startAccount(t)

	if _, err := srcUser.Append("INBOX", bytes.NewReader(cliMessage("first", "a@example.test")), &imap.AppendOptions{}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	out := runCLI(t, []string{
		"sync",
		"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
		"--source-password-env", "TEST_IMAP_PASSWORD",
		"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", filepath.Join(t.TempDir(), "state.db"),
		"--dry-run",
		"--log-level", "error",
	})

	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("dry run did not say so:\n%s", out)
	}
	if !strings.Contains(out, "Would create") {
		t.Errorf("dry run did not report the folder it would create:\n%s", out)
	}

	// The proof: the folder it said it would create does not exist.
	if _, err := dstUser.Status("Work", &imap.StatusOptions{NumMessages: true}); err == nil {
		t.Error("dry run created a destination folder")
	}
}

func runCLI(t *testing.T, args []string) string {
	t.Helper()

	out, err := runCLIErr(t, args)
	if err != nil {
		t.Fatalf("running %v: %v\n%s", args, err, out)
	}
	return out
}

func runCLIErr(t *testing.T, args []string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestFolderOptionsRejectsBadInput(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		flags   syncFlags
		wantErr string
	}{
		{"mapping without a separator", syncFlags{mappings: []string{"INBOX"}}, "want source=dest"},
		{"mapping with an empty source", syncFlags{mappings: []string{"=Archive"}}, "want source=dest"},
		{"unparseable include", syncFlags{include: []string{"["}}, "invalid --include"},
		{"unparseable exclude", syncFlags{exclude: []string{"(unclosed"}}, "invalid --exclude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := folderOptions(tc.flags, config.Folders{})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("folderOptions() error = %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// TestFolderOptionsMergesConfigAndFlags checks that a flag does not silently
// discard what the configuration file said.
func TestFolderOptionsMergesConfigAndFlags(t *testing.T) {
	t.Parallel()

	opts, err := folderOptions(
		syncFlags{mappings: []string{"Notes=Archive/Notes"}, exclude: []string{"^Junk$"}},
		config.Folders{
			Rules:   []config.MapRule{{From: "Sent Messages", To: "Sent"}},
			Exclude: []string{"^Trash$"},
		},
	)
	if err != nil {
		t.Fatalf("folderOptions() error = %v", err)
	}

	if got := opts.Mappings["Sent Messages"]; got != "Sent" {
		t.Errorf("config mapping lost: Mappings[%q] = %q", "Sent Messages", got)
	}
	if got := opts.Mappings["Notes"]; got != "Archive/Notes" {
		t.Errorf("flag mapping lost: Mappings[%q] = %q", "Notes", got)
	}
	if len(opts.Exclude) != 2 {
		t.Errorf("got %d exclude patterns, want the config's and the flag's", len(opts.Exclude))
	}
}

// TestDerivePairIDDependsOnBothEndpoints guards a mix-up that would be nearly
// impossible to diagnose: two migrations sharing a name in one state database,
// so one's progress is read as the other's and messages are never copied.
func TestDerivePairIDDependsOnBothEndpoints(t *testing.T) {
	t.Parallel()

	ep := func(url string) config.Endpoint { return config.Endpoint{URL: url} }

	base, err := derivePairID(ep("imaps://a@src.example"), ep("imaps://a@dst.example"))
	if err != nil {
		t.Fatalf("derivePairID() error = %v", err)
	}

	for _, tc := range []struct {
		name       string
		src, dst   string
		wantSameAs string
	}{
		{name: "different source host", src: "imaps://a@other.example", dst: "imaps://a@dst.example"},
		{name: "different destination host", src: "imaps://a@src.example", dst: "imaps://a@other.example"},
		{name: "different source user", src: "imaps://b@src.example", dst: "imaps://a@dst.example"},
		{name: "different destination user", src: "imaps://a@src.example", dst: "imaps://b@dst.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := derivePairID(ep(tc.src), ep(tc.dst))
			if err != nil {
				t.Fatalf("derivePairID() error = %v", err)
			}
			if got == base {
				t.Errorf("%s produced the same pair id %q", tc.name, got)
			}
		})
	}

	// The same pair must keep its identity across runs, or every run starts
	// from nothing.
	again, err := derivePairID(ep("imaps://a@src.example"), ep("imaps://a@dst.example"))
	if err != nil {
		t.Fatalf("derivePairID() error = %v", err)
	}
	if again != base {
		t.Errorf("the same pair produced two ids: %q and %q", base, again)
	}
}

func TestSyncEndpointsRequiresBothSides(t *testing.T) {
	t.Parallel()

	_, err := syncPair(syncFlags{sourceURL: "imaps://a@src.example", sourcePasswordEnv: "X"})
	if err == nil || !strings.Contains(err.Error(), "--dest-url") {
		t.Errorf("error = %v, want one asking for the destination", err)
	}
}

// startTLSAccount is startAccount over TLS with a self-signed certificate, so
// a connection succeeds only if that side's verification was disabled.
func startTLSAccount(t *testing.T, mailboxes ...string) (addr string, user *imapmemserver.User) {
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
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go func() { _ = srv.Serve(tls.NewListener(ln, tlsCfg)) }()
	t.Cleanup(func() { _ = srv.Close() })

	return ln.Addr().String(), user
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// A self-signed destination on your own network is a reasonable thing to
// accept. Accepting it must not also stop verifying the source, which is
// typically a public server reached over the internet. The two flags are
// therefore independent, and this proves they are not wired to the same bool
// and not wired to each other's side.
func TestInsecureIsPerSide(t *testing.T) {
	srcAddr, _ := startTLSAccount(t)
	dstAddr, _ := startTLSAccount(t)

	args := func(extra ...string) []string {
		return append([]string{
			"sync",
			"--source-url", "imaps://" + cliUser + "@" + srcAddr,
			"--source-password-env", "CLI_SRC_PW",
			"--dest-url", "imaps://" + cliUser + "@" + dstAddr,
			"--dest-password-env", "CLI_DST_PW",
			"--state", filepath.Join(t.TempDir(), "state.db"),
		}, extra...)
	}

	t.Setenv("CLI_SRC_PW", cliPassword)
	t.Setenv("CLI_DST_PW", cliPassword)

	for _, tc := range []struct {
		name  string
		flags []string
		stuck string
	}{
		{"neither", nil, "source"},
		{"source only", []string{"--source-insecure"}, "destination"},
		{"destination only", []string{"--dest-insecure"}, "source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCLIErr(t, args(tc.flags...))
			if err == nil {
				t.Fatalf("want a certificate failure on the %s, got success", tc.stuck)
			}
			if !strings.Contains(err.Error(), tc.stuck) {
				t.Fatalf("want the %s to fail, got: %v", tc.stuck, err)
			}
			if !strings.Contains(err.Error(), "certificate") {
				t.Fatalf("want a certificate failure, got: %v", err)
			}
		})
	}

	t.Run("both", func(t *testing.T) {
		if _, err := runCLIErr(t, args("--source-insecure", "--dest-insecure")); err != nil {
			t.Fatalf("with both sides insecure the sync should run: %v", err)
		}
	})
}

// Narrowing a large account with --folder makes almost every folder a skip.
// Listing all of them buries the ones imapsync-go chose to skip by itself,
// which are the only ones the caller has not already seen.
func TestReportSeparatesRequestedSkipsFromOurOwn(t *testing.T) {
	t.Parallel()

	report := syncer.Report{
		Folders: []syncer.FolderReport{{Source: "AU", Dest: "AU", Messages: 8, Copied: 8}},
		Skips: []folder.Skip{
			{Source: "Junk", Reason: "not in --folder", ByRequest: true},
			{Source: "Later", Reason: "not in --folder", ByRequest: true},
			{Source: "Starred", Reason: `\Flagged is a view over other mailboxes`, ByRequest: false},
		},
	}

	var out bytes.Buffer
	if err := writeSyncReport(&out, report, time.Second, false, nil); err != nil {
		t.Fatalf("writing report: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "Skipped 1 folder:") {
		t.Errorf("want a single listed skip, got:\n%s", got)
	}
	if !strings.Contains(got, "Starred") {
		t.Errorf("our own skip must be named, got:\n%s", got)
	}
	if strings.Contains(got, "Junk") || strings.Contains(got, "Later") {
		t.Errorf("requested skips must not be listed one by one, got:\n%s", got)
	}
	if !strings.Contains(got, "2 further folders left out by --folder") {
		t.Errorf("requested skips must still be counted, got:\n%s", got)
	}
}

// A dry run reports what it would do, so its totals must not claim it did it.
func TestDryRunReportDoesNotClaimToHaveCopied(t *testing.T) {
	t.Parallel()

	report := syncer.Report{
		Folders: []syncer.FolderReport{{Source: "AU", Dest: "AU", Messages: 8, Copied: 8}},
	}

	var out bytes.Buffer
	if err := writeSyncReport(&out, report, time.Second, true, nil); err != nil {
		t.Fatalf("writing report: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "8 to copy") {
		t.Errorf("want \"8 to copy\", got:\n%s", got)
	}
	if strings.Contains(got, "8 copied") {
		t.Errorf("a dry run must not say it copied anything, got:\n%s", got)
	}
}

// TestReportSaysWhatWidthEachSideSettledOn.
//
// The width a run settles on outlives the run: it is the only measurement
// anyone has of what a server will actually hold, and if it is not written down
// the next run asks for too many again.
//
// This test used to assert that a side which got what it asked for has nothing
// to say. A run of 776,791 messages proved that wrong: neither side shrank, so
// the report was silent, and "the server held 24 connections" was
// indistinguishable from "the pool is not instrumented". Holding the full width
// is the measurement that says a server tolerates that many, so both outcomes
// are reported.
func TestReportSaysWhatWidthEachSideSettledOn(t *testing.T) {
	t.Parallel()

	report := syncer.Report{
		Folders: []syncer.FolderReport{{Source: "AU", Dest: "AU", Messages: 8, Copied: 8}},
	}

	var out bytes.Buffer
	err := writeSyncReport(&out, report, time.Second, false, connections{
		{"source", "source-connections", 16, 16},
		{"destination", "dest-connections", 16, 5},
	})
	if err != nil {
		t.Fatalf("writing report: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "destination server would not hold 16 connections") {
		t.Errorf("the shrink must be reported, got:\n%s", got)
	}
	if !strings.Contains(got, "--dest-connections=5") {
		t.Errorf("the report must name the flag to set next time, got:\n%s", got)
	}
	if !strings.Contains(got, "source server held all 16 connections") {
		t.Errorf("a side that held its full width is a measurement too, got:\n%s", got)
	}
	if strings.Contains(got, "source server would not hold") {
		t.Errorf("the side that kept its width must not be reported as a shrink, got:\n%s", got)
	}
	if strings.Contains(got, "destination server held all") {
		t.Errorf("the side that shrank must not be reported as holding, got:\n%s", got)
	}
}

// TestReportNamesTheServerThatWouldNotReturnHeaders.
//
// The count has to survive into the text, because it is the only part of the run
// that says anything at all: the messages copied, no folder failed, and the run
// exits zero. It also has to name the side, since the first question on reading
// it is which server to complain to, and the consequences differ — a headerless
// source message gets stamped, a headerless destination message gets copied
// twice.
func TestReportNamesTheServerThatWouldNotReturnHeaders(t *testing.T) {
	t.Parallel()

	report := syncer.Report{
		Folders: []syncer.FolderReport{
			{Source: "INBOX", Dest: "INBOX", Messages: 40, Copied: 40, SourceHeaderless: 19},
			{Source: "Sent", Dest: "Sent", Messages: 10, Copied: 10, DestHeaderless: 3},
		},
	}

	var out bytes.Buffer
	if err := writeSyncReport(&out, report, time.Second, false, nil); err != nil {
		t.Fatalf("writing report: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "22 messages") {
		t.Errorf("want the total across folders, got:\n%s", got)
	}
	if !strings.Contains(got, "19 on the source") || !strings.Contains(got, "3 on the destination") {
		t.Errorf("want both sides named, got:\n%s", got)
	}
}

// TestAHealthyRunSaysNothingAboutHeaders.
//
// A warning printed on every run is one nobody reads, and this one has to be
// read on the day it appears.
func TestAHealthyRunSaysNothingAboutHeaders(t *testing.T) {
	t.Parallel()

	report := syncer.Report{
		Folders: []syncer.FolderReport{{Source: "INBOX", Dest: "INBOX", Messages: 40, Copied: 40}},
	}

	var out bytes.Buffer
	if err := writeSyncReport(&out, report, time.Second, false, nil); err != nil {
		t.Fatalf("writing report: %v", err)
	}
	if strings.Contains(out.String(), "no header") {
		t.Errorf("a healthy run warned about headers:\n%s", out.String())
	}
}
