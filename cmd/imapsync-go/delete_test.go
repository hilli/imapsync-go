package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/hilli/imapsync-go/internal/imapx"
)

// TestDeletingIsOptInFromTheCommandLine walks the whole surface: the flag has
// to reach the engine, the report has to say what happened, and neither may
// occur without --delete2. The engine's own tests cover the semantics; this
// covers the wiring, which is where a flag that is parsed but never passed on
// hides.
func TestDeletingIsOptInFromTheCommandLine(t *testing.T) {
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

	// One message leaves the source.
	ctx := context.Background()
	src := cliDial(t, srcAddr)
	if _, err := src.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
		t.Fatalf("selecting source: %v", err)
	}
	uids, err := src.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("listing source UIDs: %v", err)
	}
	if err := src.DeleteMessages(ctx, uids[:1]); err != nil {
		t.Fatalf("removing a source message: %v", err)
	}

	// Without the flag the destination keeps it, and the report does not so
	// much as mention deletion.
	out := run("--full")
	if got := countOn(t, dstUser, "INBOX"); got != 6 {
		t.Errorf("destination holds %d messages, want 6: something deleted without being asked", got)
	}
	if strings.Contains(out, "DELETED") || strings.Contains(out, "deleted") {
		t.Errorf("a run that was not asked to delete talks about deleting:\n%s", out)
	}

	out = run("--full", "--delete2")
	if got := countOn(t, dstUser, "INBOX"); got != 5 {
		t.Errorf("destination holds %d messages, want 5: --delete2 did not reach the engine\n%s", got, out)
	}
	if !strings.Contains(out, "DELETED") || !strings.Contains(out, "1 deleted") {
		t.Errorf("the report does not say what was deleted:\n%s", out)
	}
}

// TestARefusalIsNotAQuietSuccess is the reason the ceiling is worth having at
// all. A scheduled sync that reports success while a folder silently stops
// being mirrored is worse than one that fails loudly, so a refusal has to show
// up in the report, in the exit status, and with instructions.
//
// Both ways past it are exercised, because the two flags fail differently when
// they are not wired up: --force does nothing visible, while --delete2-ceiling
// carries the same default on both sides and so would go unnoticed entirely.
func TestARefusalIsNotAQuietSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		past []string
	}{
		{"forced", []string{"--force"}},
		{"ceiling raised", []string{"--delete2-ceiling", "0.6"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srcAddr, srcUser, _ := startCountedAccount(t)
			dstAddr, dstUser, _ := startCountedAccount(t)

			for i := range 60 {
				body := cliMessage(fmt.Sprintf("subject-%03d", i), fmt.Sprintf("m%d@example.test", i))
				if _, err := srcUser.Append("INBOX", bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
					t.Fatalf("seeding: %v", err)
				}
			}

			t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
			statePath := filepath.Join(t.TempDir(), "state.db")
			args := func(extra ...string) []string {
				return append([]string{
					"sync",
					"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
					"--source-password-env", "TEST_IMAP_PASSWORD",
					"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
					"--dest-password-env", "TEST_IMAP_PASSWORD",
					"--state", statePath,
					"--log-level", "error",
				}, extra...)
			}

			runCLI(t, args())

			// Half the source disappears — the shape of a source that answered
			// a listing with less than the truth, and far past the handful the
			// floor allows through on its own.
			ctx := context.Background()
			src := cliDial(t, srcAddr)
			if _, err := src.Select(ctx, "INBOX", imapx.SelectOptions{}); err != nil {
				t.Fatalf("selecting source: %v", err)
			}
			uids, err := src.AllUIDs(ctx)
			if err != nil {
				t.Fatalf("listing source UIDs: %v", err)
			}
			if err := src.DeleteMessages(ctx, uids[:30]); err != nil {
				t.Fatalf("removing source messages: %v", err)
			}

			out, err := runCLIErr(t, args("--full", "--delete2"))
			if err == nil {
				t.Error("a run that refused to delete exited successfully")
			}
			if got := countOn(t, dstUser, "INBOX"); got != 60 {
				t.Errorf("destination holds %d messages, want 60: the ceiling did not hold", got)
			}
			for _, want := range []string{"REFUSED", "--force", "--delete2-ceiling"} {
				if !strings.Contains(out, want) {
					t.Errorf("the report never mentions %q, so nobody can act on it:\n%s", want, out)
				}
			}

			// The ways through are the ones the report names.
			runCLI(t, args(append([]string{"--full", "--delete2"}, tc.past...)...))
			if got := countOn(t, dstUser, "INBOX"); got != 30 {
				t.Errorf("destination holds %d messages, want 30: %s did not get past the ceiling", got, tc.past[0])
			}
		})
	}
}

// countOn asks the server how many messages a mailbox holds, rather than
// counting what a listing claims. A deletion test that trusted a listing would
// be trusting the very thing --delete2's ceiling exists to distrust.
func countOn(t *testing.T, user *imapmemserver.User, mailbox string) uint32 {
	t.Helper()

	status, err := user.Status(mailbox, &imap.StatusOptions{NumMessages: true})
	if err != nil {
		t.Fatalf("reading %q status: %v", mailbox, err)
	}
	if status.NumMessages == nil {
		t.Fatalf("server did not report a message count for %q", mailbox)
	}
	return *status.NumMessages
}
