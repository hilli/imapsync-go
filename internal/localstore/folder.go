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
	"time"

	_ "modernc.org/sqlite" // pure Go driver, registered as "sqlite"
)

// schema is the per-folder database.
//
// It is not state.db and must never be merged with it. state.db records the
// relationship between two endpoints for one sync; this records what this
// folder contains, and has to be readable by someone who knows nothing about
// whatever sync produced it.
const schema = `
CREATE TABLE IF NOT EXISTS folder (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  name        TEXT    NOT NULL,
  uidvalidity INTEGER NOT NULL,
  uidnext     INTEGER NOT NULL,
  subscribed  INTEGER NOT NULL DEFAULT 0
);

-- One row per message. Flags and the authoritative internal date are the only
-- things here that the filesystem cannot express; existence is never asked of
-- this table.
CREATE TABLE IF NOT EXISTS messages (
  uid          INTEGER PRIMARY KEY,
  flags        TEXT    NOT NULL DEFAULT '',
  internaldate INTEGER NOT NULL DEFAULT 0,
  size         INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
`

// folder is one open mailbox directory.
type folder struct {
	name string // the true IMAP name, as the server spelled it
	dir  string
	db   *sql.DB

	mu          sync.Mutex
	uidNext     uint32
	uidValidity uint32
	subscribed  bool
}

// dsn builds the driver's URI for a folder database.
//
// The path is percent-escaped because SQLite decodes %HH in a file: URI, and a
// folder name is not ours to choose: "Rejsen 50%" encodes to a directory whose
// name contains a percent sign, and passing it raw makes SQLite look for a
// file that does not exist. Escaping was verified against the driver rather
// than assumed.
func dsn(path string) string {
	return "file:" + escapeDSNPath(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=synchronous(NORMAL)"
}

// escapeDSNPath hides from SQLite's URI parser the three characters that mean
// something to it.
var escapeDSNPath = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace

// createFolder makes the directory, its staging area and its database.
func createFolder(ctx context.Context, dir, name string) error {
	if err := os.MkdirAll(filepath.Join(dir, tmpName), dirPerm); err != nil {
		return fmt.Errorf("creating folder %q: %w", name, err)
	}
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, dbName)))
	if err != nil {
		return fmt.Errorf("creating folder %q: %w", name, err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating folder %q: %w", name, err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT OR IGNORE INTO folder (id, name, uidvalidity, uidnext) VALUES (1, ?, ?, 1)`,
		name, newUIDValidity())
	if err != nil {
		return fmt.Errorf("creating folder %q: %w", name, err)
	}
	return nil
}

// newUIDValidity returns an identifier that has not been used before by this
// folder. Seconds since the epoch is what every IMAP server uses for the same
// purpose, and it is monotonic for the same reason.
// newUIDValidity returns an identifier the folder has not used before.
//
// Seconds since the epoch is what every IMAP server uses, because RFC 3501
// requires the value to ascend strictly and 32 bits leave no room for finer
// resolution. That resolution is the catch: a folder whose database is lost
// and rebuilt in the same second as the value it is replacing would repeat it,
// telling the peer nothing changed at the one moment everything did. The
// counter closes that for a single process; across runs the clock has moved.
var lastUIDValidity struct {
	mu   sync.Mutex
	seen uint32
}

func newUIDValidity() uint32 {
	lastUIDValidity.mu.Lock()
	defer lastUIDValidity.mu.Unlock()
	v := uint32(time.Now().Unix()) //nolint:gosec // seconds since the epoch fit until 2106, which is what every IMAP server relies on
	if v <= lastUIDValidity.seen {
		v = lastUIDValidity.seen + 1
	}
	lastUIDValidity.seen = v
	return v
}

// noteUIDValidity records a value found on disk so that the next one issued
// exceeds it.
func noteUIDValidity(v uint32) {
	lastUIDValidity.mu.Lock()
	defer lastUIDValidity.mu.Unlock()
	if v > lastUIDValidity.seen {
		lastUIDValidity.seen = v
	}
}

