package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/state"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// TestDedupRunsWithoutReachingTheSource is the claim the dedup command makes,
// tested where a user would make it.
//
// The source URL names a host that cannot be reached. A run that opened a
// source connection would fail on it, and this is the only level at which that
// can be proved: the syncer is given a nil source pool by construction, but
// nothing inside the syncer can say whether the command built a source
// connection before handing over -- and resolving a source credential can
// prompt for a keychain password, which is a visible cost to charge someone for
// a run that does not use it.
func TestDedupRunsWithoutReachingTheSource(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	dstAddr, dstUser := startAccount(t, "Archive")

	// Two byte-identical copies, as a mailbox that arrived with duplicates in
	// it would have -- and the same again in a folder that was not asked for,
	// so that --folder is doing something rather than merely being accepted.
	body := cliMessage("only", "only@example.test")
	stuffBody(t, dstUser, "Archive", body)
	stuffBody(t, dstUser, "Archive", body)
	elsewhere := cliMessage("elsewhere", "elsewhere@example.test")
	stuffBody(t, dstUser, "INBOX", elsewhere)
	stuffBody(t, dstUser, "INBOX", elsewhere)

	// A listener that answers nothing and counts what reaches it. The promise
	// in --help is that the source is named and not contacted, and a URL that
	// merely cannot be reached would prove only that the failure was tolerated.
	srcAddr, connections := deafListener(t)

	out := runCLI(t, []string{
		"dedup",
		"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
		"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", filepath.Join(t.TempDir(), "state.db"),
		"--folder", "Archive",
		"--log-level", "error",
	})

	if n := connections(); n != 0 {
		t.Errorf("the run opened %d connections to the source it promised not to contact", n)
	}

	if !strings.Contains(out, "Removed 1 message") {
		t.Errorf("the report does not say one message was removed:\n%s", out)
	}
	if got := countMessages(t, dstUser, "Archive"); got != 1 {
		t.Errorf("the folder holds %d messages, want 1:\n%s", got, out)
	}
	if got := countMessages(t, dstUser, "INBOX"); got != 2 {
		t.Errorf("a folder that was not asked for lost messages: %d left, want 2:\n%s", got, out)
	}
}

// TestDedupDryRunChangesNothing.
func TestDedupDryRunChangesNothing(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	dstAddr, dstUser := startAccount(t)

	body := cliMessage("twice", "twice@example.test")
	stuffBody(t, dstUser, "INBOX", body)
	stuffBody(t, dstUser, "INBOX", body)

	out := runCLI(t, []string{
		"dedup",
		"--source-url", "imap+insecure://" + cliUser + "@" + unroutable,
		"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", filepath.Join(t.TempDir(), "state.db"),
		"--dry-run",
		"--log-level", "error",
	})

	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("the report does not say it was a dry run:\n%s", out)
	}
	if !strings.Contains(out, "Would remove 1 message") {
		t.Errorf("the report does not say what it would remove:\n%s", out)
	}
	if got := countMessages(t, dstUser, "INBOX"); got != 2 {
		t.Errorf("the dry run left %d messages, want both:\n%s", got, out)
	}
}

// TestDedupReportAgreesWithItself catches the wording going out of step with
// the numbers, which nothing else reads back.
func TestDedupReportAgreesWithItself(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report dedupReportFixture
		want   []string
		avoid  []string
	}{
		{
			name:   "nothing found",
			report: dedupReportFixture{folders: []dedupFolderFixture{{dest: "INBOX", population: 12}}},
			want:   []string{"Removed 0 messages", "(none)"},
			avoid:  []string{"Refused", "differed"},
		},
		{
			name: "one of each",
			report: dedupReportFixture{folders: []dedupFolderFixture{
				{dest: "INBOX", population: 12, removed: 1, unequal: 1},
			}},
			want:  []string{"Removed 1 message from 1 folder holding 12", "1 message matched on headers and size but differed, and was left alone"},
			avoid: []string{"1 messages", "were left alone"},
		},
		{
			name: "several",
			report: dedupReportFixture{folders: []dedupFolderFixture{
				{dest: "INBOX", population: 40, removed: 2, unequal: 3},
				{dest: "Archive", population: 60, removed: 1},
			}},
			want:  []string{"Removed 3 messages from 2 folders holding 100", "3 messages matched", "were left alone"},
			avoid: []string{"3 message matched", "and was left alone"},
		},
		{
			name:   "refused",
			report: dedupReportFixture{folders: []dedupFolderFixture{{dest: "INBOX", population: 10, refused: 4}}},
			want:   []string{"Refused to remove 4 messages", "--force"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderDedupReport(t, tc.report, false)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("report does not contain %q:\n%s", want, got)
				}
			}
			for _, avoid := range tc.avoid {
				if strings.Contains(got, avoid) {
					t.Errorf("report contains %q, which does not agree with its own numbers:\n%s", avoid, got)
				}
			}
		})
	}
}

