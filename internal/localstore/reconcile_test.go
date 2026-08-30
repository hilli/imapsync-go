package localstore_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/localstore"

	_ "modernc.org/sqlite"
)

// reopen closes a store and opens the same directory again, which is what a
// second run does.
func reopen(t *testing.T, s *localstore.Store, root string) (*localstore.Store, imapx.Conn) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	next := openStore(t, root)
	return next, connect(t, next)
}

func uidsIn(t *testing.T, c imapx.Conn, mailbox string) []uint32 {
	t.Helper()
	if _, err := c.Select(t.Context(), mailbox, imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select(%q) error = %v", mailbox, err)
	}
	uids, err := c.AllUIDs(t.Context())
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}
	return uids
}

// Deleting a message in Finder is a legitimate way to remove mail from the
// store. A store that answered "does this exist?" from its database would
// report mail that is gone — the bug this project already met from the other
// side, when iCloud's SEARCH index offered 100,184 UIDs for 487 messages.
func TestAMessageDeletedBehindTheStoresBackIsGone(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	keep := appendMessage(t, c, "Archive", testMessage("Keep", "keep@example.test"), nil, time.Unix(1300094813, 0))
	gone := appendMessage(t, c, "Archive", testMessage("Gone", "gone@example.test"), nil, time.Unix(1300094813, 0))

	if err := os.Remove(filepath.Join(root, "Archive", fmt.Sprintf("%010d.eml", gone))); err != nil {
		t.Fatalf("removing: %v", err)
	}

	// Without reopening: existence is read from the directory every time.
	if got := uidsIn(t, c, "Archive"); len(got) != 1 || got[0] != keep {
		t.Errorf("AllUIDs() = %v, want only %d", got, keep)
	}

	s2, c2 := reopen(t, s, root)
	_ = s2
	if got := uidsIn(t, c2, "Archive"); len(got) != 1 || got[0] != keep {
		t.Errorf("after reopening, AllUIDs() = %v, want only %d", got, keep)
	}
	flags, err := c2.FetchFlags(ctx, 0)
	if err != nil {
		t.Fatalf("FetchFlags() error = %v", err)
	}
	if len(flags) != 1 {
		t.Errorf("FetchFlags() returned %d messages, want 1: the row outlived its file", len(flags))
	}
}

// Mail can be added to the store by copying files into a folder, which falls
// out of treating the directory as the truth.
func TestAMessageCopiedInByHandIsAdopted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	appendMessage(t, c, "Archive", testMessage("First", "first@example.test"), nil, time.Unix(1300094813, 0))

	body := testMessage("Dropped in", "dropped@example.test")
	dropped := filepath.Join(root, "Archive", "Some message from a friend.eml")
	if err := os.WriteFile(dropped, body, 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}
	when := time.Unix(1400000000, 0)
	if err := os.Chtimes(dropped, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	s2, c2 := reopen(t, s, root)
	_ = s2
	uids := uidsIn(t, c2, "Archive")
	if len(uids) != 2 {
		t.Fatalf("AllUIDs() = %v, want the hand-copied message adopted", uids)
	}
	if _, err := os.Stat(dropped); !os.IsNotExist(err) {
		t.Errorf("the adopted message kept its old name")
	}

	metas, err := c2.FetchMeta(ctx, uids, nil)
	if err != nil {
		t.Fatalf("FetchMeta() error = %v", err)
	}
	var found bool
	for _, m := range metas {
		if !m.InternalDate.Equal(when) {
			continue
		}
		found = true
		var got bytes.Buffer
		if _, err := c2.FetchBody(ctx, m.UID, &got); err != nil {
			t.Fatalf("FetchBody() error = %v", err)
		}
		if !bytes.Equal(got.Bytes(), body) {
			t.Errorf("the adopted message was altered")
		}
	}
	if !found {
		t.Errorf("the adopted message did not take its date from the file: %+v", metas)
	}
}

