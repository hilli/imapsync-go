package localstore_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/localstore"
)

const (
	testUser     = "sync@example.test"
	testPassword = "correct-horse"
)

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

// openStore opens a store in a temporary directory and closes it at the end,
// which is also what leaves it quiescent.
func openStore(t *testing.T, dir string) *localstore.Store {
	t.Helper()
	s, err := localstore.Open(dir)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", dir, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func connect(t *testing.T, s *localstore.Store) imapx.Conn {
	t.Helper()
	c, err := s.Connect(t.Context())
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	return c
}

// appendMessage puts one message in a folder and returns its UID.
func appendMessage(t *testing.T, c imapx.Conn, mailbox string, body []byte, flags []string, date time.Time) uint32 {
	t.Helper()
	res, err := c.Append(t.Context(), mailbox, imapx.AppendMessage{
		Size:         int64(len(body)),
		Flags:        flags,
		InternalDate: date,
		Body:         bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("Append(%q) error = %v", mailbox, err)
	}
	if !res.Assigned() {
		t.Fatalf("Append(%q) did not report where the message landed", mailbox)
	}
	return res.UID
}

func TestAppendAndFetchRoundTripsTheMessageUnchanged(t *testing.T) {
	t.Parallel()

	s := openStore(t, t.TempDir())
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	if _, err := c.Select(ctx, "Archive", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	body := testMessage("Hello", "one@example.test")
	uid := appendMessage(t, c, "Archive", body, []string{"\\Seen"}, time.Unix(1300094813, 0))

	var got bytes.Buffer
	n, err := c.FetchBody(ctx, uid, &got)
	if err != nil {
		t.Fatalf("FetchBody() error = %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("FetchBody() size = %d, want %d", n, len(body))
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Errorf("stored message differs from the one appended:\ngot  %q\nwant %q", got.Bytes(), body)
	}
}

// The point of the .eml name is that a person can double-click a message. That
// only holds if the file is the message.
func TestMessagesAreStoredAsThemselvesWithAnEmlName(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	body := testMessage("Hello", "one@example.test")
	uid := appendMessage(t, c, "Archive", body, nil, time.Unix(1300094813, 0))

	path := filepath.Join(root, "Archive", fmt.Sprintf("%010d.eml", uid))
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected the message at %s: %v", path, err)
	}
	if !bytes.Equal(onDisk, body) {
		t.Errorf("file at %s is not the message that was appended", path)
	}
}

// Finder shows "Date Created" from st_birthtime, which os.Chtimes cannot set.
// A store that set only the modification time would show every message as
// created on the day of the backup.
func TestBothFileTimestampsCarryTheInternalDate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	want := time.Date(2011, 3, 14, 9, 26, 53, 0, time.UTC)
	uid := appendMessage(t, c, "Archive", testMessage("Hello", "one@example.test"), nil, want)

	path := filepath.Join(root, "Archive", fmt.Sprintf("%010d.eml", uid))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(want) {
		t.Errorf("mtime = %v, want %v", info.ModTime().UTC(), want)
	}
	if birth, ok := birthtime(t, path); ok && !birth.Equal(want) {
		t.Errorf("birthtime = %v, want %v", birth.UTC(), want)
	}
}

func TestFetchMetaReportsFlagsDatesAndSizes(t *testing.T) {
	t.Parallel()

	s := openStore(t, t.TempDir())
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	if _, err := c.Select(ctx, "Archive", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	body := testMessage("Hello", "one@example.test")
	date := time.Unix(1300094813, 0)
	uid := appendMessage(t, c, "Archive", body, []string{"\\Seen", "\\Flagged"}, date)

	metas, err := c.FetchMeta(ctx, []uint32{uid}, []string{"Message-ID", "Subject"})
	if err != nil {
		t.Fatalf("FetchMeta() error = %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("FetchMeta() returned %d messages, want 1", len(metas))
	}
	m := metas[0]
	if m.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", m.Size, len(body))
	}
	if !m.InternalDate.Equal(date) {
		t.Errorf("InternalDate = %v, want %v", m.InternalDate, date)
	}
	if len(m.Flags) != 2 {
		t.Errorf("Flags = %v, want two", m.Flags)
	}
	if !bytes.Contains(m.Header, []byte("one@example.test")) {
		t.Errorf("Header lacks the requested Message-ID: %q", m.Header)
	}
	if bytes.Contains(m.Header, []byte("sender@example.test")) {
		t.Errorf("Header carries a field that was not asked for: %q", m.Header)
	}
}

// \Recent means "arrived since another client last looked". It is not a
// property of a message, cannot be set by APPEND, and this tool strips it on
// the way out.
func TestRecentIsNotStored(t *testing.T) {
	t.Parallel()

	s := openStore(t, t.TempDir())
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	if _, err := c.Select(ctx, "Archive", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	appendMessage(t, c, "Archive", testMessage("Hello", "one@example.test"),
		[]string{"\\Seen", "\\Recent"}, time.Unix(1300094813, 0))

	flags, err := c.FetchFlags(ctx, 0)
	if err != nil {
		t.Fatalf("FetchFlags() error = %v", err)
	}
	if len(flags) != 1 {
		t.Fatalf("FetchFlags() returned %d, want 1", len(flags))
	}
	for _, f := range flags[0].Flags {
		if strings.EqualFold(f, "\\Recent") {
			t.Errorf("\\Recent was stored: %v", flags[0].Flags)
		}
	}
}

// Flags change often; message files must not, or an incremental backup would
// re-copy a folder every time it was read.
func TestStoreFlagsDoesNotTouchTheMessageFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	if _, err := c.Select(ctx, "Archive", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	uid := appendMessage(t, c, "Archive", testMessage("Hello", "one@example.test"), nil, time.Unix(1300094813, 0))

	path := filepath.Join(root, "Archive", fmt.Sprintf("%010d.eml", uid))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := c.StoreFlags(ctx, uid, []string{"\\Seen", "\\Answered"}); err != nil {
		t.Fatalf("StoreFlags() error = %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the message moved or was rewritten when its flags changed: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("message mtime changed with its flags: %v -> %v", before.ModTime(), after.ModTime())
	}

	flags, err := c.FetchFlags(ctx, 0)
	if err != nil {
		t.Fatalf("FetchFlags() error = %v", err)
	}
	if len(flags) != 1 || len(flags[0].Flags) != 2 {
		t.Errorf("FetchFlags() = %v, want two flags on one message", flags)
	}
}

func TestLargeFoldersAreSplitAndSmallOnesAreNot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	if err := c.CreateFolder(ctx, "Big"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	if _, err := c.Select(ctx, "Big", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	// Reaching UID 10,000 by appending 10,000 messages would be a slow way to
	// prove a rule about UIDs, so a message is dropped in at a high UID and
	// adopted, which moves uidnext past the threshold.
	high := filepath.Join(root, "Big", "+0000010000", "0000010001.eml")
	if err := os.MkdirAll(filepath.Dir(high), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(high, testMessage("High", "high@example.test"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}

	s2 := openStore(t, root)
	c2 := connect(t, s2)
	if _, err := c2.Select(ctx, "Big", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	uid := appendMessage(t, c2, "Big", testMessage("Next", "next@example.test"), nil, time.Unix(1300094813, 0))
	if uid <= 10001 {
		t.Fatalf("uid = %d, want one past the message already on disk", uid)
	}

	want := filepath.Join(root, "Big", "+0000010000", fmt.Sprintf("%010d.eml", uid))
	if _, err := os.Stat(want); err != nil {
		t.Errorf("message with uid %d is not in its shard at %s: %v", uid, want, err)
	}

	// A folder below the threshold stays entirely flat.
	if err := c.CreateFolder(ctx, "Small"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	small := appendMessage(t, c, "Small", testMessage("S", "s@example.test"), nil, time.Unix(1300094813, 0))
	flat := filepath.Join(root, "Small", fmt.Sprintf("%010d.eml", small))
	if _, err := os.Stat(flat); err != nil {
		t.Errorf("small folder was split: %v", err)
	}
}

// A folder called "+0000010000" is a legal IMAP name and must not be mistaken
// for a shard directory.
func TestAFolderNamedLikeAShardIsEncoded(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	const name = "+0000010000"
	if err := c.CreateFolder(ctx, name); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "%2B0000010000", ".imapsync-folder.db")); err != nil {
		t.Errorf("folder %q was not encoded away from the shard namespace: %v", name, err)
	}

	if !listed(t, c).has(name) {
		t.Errorf("folder %q did not come back under its own name; got %v", name, listed(t, c))
	}
}

// names is what ListFolders reported, which always includes the INBOX every
// account has.
type names []string

func (n names) has(want string) bool { return slices.Contains(n, want) }

func listed(t *testing.T, c imapx.Conn) names {
	t.Helper()
	folders, err := c.ListFolders(t.Context(), imapx.ListOptions{})
	if err != nil {
		t.Fatalf("ListFolders() error = %v", err)
	}
	out := make(names, 0, len(folders))
	for _, f := range folders {
		out = append(out, f.Name)
	}
	return out
}

func TestFolderNamesSurviveTheirPunctuation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s := openStore(t, root)
	c := connect(t, s)
	ctx := t.Context()

	want := []string{"Arkiv", "Slettet post", "Sendt/2024", "Rejsen 50%", ".hidden"}
	for _, n := range want {
		if err := c.CreateFolder(ctx, n); err != nil {
			t.Fatalf("CreateFolder(%q) error = %v", n, err)
		}
	}

	got := listed(t, c)
	for _, n := range want {
		if !got.has(n) {
			t.Errorf("folder %q did not come back; got %v", n, got)
		}
	}
	// "Sendt" holds only "Sendt/2024" and no mail of its own, so it is
	// scaffolding rather than a mailbox.
	if got.has("Sendt") {
		t.Errorf("an intermediate directory was reported as a folder: %v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "Arkiv")); err != nil {
		t.Errorf("a plain name should be a plain directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Sendt", "2024")); err != nil {
		t.Errorf("hierarchy should be directories: %v", err)
	}
}