// unroutable is an address nothing answers on. 192.0.2.0/24 is TEST-NET-1
// (RFC 5737), reserved for documentation and not routed, so a connection
// attempt cannot reach a real host by accident.
const unroutable = "192.0.2.1:143"

func stuffBody(t *testing.T, u *imapmemserver.User, mailbox string, body []byte) {
	t.Helper()

	if _, err := u.Append(mailbox, bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
		t.Fatalf("appending to %q: %v", mailbox, err)
	}
}

func countMessages(t *testing.T, u *imapmemserver.User, mailbox string) int {
	t.Helper()

	d, err := u.Status(mailbox, &imap.StatusOptions{NumMessages: true})
	if err != nil {
		t.Fatalf("status of %q: %v", mailbox, err)
	}
	if d.NumMessages == nil {
		t.Fatalf("the server reported no message count for %q", mailbox)
	}
	return int(*d.NumMessages)
}

type dedupFolderFixture struct {
	dest                                  string
	population, removed, refused, unequal int
}

type dedupReportFixture struct {
	folders []dedupFolderFixture
	skips   int
}

// renderDedupReport prints a made-up report, so the wording can be read back
// without staging a server that produces exactly those numbers.
func renderDedupReport(t *testing.T, fx dedupReportFixture, dryRun bool) string {
	t.Helper()

	var report syncer.DedupReport
	for _, f := range fx.folders {
		report.Folders = append(report.Folders, syncer.DedupFolderReport{
			Dest:       f.dest,
			Population: f.population,
			Removed:    f.removed,
			Refused:    f.refused,
			Unequal:    f.unequal,
		})
	}
	for i := range fx.skips {
		report.Skips = append(report.Skips, syncer.DedupSkip{Dest: fmt.Sprintf("skipped%d", i)})
	}

	var buf bytes.Buffer
	if err := writeDedupReport(&buf, report, 250*time.Millisecond, dryRun); err != nil {
		t.Fatalf("writing the report: %v", err)
	}
	return buf.String()
}

// deafListener accepts connections, says nothing, and counts them.
//
// Counting is the point. An unroutable address proves only that a failed
// source connection is survivable, which is a weaker and less useful promise
// than the one the command makes.
func deafListener(t *testing.T) (addr string, count func() int) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var n atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), func() int { return int(n.Load()) }
}

// TestDedupFailsWhenTwoSourceFoldersShareOneDestination checks the exit code as
// well as the message.
//
// Two sets of records over one mailbox means re-pointing one set and leaving
// the other naming a message that is gone. A run that printed the refusal and
// exited zero would be a scheduled job reporting success while doing nothing,
// which is the failure mode this project keeps finding.
func TestDedupFailsWhenTwoSourceFoldersShareOneDestination(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	dstAddr, dstUser := startAccount(t, "Shared")
	srcAddr, _ := deafListener(t)

	body := cliMessage("shared", "shared@example.test")
	stuffBody(t, dstUser, "Shared", body)
	stuffBody(t, dstUser, "Shared", body)

	srcURL := "imap+insecure://" + cliUser + "@" + srcAddr
	dstURL := "imap+insecure://" + cliUser + "@" + dstAddr
	pairID, err := derivePairID(config.Endpoint{URL: srcURL}, config.Endpoint{URL: dstURL})
	if err != nil {
		t.Fatalf("deriving the pair id: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := state.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("opening state: %v", err)
	}
	for _, source := range []string{"One", "Two"} {
		if _, err := db.EnsureFolder(context.Background(), pairID, source, "Shared"); err != nil {
			t.Fatalf("recording folder %q: %v", source, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing state: %v", err)
	}

	out, err := runCLIErr(t, []string{
		"dedup",
		"--source-url", srcURL,
		"--dest-url", dstURL,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", dbPath,
		"--folder", "Shared",
		"--log-level", "error",
	})
	if err == nil {
		t.Fatalf("the command succeeded on a folder it refused to deduplicate:\n%s", out)
	}
	if got := countMessages(t, dstUser, "Shared"); got != 2 {
		t.Errorf("the refused folder lost messages: %d left, want 2:\n%s", got, out)
	}
}