// openFolder opens an existing folder and reconciles it against its directory.
func openFolder(ctx context.Context, dir string) (*folder, error) {
	dbPath := filepath.Join(dir, dbName)
	f := &folder{dir: dir}

	db, err := sql.Open("sqlite", dsn(dbPath))
	if err == nil {
		if err = db.PingContext(ctx); err == nil {
			err = healthy(ctx, db)
		}
		if err != nil {
			_ = db.Close()
		}
	}
	if err != nil {
		// A database that will not open, or that fails its own integrity
		// check, is treated exactly as a missing one: the mail is the truth
		// and this file is a cache. Dovecot's advice for a damaged index is
		// the same — delete it and leave the messages alone.
		if db, err = rebuild(ctx, dbPath, dir); err != nil {
			return nil, err
		}
	}
	f.db = db

	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("opening folder %s: %w", dir, err)
	}
	if err := f.load(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := f.reconcile(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return f, nil
}

func healthy(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity check: %s", result)
	}
	return nil
}

// rebuild discards an unusable database and starts a fresh one.
//
// The folder takes a new UIDVALIDITY, because the old numbering cannot be
// reconstructed: rebuilding uidnext as the highest surviving UID plus one
// would hand out again the numbers of messages that were deleted, and IMAP
// promises within a UIDVALIDITY that it never does. A new UIDVALIDITY is the
// protocol's own way of saying "my numbering is not the one you remember".
func rebuild(ctx context.Context, dbPath, dir string) (*sql.DB, error) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("discarding unusable database %s: %w", dbPath+suffix, err)
		}
	}
	if err := createFolder(ctx, dir, folderName(filepath.Base(dir))); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		return nil, fmt.Errorf("reopening folder %s: %w", dir, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("reopening folder %s: %w", dir, err)
	}
	return db, nil
}

func (f *folder) load(ctx context.Context) error {
	var subscribed int
	err := f.db.QueryRowContext(ctx,
		`SELECT name, uidvalidity, uidnext, subscribed FROM folder WHERE id = 1`).
		Scan(&f.name, &f.uidValidity, &f.uidNext, &subscribed)
	if errors.Is(err, sql.ErrNoRows) {
		f.name = folderName(filepath.Base(f.dir))
		f.uidValidity, f.uidNext = newUIDValidity(), 1
		_, err = f.db.ExecContext(ctx,
			`INSERT INTO folder (id, name, uidvalidity, uidnext) VALUES (1, ?, ?, ?)`,
			f.name, f.uidValidity, f.uidNext)
	} else if err == nil {
		// Whatever this folder has already used, the next one must beat, even
		// if the clock disagrees.
		noteUIDValidity(f.uidValidity)
	}
	if err != nil {
		return fmt.Errorf("reading folder %s: %w", f.dir, err)
	}
	f.subscribed = subscribed != 0
	return nil
}

// onDisk is what a directory scan found.
type onDisk struct {
	uids    map[uint32]string // uid -> path relative to the folder
	foreign []string          // .eml files whose names are not UIDs
	maxUID  uint32
}

// scan reads the folder's directory and its shards. It asks for names only —
// no stat per message — because this runs once per folder per run and a large
// folder holds 413,954 of them.
func scan(dir string) (onDisk, error) {
	found := onDisk{uids: make(map[uint32]string)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return found, fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir() && isShardDir(name):
			shard, err := os.ReadDir(filepath.Join(dir, name))
			if err != nil {
				return found, fmt.Errorf("reading %s: %w", filepath.Join(dir, name), err)
			}
			for _, s := range shard {
				found.add(name+"/"+s.Name(), s.Name(), s.IsDir())
			}
		default:
			found.add(name, name, e.IsDir())
		}
	}
	return found, nil
}

