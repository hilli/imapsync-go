package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

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

	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("running %v: %v\n%s", args, err, out.String())
	}
	return out.String()
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

	_, _, _, _, err := syncEndpoints(syncFlags{sourceURL: "imaps://a@src.example", sourcePasswordEnv: "X"})
	if err == nil || !strings.Contains(err.Error(), "--dest-url") {
		t.Errorf("error = %v, want one asking for the destination", err)
	}
}
