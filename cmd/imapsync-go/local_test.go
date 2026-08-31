package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// TestABackupRunsThroughTheOrdinarySyncCommand is the whole point of L1: a
// directory is a destination like any other, and no new verb was needed to
// reach it.
func TestABackupRunsThroughTheOrdinarySyncCommand(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	srcAddr, srcUser := startAccount(t, "Archive")
	stuff(t, srcUser, "INBOX", "one")
	stuff(t, srcUser, "Archive", "two")

	dir := filepath.Join(t.TempDir(), "backup")
	out := runCLI(t, []string{
		"sync",
		"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
		"--source-password-env", "TEST_IMAP_PASSWORD",
		"--dest-url", "file://" + dir,
		"--state", filepath.Join(t.TempDir(), "state.db"),
		"--log-level", "error",
	})

	for _, folder := range []string{"INBOX", "Archive"} {
		found := messageFiles(t, filepath.Join(dir, folder))
		if len(found) != 1 {
			t.Errorf("%s holds %d messages on disk, want 1:\n%s", folder, len(found), out)
		}
	}

	// The names are the promise that these open on a desktop, which is why the
	// store gave up being a maildir.
	for _, f := range messageFiles(t, filepath.Join(dir, "INBOX")) {
		if filepath.Ext(f) != ".eml" {
			t.Errorf("a message was written as %q, want a .eml file", filepath.Base(f))
		}
	}

	// Quiescent at rest. A backup tool copying this directory unattended must
	// not find a transaction in progress beside every folder.
	assertQuiescent(t, dir)
}

// TestAFailedStartStillSettlesTheStore names the property that a nil pointer
// dereference cost, and that no error message would have revealed.
//
// The unwind closures used to read the function's named results, which a
// `return nil, nil, nil, err` had already cleared — so a run that failed after
// opening a local store panicked partway through its own teardown and never
// reached the store. Left behind: the -wal files the store exists to avoid.
//
// The failure is provoked with an unusable connection count because it happens
// after both endpoints are open and before any mail moves, which is exactly
// the window the teardown has to survive.
func TestAFailedStartStillSettlesTheStore(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	dstAddr, _ := startAccount(t)

	dir := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("preparing the source directory: %v", err)
	}
	out, err := runCLIErr(t, []string{
		"sync",
		"--source-url", "file://" + dir,
		"--dest-url", "imap+insecure://" + cliUser + "@" + dstAddr,
		"--dest-password-env", "TEST_IMAP_PASSWORD",
		"--state", filepath.Join(t.TempDir(), "state.db"),
		"--dest-connections", "0",
		"--log-level", "error",
	})
	if err == nil {
		t.Fatalf("a destination connection count of 0 was accepted:\n%s", out)
	}

	// The store was opened, or this test proves nothing about closing it.
	if _, statErr := os.Stat(filepath.Join(dir, "INBOX")); statErr != nil {
		t.Fatalf("the store was never opened, so its teardown was never tested: %v", statErr)
	}
	assertQuiescent(t, dir)
}

// TestProbeRefusesADirectory. Every number probe prints is a property of a
// server, and a directory has none — so the honest answer is to say so rather
// than report a ceiling nothing measured.
func TestProbeRefusesADirectory(t *testing.T) {
	t.Parallel()

	out, err := runCLIErr(t, []string{"probe", "--url", "file://" + t.TempDir()})
	if err == nil {
		t.Fatalf("probe accepted a directory:\n%s", out)
	}
	if !strings.Contains(err.Error(), "file://") {
		t.Errorf("probe error = %v; it does not say which endpoint it means", err)
	}
}

