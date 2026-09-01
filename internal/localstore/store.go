package localstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hilli/imapsync-go/internal/imapx"
)

// Store is a directory holding mail.
//
// It owns the open folder databases: several connections may be working in the
// same folder at once, and one database handle shared between them is both
// faster and safer than several handles on one file.
type Store struct {
	root string

	// readOnly stores belong to a dry run. Every write refuses rather than
	// being merely unused, because "nothing calls it" is not a guarantee and
	// this one is made to a backup.
	readOnly bool

	mu      sync.Mutex
	folders map[string]*folder
	closed  bool
}

// ErrReadOnly is what every write reports on a store opened for a dry run.
var ErrReadOnly = errors.New("this store is open read-only for a dry run")

// Open opens or creates a store rooted at dir.
func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("opening store %s: %w", dir, err)
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, fmt.Errorf("opening store %s: %w", abs, err)
	}
	s := &Store{root: abs, folders: make(map[string]*folder)}

	// Every IMAP account has an INBOX, and nothing creates it: a sync appends
	// to the destination's INBOX expecting it to be there, as it is on every
	// real server. A store that presents itself as an account has to supply
	// the one mailbox an account is guaranteed to have.
	if err := s.create(context.Background(), "INBOX"); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing store for a dry run, creating nothing.
//
// Open is not usable for this. It creates the root directory and then an
// INBOX inside it, so pointing a dry run at a directory the user had already
// made would materialise a mail store in it — and opening a folder would go on
// to adopt stray files by renaming them. Both were reachable, and the second
// rewrites mail the run had promised to leave alone.
//
// The directory has to exist, because the caller that has one that does not
// uses a scratch store instead: an empty store is the honest answer to "what
// is already at the destination" when the answer is nothing.
func OpenReadOnly(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("opening store %s: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("opening store %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("opening store %s: not a directory", abs)
	}
	return &Store{root: abs, readOnly: true, folders: make(map[string]*folder)}, nil
}

// Root is the directory the store lives in.
func (s *Store) Root() string { return s.root }

// Close checkpoints and closes every folder database, leaving the store in the
// state a backup can copy safely.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	names := make([]string, 0, len(s.folders))
	for dir := range s.folders {
		names = append(names, dir)
	}
	sort.Strings(names)

	var firstErr error
	for _, dir := range names {
		if err := s.folders[dir].close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.folders, dir)
	}
	return firstErr
}

// Connect returns a handle on the store. It is what pool.DialFunc calls, and
// it neither authenticates nor opens a socket.
func (s *Store) Connect(context.Context) (imapx.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("store is closed")
	}
	return &Conn{store: s}, nil
}

func (s *Store) dir(name string) string {
	return filepath.Join(s.root, filepath.FromSlash(folderRel(name)))
}

// isFolderDir reports whether a directory is a mailbox.
//
// A directory holding messages is a folder even with no database, because the
// database is a cache and the mail is the truth: deleting it — which is the
// documented repair — must not make a folder disappear along with it. The
// database is checked first only because it is one stat in the common case.
//
// A directory holding neither is scaffolding for the folders below it, which
// is why "Sendt" does not become a mailbox merely by containing "Sendt/2024".
func isFolderDir(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, dbName)); err == nil {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			if isShardDir(e.Name()) {
				return true
			}
			continue
		}
		if isMessageName(e.Name()) {
			return true
		}
	}
	return false
}

// open returns the shared handle on a folder, opening and reconciling it the
// first time it is asked for.
func (s *Store) open(ctx context.Context, name string) (*folder, error) {
	dir := s.dir(name)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("store is closed")
	}
	if f, ok := s.folders[dir]; ok {
		s.mu.Unlock()
		return f, nil
	}
	s.mu.Unlock()

	if !isFolderDir(dir) {
		return nil, fmt.Errorf("selecting %q: %w", name, fs.ErrNotExist)
	}

	open := openFolder
	if s.readOnly {
		open = openFolderReadOnly
	}
	f, err := open(ctx, dir)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.folders[dir]; ok {
		// Another connection got there first while this one was reconciling.
		_ = f.close()
		return existing, nil
	}
	s.folders[dir] = f
	return f, nil
}

// create makes a folder, refusing a name that would collide with an existing
// one on a case-insensitive filesystem.
func (s *Store) create(ctx context.Context, name string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	dir := s.dir(name)
	// The collision check comes first, because on a case-insensitive
	// filesystem the existence check below cannot tell "this folder already
	// exists" from "a folder spelled differently is standing in its place".
	if err := s.checkCaseCollision(dir, name); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, dbName)); err == nil {
		return nil
	}
	return createFolder(ctx, dir, name)
}

// checkCaseCollision refuses to create a folder whose name differs from an
// existing sibling only in case.
//
// macOS is case-insensitive, so "Archive" and "archive" — both legal and
// distinct on an IMAP server — would land in one directory and silently merge
// two folders into one. That is indistinguishable from losing mail, so it
// fails loudly instead.
func (s *Store) checkCaseCollision(dir, name string) error {
	parent := filepath.Dir(dir)
	leaf := filepath.Base(dir)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // the parent does not exist yet, so nothing to collide with
	}
	if err != nil {
		return fmt.Errorf("creating %q: %w", name, err)
	}
	for _, e := range entries {
		if !e.IsDir() || isShardDir(e.Name()) || e.Name() == tmpName {
			continue
		}
		if e.Name() != leaf && strings.EqualFold(e.Name(), leaf) {
			return fmt.Errorf(
				"creating %q: %q already exists and this filesystem does not "+
					"distinguish them, so the two folders would merge",
				name, folderName(e.Name()))
		}
	}
	return nil
}

// list walks the tree for folders, which are the directories holding a folder
// database.
func (s *Store) list(ctx context.Context) ([]imapx.Folder, error) {
	var out []imapx.Folder
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if path != s.root && (base == tmpName || isShardDir(base)) {
			return filepath.SkipDir
		}
		if !isFolderDir(path) {
			return nil
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil || rel == "." {
			return nil //nolint:nilerr // the root itself is not a folder
		}

		name := folderName(filepath.ToSlash(rel))
		if stored := s.trueName(ctx, path); stored != "" {
			name = stored
		}
		out = append(out, imapx.Folder{
			Name:       name,
			Delim:      Delim,
			Selectable: true,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing folders in %s: %w", s.root, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// trueName reads the folder's own record of its IMAP name, which is the
// spelling to restore with. A filesystem may hand back a different Unicode
// normalisation than the one it was given, so the path is only a fallback.
func (s *Store) trueName(ctx context.Context, dir string) string {
	s.mu.Lock()
	if f, ok := s.folders[dir]; ok {
		s.mu.Unlock()
		return f.name
	}
	s.mu.Unlock()

	// A read-only store reads even this with the immutable handle: listing the
	// folders is enough to open every database in the tree, and the read-write
	// URI would leave -wal and -shm beside each one.
	uri := dsn(filepath.Join(dir, dbName))
	if s.readOnly {
		uri = roDSN(filepath.Join(dir, dbName))
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return ""
	}
	defer func() { _ = db.Close() }()

	var name string
	if err := db.QueryRowContext(ctx, `SELECT name FROM folder WHERE id = 1`).Scan(&name); err != nil {
		return ""
	}
	return name
}