func (d *onDisk) add(rel, name string, isDir bool) {
	if isDir || !isMessageName(name) {
		return
	}
	uid, ok := parseMessageName(name)
	if !ok {
		d.foreign = append(d.foreign, rel)
		return
	}
	d.uids[uid] = rel
	if uid > d.maxUID {
		d.maxUID = uid
	}
}

// reconcile makes the database agree with the directory, with the directory
// winning every disagreement.
//
// Files get deleted in Finder, restored from backups, and copied in from
// elsewhere. A store that answered "does this message exist?" from its own
// database would report mail that is gone and overlook mail that is there —
// the same bug this project already met from the other side, when iCloud's
// SEARCH index offered 100,184 UIDs for a folder holding 487 messages.
func (f *folder) reconcile(ctx context.Context) error {
	found, err := scan(f.dir)
	if err != nil {
		return err
	}

	// A .eml nobody here named is somebody's own mail, dropped in by hand.
	// Adopting it is the point: mail can be added to the store with Finder.
	for _, rel := range found.foreign {
		uid, err := f.claim(ctx, found.maxUID)
		if err != nil {
			return err
		}
		if uid > found.maxUID {
			found.maxUID = uid
		}
		dst := messageRel(uid)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(f.dir, dst)), dirPerm); err != nil {
			return fmt.Errorf("adopting %s: %w", rel, err)
		}
		if err := os.Rename(filepath.Join(f.dir, rel), filepath.Join(f.dir, dst)); err != nil {
			return fmt.Errorf("adopting %s: %w", rel, err)
		}
		found.uids[uid] = dst
	}

	known, err := f.knownUIDs(ctx)
	if err != nil {
		return err
	}

	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reconciling %s: %w", f.dir, err)
	}
	defer func() { _ = tx.Rollback() }()

	for uid, rel := range found.uids {
		if _, ok := known[uid]; ok {
			continue
		}
		// A file with no row. Its flags are unknown and a later sync from a
		// live source fills them in; its date comes from the modification
		// time, which cp -p, rsync -t and Finder all preserve when a message
		// is copied in from somewhere else.
		info, err := os.Stat(filepath.Join(f.dir, rel))
		if err != nil {
			continue // vanished mid-scan; the next run will see it
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO messages (uid, flags, internaldate, size) VALUES (?, '', ?, ?)`,
			uid, info.ModTime().Unix(), info.Size()); err != nil {
			return fmt.Errorf("adopting message %d in %s: %w", uid, f.dir, err)
		}
	}

	for uid := range known {
		if _, ok := found.uids[uid]; ok {
			continue
		}
		// A row with no file: deleted outside the tool. That is a legitimate
		// way to remove mail from the store, not an error.
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE uid = ?`, uid); err != nil {
			return fmt.Errorf("forgetting message %d in %s: %w", uid, f.dir, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reconciling %s: %w", f.dir, err)
	}
	return f.raiseUIDNext(ctx, found.maxUID)
}

// raiseUIDNext keeps UID allocation monotonic.
//
// A database restored from an older backup is consistent, complains about
// nothing, and has a uidnext below UIDs already on disk. Left alone the store
// would issue those numbers a second time, so two different messages would
// share a UID inside one UIDVALIDITY — the guarantee IMAP makes most firmly,
// broken silently. Raising and never lowering heals a stale database instead.
func (f *folder) raiseUIDNext(ctx context.Context, maxUID uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if maxUID < f.uidNext {
		return nil
	}
	f.uidNext = maxUID + 1
	_, err := f.db.ExecContext(ctx, `UPDATE folder SET uidnext = ? WHERE id = 1`, f.uidNext)
	if err != nil {
		return fmt.Errorf("raising uidnext in %s: %w", f.dir, err)
	}
	return nil
}

// claim allocates the next UID. The lock is held for the increment only; the
// message itself is written outside it, which is the point of one file per
// message.
func (f *folder) claim(ctx context.Context, atLeast uint32) (uint32, error) {
	f.mu.Lock()
	if atLeast >= f.uidNext {
		f.uidNext = atLeast + 1
	}
	uid := f.uidNext
	f.uidNext++
	next := f.uidNext
	f.mu.Unlock()

	if _, err := f.db.ExecContext(ctx, `UPDATE folder SET uidnext = ? WHERE id = 1`, next); err != nil {
		return 0, fmt.Errorf("allocating a uid in %s: %w", f.dir, err)
	}
	return uid, nil
}

func (f *folder) knownUIDs(ctx context.Context) (map[uint32]struct{}, error) {
	rows, err := f.db.QueryContext(ctx, `SELECT uid FROM messages`)
	if err != nil {
		return nil, fmt.Errorf("reading messages in %s: %w", f.dir, err)
	}
	defer func() { _ = rows.Close() }()

	known := make(map[uint32]struct{})
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("reading messages in %s: %w", f.dir, err)
		}
		known[uid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading messages in %s: %w", f.dir, err)
	}
	return known, nil
}