// A database that will not open is the benign failure: it announces itself,
// and the mail is still there. The folder must come back with its messages
// under a new UIDVALIDITY.
//
// The new UIDVALIDITY is the whole of the fix rather than a detail of it. The
// highest deleted UID cannot be recovered from the surviving files, so the
// rebuilt folder will hand out numbers it has used before; what IMAP promises
// is not that a UID is never reused but that a (UIDVALIDITY, UID) pair is not,
// and changing the first is how a server says "my numbering is not the one you
// remember".
func TestALostDatabaseRebuildsUnderANewUIDValidity(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	var uids []uint32
	for i := range 3 {
		uids = append(uids, appendMessage(t, c,
			"Archive", testMessage(fmt.Sprintf("M%d", i), fmt.Sprintf("m%d@example.test", i)),
			nil, time.Unix(1300094813, 0)))
	}
	if _, err := c.Select(ctx, "Archive", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	before, err := c.Select(ctx, "Archive", imapx.SelectOptions{})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	// Delete the highest-numbered message, then the database, so that the
	// rebuilt folder cannot know UID 3 was ever issued.
	highest := uids[len(uids)-1]
	if err := os.Remove(filepath.Join(root, "Archive", fmt.Sprintf("%010d.eml", highest))); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := os.Remove(filepath.Join(root, "Archive", ".imapsync-folder.db")); err != nil {
		t.Fatalf("removing database: %v", err)
	}

	s2 := openStore(t, root)
	c2 := connect(t, s2)
	after, err := c2.Select(ctx, "Archive", imapx.SelectOptions{})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if after.NumMessages != 2 {
		t.Errorf("NumMessages = %d, want the 2 surviving messages", after.NumMessages)
	}
	if after.UIDValidity == before.UIDValidity {
		t.Errorf("UIDVALIDITY was reused after the database was lost: %d", after.UIDValidity)
	}

	// Reissuing the deleted message's UID is expected here, and is only safe
	// because the pair it forms has never been used.
	next := appendMessage(t, c2, "Archive", testMessage("New", "new@example.test"), nil, time.Unix(1300094813, 0))
	if next == highest && after.UIDValidity == before.UIDValidity {
		t.Errorf("uid %d was reissued under the same uidvalidity %d", next, after.UIDValidity)
	}
}

// A stale database is the dangerous case, because nothing complains. Restoring
// an older copy leaves uidnext below UIDs already on disk, and the store would
// issue those numbers a second time inside one UIDVALIDITY.
func TestAStaleDatabaseDoesNotIssueUIDsTwice(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	var uids []uint32
	for i := range 4 {
		uids = append(uids, appendMessage(t, c,
			"Archive", testMessage(fmt.Sprintf("M%d", i), fmt.Sprintf("m%d@example.test", i)),
			nil, time.Unix(1300094813, 0)))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Wind the database back, as restoring last month's backup would. The
	// messages on disk are untouched and the database is perfectly consistent.
	dbPath := filepath.Join(root, "Archive", ".imapsync-folder.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE folder SET uidnext = 2`); err != nil {
		t.Fatalf("winding back uidnext: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM messages WHERE uid > 1`); err != nil {
		t.Fatalf("winding back messages: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	s2 := openStore(t, root)
	c2 := connect(t, s2)
	mailbox, err := c2.Select(ctx, "Archive", imapx.SelectOptions{})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if mailbox.NumMessages != 4 {
		t.Errorf("NumMessages = %d, want all 4 still on disk", mailbox.NumMessages)
	}

	highest := uids[len(uids)-1]
	next := appendMessage(t, c2, "Archive", testMessage("New", "new@example.test"), nil, time.Unix(1300094813, 0))
	if next <= highest {
		t.Errorf("uid %d was issued again over a message already on disk (highest %d)", next, highest)
	}
}

// A message still in staging is not a message. The rename that publishes it is
// atomic, so a crash mid-append can never be observed as a half-written one.
func TestAnUnfinishedAppendIsInvisible(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	appendMessage(t, c, "Archive", testMessage("Real", "real@example.test"), nil, time.Unix(1300094813, 0))

	staged := filepath.Join(root, "Archive", ".tmp", "append-half-written")
	if err := os.WriteFile(staged, []byte("From: nobody\r\n\r\ntrunc"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}

	s2, c2 := reopen(t, s, root)
	_ = s2
	if got := uidsIn(t, c2, "Archive"); len(got) != 1 {
		t.Errorf("AllUIDs() = %v, want the staged file ignored", got)
	}
}

// Two IMAP folders differing only in case are legal and distinct. On a
// case-insensitive filesystem they would land in one directory, which is
// indistinguishable from losing mail.
func TestFoldersDifferingOnlyInCaseAreRefused(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	err := c.CreateFolder(ctx, "archive")

	if caseSensitive(t, root) {
		if err != nil {
			t.Fatalf("on a case-sensitive filesystem both folders are fine: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("creating %q beside %q was allowed; the two folders would merge", "archive", "Archive")
	}
}

// caseSensitive reports what the filesystem under dir actually does, rather
// than assuming from the operating system.
func caseSensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, ".Case-Probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatalf("probing: %v", err)
	}
	defer func() { _ = os.Remove(probe) }()
	_, err := os.Stat(filepath.Join(dir, ".case-probe"))
	return os.IsNotExist(err)
}

// rowCount reads the folder database directly, which is the only way to see a
// row for a message that no longer exists: every read path filters against the
// directory first, so the store answers correctly whether or not the row was
// ever cleaned up.
func rowCount(t *testing.T, root, folder string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, folder, ".imapsync-folder.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM messages`).Scan(&n); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	return n
}

// Mail deleted outside the tool must take its row with it. Nothing in the API
// can tell the difference, which is exactly why this needs asserting: without
// it a store whose mail is deleted keeps a row per message for ever, and over
// 776,802 messages that is the whole index retained to describe nothing.
func TestRowsDoNotOutliveTheirMessages(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	var uids []uint32
	for i := range 3 {
		uids = append(uids, appendMessage(t, c,
			"Archive", testMessage(fmt.Sprintf("M%d", i), fmt.Sprintf("m%d@example.test", i)),
			nil, time.Unix(1300094813, 0)))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if n := rowCount(t, root, "Archive"); n != 3 {
		t.Fatalf("rows = %d before deleting anything, want 3", n)
	}

	for _, uid := range uids[:2] {
		if err := os.Remove(filepath.Join(root, "Archive", fmt.Sprintf("%010d.eml", uid))); err != nil {
			t.Fatalf("removing: %v", err)
		}
	}

	s2 := openStore(t, root)
	c2 := connect(t, s2)
	if got := uidsIn(t, c2, "Archive"); len(got) != 1 {
		t.Fatalf("AllUIDs() = %v, want 1", got)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if n := rowCount(t, root, "Archive"); n != 1 {
		t.Errorf("rows = %d after two messages were deleted, want 1", n)
	}
}

// A folder whose name starts with a dot must not become a hidden directory.
// The store exists to be looked at: a mailbox the file browser refuses to show
// is one the person cannot restore from, or even find.
func TestAFolderNamedWithALeadingDotIsNotHidden(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)

	const name = ".hidden"
	if err := c.CreateFolder(t.Context(), name); err != nil {
		t.Fatalf("CreateFolder(%q) error = %v", name, err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading root: %v", err)
	}
	var found bool
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("folder %q became the hidden directory %q", name, e.Name())
		}
		if e.Name() != "INBOX" {
			found = true
		}
	}
	if !found {
		t.Errorf("folder %q produced no visible directory in %v", name, entries)
	}
	if !listed(t, c).has(name) {
		t.Errorf("folder %q did not come back under its own name", name)
	}
}

// Mail exported from another program is often numbered, and "42.eml" is not
// UID 42: taking it for one would give two messages the same number, or claim
// a number the folder had not issued.
func TestAShortNumericNameIsAdoptedRatherThanBelieved(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	mine := appendMessage(t, c, "Archive", testMessage("Mine", "mine@example.test"), nil, time.Unix(1300094813, 0))

	body := testMessage("Exported", "exported@example.test")
	if err := os.WriteFile(filepath.Join(root, "Archive", "42.eml"), body, 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}

	s2, c2 := reopen(t, s, root)
	_ = s2
	uids := uidsIn(t, c2, "Archive")
	if len(uids) != 2 {
		t.Fatalf("AllUIDs() = %v, want both messages", uids)
	}
	if slices.Contains(uids, 42) {
		t.Errorf("the exported file was read as uid 42 rather than adopted: %v", uids)
	}
	for _, uid := range uids {
		if uid != mine {
			if _, err := os.Stat(filepath.Join(root, "Archive", fmt.Sprintf("%010d.eml", uid))); err != nil {
				t.Errorf("adopted message %d is not stored under its uid: %v", uid, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "Archive", "42.eml")); !os.IsNotExist(err) {
		t.Errorf("the exported file kept its own name")
	}
}
