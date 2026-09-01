package localstore_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/localstore"
)

// openReadOnly opens a store for a dry run and closes it at the end.
func openReadOnly(t *testing.T, dir string) *localstore.Store {
	t.Helper()
	s, err := localstore.OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("OpenReadOnly(%q) error = %v", dir, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// treeOf fingerprints a directory: every path, and for files the size, the
// modification time and the contents. Anything a store writes shows up here,
// including the -wal and -shm files a plain read-only SQLite handle leaves
// behind.
func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			out = append(out, "dir  "+rel)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path) //nolint:gosec // a path this test just walked
		if err != nil {
			return err
		}
		out = append(out, "file "+rel+" "+
			info.ModTime().Format(time.RFC3339Nano)+" "+
			string(body))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func sameTree(t *testing.T, before, after []string, what string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s changed the tree: %d entries before, %d after\nbefore: %v\nafter:  %v",
			what, len(before), len(after), before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("%s changed the tree:\n before: %s\n after:  %s", what, before[i], after[i])
		}
	}
}

// seedStore builds a real store with one folder holding two messages, and
// leaves it quiescent.
func seedStore(t *testing.T, dir string) {
	t.Helper()
	s, err := localstore.Open(dir)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", dir, err)
	}
	c := connect(t, s)
	if err := c.CreateFolder(t.Context(), "Arkiv"); err != nil {
		t.Fatalf("CreateFolder() error = %v", err)
	}
	appendMessage(t, c, "Arkiv", testMessage("one", "one@example.test"),
		[]string{"\\Seen"}, time.Unix(1_700_000_000, 0))
	appendMessage(t, c, "Arkiv", testMessage("two", "two@example.test"),
		nil, time.Unix(1_700_000_100, 0))
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// TestADryRunDoesNotBuildAStoreInTheDestination is the bug this mode exists
// for, and it was found in the field rather than here.
//
// Open creates the root and then an INBOX inside it, so a dry run pointed at a
// directory the user had already made left .imapsync-folder.db and .tmp behind
// in it — while the README promised a dry run creates nothing at all. The
// destination existing is the whole trigger: the scratch-store path that was
// supposed to cover this only fires when it does not.
func TestADryRunDoesNotBuildAStoreInTheDestination(t *testing.T) {
	dir := t.TempDir()

	before := treeOf(t, dir)
	s := openReadOnly(t, dir)
	c := connect(t, s)
	if _, err := c.ListFolders(t.Context(), imapx.ListOptions{}); err != nil {
		t.Fatalf("ListFolders() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	} else if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("a read-only store created %v in an empty destination, want nothing", names)
	}
	sameTree(t, before, treeOf(t, dir), "opening a read-only store")
}

// TestAReadOnlyStoreLeavesAPopulatedStoreByteIdentical covers the paths that
// only exist once there is something to read: the folder databases, which a
// plain mode=ro handle would decorate with -wal and -shm, and the listing pass
// that opens every one of them to read its true name.
func TestAReadOnlyStoreLeavesAPopulatedStoreByteIdentical(t *testing.T) {
	dir := t.TempDir()
	seedStore(t, dir)

	before := treeOf(t, dir)

	s := openReadOnly(t, dir)
	c := connect(t, s)
	if _, err := c.ListFolders(t.Context(), imapx.ListOptions{}); err != nil {
		t.Fatalf("ListFolders() error = %v", err)
	}
	if _, err := c.Select(t.Context(), "Arkiv", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	uids, err := c.AllUIDs(t.Context())
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}
	if _, err := c.FetchMeta(t.Context(), uids, []string{"Message-ID"}); err != nil {
		t.Fatalf("FetchMeta() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sameTree(t, before, treeOf(t, dir), "reading a store read-only")
}

// TestAReadOnlyStoreStillAnswersWhatIsThere is the reason for reading the real
// directory rather than substituting an empty scratch store for it. A dry run
// that reported an existing backup as entirely uncopied would be keeping the
// promise about writes by breaking the one about answers.
func TestAReadOnlyStoreStillAnswersWhatIsThere(t *testing.T) {
	dir := t.TempDir()
	seedStore(t, dir)

	s := openReadOnly(t, dir)
	c := connect(t, s)

	folders, err := c.ListFolders(t.Context(), imapx.ListOptions{})
	if err != nil {
		t.Fatalf("ListFolders() error = %v", err)
	}
	var names []string
	for _, f := range folders {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "Arkiv" || names[1] != "INBOX" {
		t.Errorf("ListFolders() = %v, want [Arkiv INBOX]", names)
	}

	box, err := c.Select(t.Context(), "Arkiv", imapx.SelectOptions{})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if box.NumMessages != 2 {
		t.Errorf("Select().NumMessages = %d, want 2", box.NumMessages)
	}

	uids, err := c.AllUIDs(t.Context())
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}
	if len(uids) != 2 {
		t.Fatalf("AllUIDs() = %v, want 2 uids", uids)
	}

	metas, err := c.FetchMeta(t.Context(), uids, nil)
	if err != nil {
		t.Fatalf("FetchMeta() error = %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("FetchMeta() returned %d, want 2", len(metas))
	}
	// The flags come from the database, which is the half a directory listing
	// cannot supply: reading them proves the read-only handle is really
	// reading the cache rather than guessing from the filesystem.
	var seen bool
	for _, m := range metas {
		for _, fl := range m.Flags {
			if fl == "\\Seen" {
				seen = true
			}
		}
	}
	if !seen {
		t.Errorf("FetchMeta() lost the \\Seen flag: %+v", metas)
	}
}

// TestAReadOnlyStoreRefusesEveryWrite pins the refusals themselves rather than
// the absence of callers. "Nothing calls it during a dry run" was true of the
// adoption pass and still left the store one selected folder away from
// renaming somebody's mail.
func TestAReadOnlyStoreRefusesEveryWrite(t *testing.T) {
	dir := t.TempDir()
	seedStore(t, dir)

	before := treeOf(t, dir)

	s := openReadOnly(t, dir)
	c := connect(t, s)
	if _, err := c.Select(t.Context(), "Arkiv", imapx.SelectOptions{}); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	uids, err := c.AllUIDs(t.Context())
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}

	writes := map[string]error{
		"CreateFolder":    c.CreateFolder(t.Context(), "New"),
		"SubscribeFolder": c.SubscribeFolder(t.Context(), "Arkiv"),
		"StoreFlags":      c.StoreFlags(t.Context(), uids[0], []string{"\\Deleted"}),
		"DeleteMessages":  c.DeleteMessages(t.Context(), uids),
	}
	_, writes["Append"] = c.Append(t.Context(), "Arkiv", imapx.AppendMessage{
		Size: int64(len(testMessage("three", "three@example.test"))),
		Body: bytes.NewReader(testMessage("three", "three@example.test")),
	})

	for name, err := range writes {
		if !errors.Is(err, localstore.ErrReadOnly) {
			t.Errorf("%s() error = %v, want ErrReadOnly", name, err)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	sameTree(t, before, treeOf(t, dir), "a refused write")
}

// TestAReadOnlyFolderWithNoDatabaseReadsTheMailAnyway keeps the documented
// repair working under a dry run. Deleting a folder database is what the
// README tells people to do with a damaged one, and the mail beside it is the
// truth; a dry run may not rebuild the cache, so it does without rather than
// failing on a file it was never going to write.
func TestAReadOnlyFolderWithNoDatabaseReadsTheMailAnyway(t *testing.T) {
	dir := t.TempDir()
	seedStore(t, dir)

	if err := os.Remove(filepath.Join(dir, "Arkiv", ".imapsync-folder.db")); err != nil {
		t.Fatalf("removing the folder database: %v", err)
	}
	before := treeOf(t, dir)

	s := openReadOnly(t, dir)
	c := connect(t, s)
	box, err := c.Select(t.Context(), "Arkiv", imapx.SelectOptions{})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if box.NumMessages != 2 {
		t.Errorf("Select().NumMessages = %d, want 2", box.NumMessages)
	}
	uids, err := c.AllUIDs(t.Context())
	if err != nil {
		t.Fatalf("AllUIDs() error = %v", err)
	}
	if len(uids) != 2 {
		t.Errorf("AllUIDs() = %v, want 2 uids read from the directory", uids)
	}
	metas, err := c.FetchMeta(t.Context(), uids, nil)
	if err != nil {
		t.Fatalf("FetchMeta() error = %v", err)
	}
	if len(metas) != 2 {
		t.Errorf("FetchMeta() returned %d records from disk, want 2", len(metas))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Above all it must not have rebuilt the database it found missing.
	sameTree(t, before, treeOf(t, dir), "reading a folder whose database is gone")
}

// TestOpenReadOnlyRefusesAMissingDirectory keeps the two dry-run paths
// distinct: the caller that has no destination yet uses a scratch store, so a
// read-only store being asked for one is a mistake rather than an empty
// answer.
func TestOpenReadOnlyRefusesAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	if _, err := localstore.OpenReadOnly(missing); err == nil {
		t.Fatalf("OpenReadOnly(%q) succeeded, want an error", missing)
	}
}