// uids lists what is on disk, in order. Existence is a question for the
// directory, never for the database.
func (f *folder) uids() ([]uint32, error) {
	found, err := scan(f.dir)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, 0, len(found.uids))
	for uid := range found.uids {
		out = append(out, uid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// record is one message's annotations.
type record struct {
	uid   uint32
	flags []string
	date  time.Time
	size  int64
}

func (f *folder) records(ctx context.Context, uids []uint32) ([]record, error) {
	present, err := f.uids()
	if err != nil {
		return nil, err
	}
	live := make(map[uint32]struct{}, len(present))
	for _, uid := range present {
		live[uid] = struct{}{}
	}

	wanted := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		wanted[uid] = struct{}{}
	}

	rows, err := f.db.QueryContext(ctx,
		`SELECT uid, flags, internaldate, size FROM messages ORDER BY uid`)
	if err != nil {
		return nil, fmt.Errorf("reading messages in %s: %w", f.dir, err)
	}
	defer func() { _ = rows.Close() }()

	var out []record
	for rows.Next() {
		var (
			r     record
			flags string
			unix  int64
		)
		if err := rows.Scan(&r.uid, &flags, &unix, &r.size); err != nil {
			return nil, fmt.Errorf("reading messages in %s: %w", f.dir, err)
		}
		if _, ok := live[r.uid]; !ok {
			continue
		}
		if uids != nil {
			if _, ok := wanted[r.uid]; !ok {
				continue
			}
		}
		r.flags = splitFlags(flags)
		r.date = time.Unix(unix, 0)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading messages in %s: %w", f.dir, err)
	}
	return out, nil
}

func (f *folder) path(uid uint32) string {
	return filepath.Join(f.dir, messageRel(uid))
}

func splitFlags(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, " ")
}

func joinFlags(flags []string) string {
	clean := make([]string, 0, len(flags))
	for _, f := range flags {
		// \Recent is not a property of a message but of a session: it means
		// "arrived since another client last looked", it cannot be set by
		// APPEND, and this tool already strips it on the way out. Recording it
		// would be recording one server's passing mood.
		if strings.EqualFold(f, "\\Recent") || f == "" {
			continue
		}
		clean = append(clean, strings.ReplaceAll(f, " ", "_"))
	}
	sort.Strings(clean)
	return strings.Join(clean, " ")
}

// checkpoint empties the write-ahead log and closes the database.
//
// SQLite's own guidance is that copying a database file is safe "as long as
// there are no transactions in progress", so the store makes that its resting
// state rather than asking anyone to run a sync before their backup. TRUNCATE
// removes the -wal and -shm files instead of leaving them empty, so a store at
// rest is a tree of messages and one quiescent database per folder.
func (f *folder) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := f.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	closeErr := f.db.Close()
	if err != nil {
		return fmt.Errorf("checkpointing %s: %w", f.dir, err)
	}
	return closeErr
}