// assertQuiescent checks that SQLite left nothing uncommitted beside a folder.
// A -wal or -shm file means a copy of this directory could be a copy of a
// transaction in progress.
func assertQuiescent(t *testing.T, dir string) {
	t.Helper()

	var busy []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm")) {
			busy = append(busy, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	if len(busy) > 0 {
		t.Errorf("the store was left mid-transaction: %v", busy)
	}
}

// messageFiles lists the .eml files under a folder, at whatever depth the
// store's sharding put them.
func messageFiles(t *testing.T, dir string) []string {
	t.Helper()

	var found []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".eml" {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return found
}

// stuff puts one message into a mailbox on the in-memory server.
func stuff(t *testing.T, u *imapmemserver.User, mailbox, subject string) {
	t.Helper()

	body := fmt.Appendf(nil,
		"Message-ID: <%s@example.test>\r\n"+
			"From: sender@example.test\r\n"+
			"Subject: %s\r\n"+
			"Date: Mon, 27 Aug 2026 12:00:00 +0000\r\n"+
			"\r\nBody.\r\n", subject, subject)
	if _, err := u.Append(mailbox, bytes.NewReader(body), &imap.AppendOptions{}); err != nil {
		t.Fatalf("appending to %q: %v", mailbox, err)
	}
}

// TestAMissingSourceDirectoryIsAnError is the difference between a restore that
// failed and a restore that succeeded at nothing.
//
// The store creates what it is pointed at, which is right for a destination and
// dangerous for a source: a mistyped path used to be created empty, diffed
// against the destination, and reported as a clean run with nothing to do and
// an exit code of zero. Someone checking that their mail came back would have
// been told it had.
func TestAMissingSourceDirectoryIsAnError(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "not-here")
	out, err := runCLIErr(t, []string{
		"sync",
		"--source-url", "file://" + missing,
		"--dest-url", "file://" + filepath.Join(t.TempDir(), "dest"),
		"--state", filepath.Join(t.TempDir(), "state.db"),
		"--log-level", "error",
	})
	if err == nil {
		t.Fatalf("a source directory that does not exist was accepted:\n%s", out)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v; it does not name the path that is missing", err)
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Error("the missing source directory was created anyway")
	}

	// A file where a directory belongs is the same mistake wearing a different
	// hat, and must not be opened as a store.
	file := filepath.Join(t.TempDir(), "mail.txt")
	if err := os.WriteFile(file, []byte("not a store"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", file, err)
	}
	if _, err := runCLIErr(t, []string{
		"sync",
		"--source-url", "file://" + file,
		"--dest-url", "file://" + filepath.Join(t.TempDir(), "dest"),
		"--state", filepath.Join(t.TempDir(), "state.db"),
		"--log-level", "error",
	}); err == nil {
		t.Error("a regular file was accepted as a source of mail")
	}
}

// TestADryRunCreatesNothing holds the tool to the plain meaning of the flag.
//
// A dry run against a destination that does not exist yet is the most likely
// dry run there is — it is how you check a new backup before committing to it
// — and it used to leave behind the directory tree and an INBOX, so the
// "preview" had already half-happened.
func TestADryRunCreatesNothing(t *testing.T) {
	t.Setenv("TEST_IMAP_PASSWORD", cliPassword)
	srcAddr, srcUser := startAccount(t)
	stuff(t, srcUser, "INBOX", "one")

	dest := filepath.Join(t.TempDir(), "backup")
	out := runCLI(t, []string{
		"sync",
		"--source-url", "imap+insecure://" + cliUser + "@" + srcAddr,
		"--source-password-env", "TEST_IMAP_PASSWORD",
		"--dest-url", "file://" + dest,
		"--state", filepath.Join(t.TempDir(), "state.db"),
		"--dry-run",
		"--log-level", "error",
	})

	if _, err := os.Stat(dest); err == nil {
		t.Errorf("--dry-run created %s:\n%s", dest, out)
	}

	// And it still has to say what a real run would do, or refusing to create
	// the directory would have cost the flag its purpose.
	if !strings.Contains(out, "1 to copy") {
		t.Errorf("the dry run did not report the message it would copy:\n%s", out)
	}
}
