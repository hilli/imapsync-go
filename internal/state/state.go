// Package state persists what has been synchronised.
//
// The store exists to make APPEND survivable. APPEND is not idempotent and no
// transaction spans IMAP and SQLite, so a crash between "the server accepted
// the message" and "we recorded it" would otherwise duplicate that message on
// the next run. Every write here is ordered so that a crash leaves a suspect
// row to investigate rather than a silent gap.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go driver, registered as "sqlite"
)

// State is the lifecycle of one message copy.
type State string

const (
	// StatePlanned means the message was seen on the source and not yet copied.
	StatePlanned State = "planned"
	// StateInFlight means an APPEND was issued and its outcome is unknown. Every
	// such row is a suspect after a crash.
	StateInFlight State = "in_flight"
	// StateDone means the destination UID is known and recorded.
	StateDone State = "done"
	// StateFailed means the copy was abandoned with a recorded reason.
	StateFailed State = "failed"
	// StateGone means the source listed this UID and then had no message for
	// it. That is a fact about the source rather than a failure of ours, and it
	// is permanent: within one UIDVALIDITY a UID is never reissued, so there
	// will never be anything at that number to copy.
	StateGone State = "gone"
)

// Folder is one source mailbox and the destination it maps to.
type Folder struct {
	ID     int64
	PairID string
	Source string
	Dest   string

	SrcUIDValidity   uint32
	DstUIDValidity   uint32
	SrcHighestModSeq uint64
	// SrcDeletedThrough is the source modseq up to which deletions have been
	// carried out on the destination. It is not the same as SrcHighestModSeq:
	// a run that was not asked to delete advances one and not the other.
	SrcDeletedThrough uint64
	LastSync          time.Time
}

// Message is one message copy, keyed by its source identity.
type Message struct {
	FolderID       int64
	SrcUIDValidity uint32
	SrcUID         uint32

	DstUIDValidity uint32
	DstUID         uint32

	// IdentHash is the tier-3 header digest, used to adopt messages already
	// present on the destination when the UID map cannot answer.
	IdentHash string
	// StampID is the tier-4 marker, set only for messages with no usable
	// Message-ID.
	StampID string

	State        State
	Size         int64
	Flags        string
	InternalDate time.Time
	LastError    string
}

// DB is the state store. It is safe for concurrent use.
type DB struct {
	db *sql.DB
}

// schema is applied in full on every open; each statement is idempotent.
const schema = `
CREATE TABLE IF NOT EXISTS folders (
  id                 INTEGER PRIMARY KEY,
  pair_id            TEXT    NOT NULL,
  source             TEXT    NOT NULL,
  dest               TEXT    NOT NULL,
  src_uidvalidity    INTEGER NOT NULL DEFAULT 0,
  dst_uidvalidity    INTEGER NOT NULL DEFAULT 0,
  src_highestmodseq  INTEGER NOT NULL DEFAULT 0,
  src_deleted_through INTEGER NOT NULL DEFAULT 0,
  last_sync          INTEGER NOT NULL DEFAULT 0,
  UNIQUE (pair_id, source)
);

CREATE TABLE IF NOT EXISTS messages (
  folder_id        INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  src_uidvalidity  INTEGER NOT NULL,
  src_uid          INTEGER NOT NULL,
  dst_uidvalidity  INTEGER NOT NULL DEFAULT 0,
  dst_uid          INTEGER NOT NULL DEFAULT 0,
  ident_hash       TEXT    NOT NULL DEFAULT '',
  stamp_id         TEXT    NOT NULL DEFAULT '',
  state            TEXT    NOT NULL,
  size             INTEGER NOT NULL DEFAULT 0,
  flags            TEXT    NOT NULL DEFAULT '',
  internaldate     INTEGER NOT NULL DEFAULT 0,
  last_error       TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (folder_id, src_uidvalidity, src_uid)
) WITHOUT ROWID;

-- Recovery scans by state within a folder, and adoption looks messages up by
-- digest without knowing their source UID.
CREATE INDEX IF NOT EXISTS messages_by_state ON messages (folder_id, state);
CREATE INDEX IF NOT EXISTS messages_by_ident ON messages (folder_id, ident_hash);
`

// Open opens or creates the state database at path.
//
// WAL is enabled because M2 runs many connections against this file at once,
// and a busy timeout is set so a concurrent writer waits rather than failing
// the run outright.
//
// The path is escaped because SQLite decodes %HH in a file: URI, so a state
// database under a directory whose name contains a percent sign — which is a
// path the user chose, not one we did — would otherwise be looked for
// somewhere else and reported as missing.
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := "file:" + escapeDSNPath(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening state database %s: %w", path, err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("opening state database %s: %w", path, err)
	}
	if _, err := sqlDB.ExecContext(ctx, schema); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("applying schema to %s: %w", path, err)
	}
	if err := migrate(ctx, sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return &DB{db: sqlDB}, nil
}

// migrations are applied to databases created before a column existed.
//
// CREATE TABLE IF NOT EXISTS does nothing to a table that is already there, so
// a column added to the schema above never reaches an existing state database —
// and an existing database is precisely the one that has something to lose.
var migrations = []string{
	`ALTER TABLE folders ADD COLUMN src_deleted_through INTEGER NOT NULL DEFAULT 0`,
}

// migrate applies each migration, treating a column that is already there as
// success. SQLite has no ADD COLUMN IF NOT EXISTS, so the error is the check.
func migrate(ctx context.Context, db *sql.DB) error {
	for _, stmt := range migrations {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}

// Close releases the database.
func (d *DB) Close() error {
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("closing state database: %w", err)
	}
	return nil
}

// EnsureFolder returns the stored row for a source mailbox, creating it if this
// is the first time the folder has been seen.
func (d *DB) EnsureFolder(ctx context.Context, pairID, source, dest string) (Folder, error) {
	const insert = `
INSERT INTO folders (pair_id, source, dest) VALUES (?, ?, ?)
ON CONFLICT (pair_id, source) DO UPDATE SET dest = excluded.dest`

	if _, err := d.db.ExecContext(ctx, insert, pairID, source, dest); err != nil {
		return Folder{}, fmt.Errorf("recording folder %q: %w", source, err)
	}

	const query = `
SELECT id, pair_id, source, dest, src_uidvalidity, dst_uidvalidity, src_highestmodseq, src_deleted_through, last_sync
FROM folders WHERE pair_id = ? AND source = ?`

	var (
		f        Folder
		lastSync int64
	)
	err := d.db.QueryRowContext(ctx, query, pairID, source).Scan(
		&f.ID, &f.PairID, &f.Source, &f.Dest,
		&f.SrcUIDValidity, &f.DstUIDValidity, &f.SrcHighestModSeq, &f.SrcDeletedThrough, &lastSync,
	)
	if err != nil {
		return Folder{}, fmt.Errorf("reading folder %q: %w", source, err)
	}
	if lastSync != 0 {
		f.LastSync = time.Unix(lastSync, 0).UTC()
	}
	return f, nil
}

// FenceUIDValidity reconciles the stored UIDVALIDITY pair with what the servers
// currently report, and reports whether the stored message rows survived.
//
// UIDVALIDITY changing means the server has renumbered the mailbox, so every
// recorded UID on that side is meaningless. Discarding the rows forces the next
// run to rebuild them by digest, which re-adopts the messages already on the
// destination instead of copying them again.
func (d *DB) FenceUIDValidity(ctx context.Context, folderID int64, srcUIDValidity, dstUIDValidity uint32) (kept bool, err error) {
	const query = `SELECT src_uidvalidity, dst_uidvalidity FROM folders WHERE id = ?`

	var storedSrc, storedDst uint32
	if err := d.db.QueryRowContext(ctx, query, folderID).Scan(&storedSrc, &storedDst); err != nil {
		return false, fmt.Errorf("reading folder %d: %w", folderID, err)
	}

	// A zero stored value means this folder has never been synchronised, which
	// is not an invalidation.
	changed := (storedSrc != 0 && storedSrc != srcUIDValidity) ||
		(storedDst != 0 && storedDst != dstUIDValidity)

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning UIDVALIDITY fence: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if changed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE folder_id = ?`, folderID); err != nil {
			return false, fmt.Errorf("invalidating folder %d: %w", folderID, err)
		}
		// The watermark goes with them. Modification sequences are only
		// comparable within one UIDVALIDITY, and a renumbered mailbox can come
		// back with a lower one than we stored — which would leave the folder
		// looking permanently up to date to a fast path that trusts it.
		if _, err := tx.ExecContext(ctx, `UPDATE folders SET src_highestmodseq = 0, src_deleted_through = 0 WHERE id = ?`, folderID); err != nil {
			return false, fmt.Errorf("clearing folder %d watermark: %w", folderID, err)
		}
	}
	const update = `UPDATE folders SET src_uidvalidity = ?, dst_uidvalidity = ? WHERE id = ?`
	if _, err := tx.ExecContext(ctx, update, srcUIDValidity, dstUIDValidity, folderID); err != nil {
		return false, fmt.Errorf("updating folder %d: %w", folderID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing UIDVALIDITY fence: %w", err)
	}

	return !changed, nil
}

// MarkSynced records a successful pass over a folder.
//
// A modseq of zero leaves the stored watermark alone rather than clearing it.
// Two callers need that: a server that does not report HIGHESTMODSEQ at all,
// and a folder that finished with messages still to copy, which must not
// advance the watermark but has no reason to forget the last one it earned —
// that older watermark still bounds which flags can have changed.
// deletedThrough follows the same rule for the same reason: zero leaves the
// stored value alone. A run that was not asked to delete, or that refused to,
// must not claim that deletions have been carried out up to this point, or the
// next run's fast path will skip the folder and the deletion will never happen.
func (d *DB) MarkSynced(ctx context.Context, folderID int64, highestModSeq, deletedThrough uint64, at time.Time) error {
	const update = `UPDATE folders
	                SET src_highestmodseq = CASE WHEN ?1 = 0 THEN src_highestmodseq ELSE ?1 END,
	                    src_deleted_through = CASE WHEN ?2 = 0 THEN src_deleted_through ELSE ?2 END,
	                    last_sync = ?3
	                WHERE id = ?4`
	//nolint:gosec // modseq values are far below 2^63 in practice
	if _, err := d.db.ExecContext(ctx, update, int64(highestModSeq), int64(deletedThrough), at.Unix(), folderID); err != nil {
		return fmt.Errorf("marking folder %d synced: %w", folderID, err)
	}
	return nil
}

// ClaimedDestUIDs returns the destination UIDs this folder has already mapped a
// source message onto.
//
// Adoption needs it. A destination message that some other source message is
// already recorded against must not be offered to a second one: two identical
// messages in the source and one copy at the destination is a real state, and
// collapsing them onto each other silently drops mail rather than copying it.
func (d *DB) ClaimedDestUIDs(ctx context.Context, folderID int64, dstUIDValidity uint32) (map[uint32]struct{}, error) {
	const query = `SELECT dst_uid FROM messages
	               WHERE folder_id = ? AND dst_uidvalidity = ? AND dst_uid IS NOT NULL AND dst_uid != 0`

	rows, err := d.db.QueryContext(ctx, query, folderID, dstUIDValidity)
	if err != nil {
		return nil, fmt.Errorf("reading claimed destination UIDs for folder %d: %w", folderID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[uint32]struct{})
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("reading claimed destination UIDs for folder %d: %w", folderID, err)
		}
		out[uid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading claimed destination UIDs for folder %d: %w", folderID, err)
	}
	return out, nil
}

// SyncedUIDs returns the state of every recorded message in a folder, keyed by
// source UID. It is the left-hand side of the folder diff.
func (d *DB) SyncedUIDs(ctx context.Context, folderID int64, srcUIDValidity uint32) (map[uint32]State, error) {
	const query = `SELECT src_uid, state FROM messages WHERE folder_id = ? AND src_uidvalidity = ?`

	rows, err := d.db.QueryContext(ctx, query, folderID, srcUIDValidity)
	if err != nil {
		return nil, fmt.Errorf("reading synchronised UIDs for folder %d: %w", folderID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[uint32]State)
	for rows.Next() {
		var (
			uid uint32
			st  State
		)
		if err := rows.Scan(&uid, &st); err != nil {
			return nil, fmt.Errorf("reading synchronised UIDs for folder %d: %w", folderID, err)
		}
		out[uid] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading synchronised UIDs for folder %d: %w", folderID, err)
	}
	return out, nil
}

// MarkGone records that the source has no message at this UID.
//
// iCloud's SEARCH ALL is the reason this exists: on a 414k-message INBOX it
// returns just over half a million UIDs, and around ninety thousand of them
// have no message behind them. Without somewhere to write that down, every run
// asks for all ninety thousand again.
//
// It is deliberately not a failure. A failure goes back into the queue on the
// next run and stops the folder's watermark advancing, which is right for a
// message that could not be copied and wrong for one that does not exist.
func (d *DB) MarkGone(ctx context.Context, folderID int64, srcUIDValidity, srcUID uint32) error {
	const upsert = `
INSERT INTO messages (folder_id, src_uidvalidity, src_uid, state, internaldate)
VALUES (?, ?, ?, ?, 0)
ON CONFLICT (folder_id, src_uidvalidity, src_uid) DO UPDATE SET
  state      = excluded.state,
  last_error = ''`

	if _, err := d.db.ExecContext(ctx, upsert, folderID, srcUIDValidity, srcUID, StateGone); err != nil {
		return fmt.Errorf("recording UID %d as absent from the source: %w", srcUID, err)
	}
	return nil
}

// Mirror is one copied message's place on both sides, together with the flags
// it carried when it was copied.
type Mirror struct {
	SrcUID uint32
	DstUID uint32
	Flags  string
}

// Ident pairs a source message with the digest that identifies it.
type Ident struct {
	SrcUID    uint32
	IdentHash string
}

// Idents returns what is known about the identity of each recorded source
// message in a folder.
//
// Deliberately not scoped by destination UIDVALIDITY, unlike Mirrored. This
// answers "what does the source hold", and the answer does not stop being true
// because the destination was renumbered. Scoping it would empty the set
// exactly when the destination is full of messages that need matching against
// it, and every one of them would look like a stranger.
func (d *DB) Idents(ctx context.Context, folderID int64, srcUIDValidity uint32) ([]Ident, error) {
	const query = `
SELECT src_uid, ident_hash FROM messages
WHERE folder_id = ? AND src_uidvalidity = ? AND state = ?`

	rows, err := d.db.QueryContext(ctx, query, folderID, srcUIDValidity, StateDone)
	if err != nil {
		return nil, fmt.Errorf("reading identities for folder %d: %w", folderID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Ident
	for rows.Next() {
		var id Ident
		if err := rows.Scan(&id.SrcUID, &id.IdentHash); err != nil {
			return nil, fmt.Errorf("reading identities for folder %d: %w", folderID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading identities for folder %d: %w", folderID, err)
	}
	return out, nil
}

// Mirrored returns the messages of a folder that are known to exist on both
// sides, with the destination UID that names each one.
//
// The destination UIDVALIDITY is part of the question rather than an
// afterthought. A dst_uid recorded under a different validity names a message
// that no longer exists, and a STORE against it would land on whatever occupies
// that number now — silently marking a stranger read. When the destination has
// been renumbered this returns nothing, which is the right answer: there is no
// mapping left to update through.
func (d *DB) Mirrored(ctx context.Context, folderID int64, srcUIDValidity, dstUIDValidity uint32) ([]Mirror, error) {
	const query = `
SELECT src_uid, dst_uid, flags FROM messages
WHERE folder_id = ? AND src_uidvalidity = ? AND dst_uidvalidity = ? AND state = ? AND dst_uid != 0`

	rows, err := d.db.QueryContext(ctx, query, folderID, srcUIDValidity, dstUIDValidity, StateDone)
	if err != nil {
		return nil, fmt.Errorf("reading the message map for folder %d: %w", folderID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Mirror
	for rows.Next() {
		var m Mirror
		if err := rows.Scan(&m.SrcUID, &m.DstUID, &m.Flags); err != nil {
			return nil, fmt.Errorf("reading the message map for folder %d: %w", folderID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the message map for folder %d: %w", folderID, err)
	}
	return out, nil
}

// RecordFlags remembers the flags a message now carries on the destination.
//
// This is written after the STORE, not before. The two orderings fail
// differently: recording first and crashing leaves the state claiming a change
// that never reached the server, and nothing would ever correct it. Recording
// afterwards and crashing costs one repeated STORE on the next run, which sets
// the flags to what they already are.
func (d *DB) RecordFlags(ctx context.Context, folderID int64, srcUIDValidity, srcUID uint32, flags string) error {
	const update = `
UPDATE messages SET flags = ?
WHERE folder_id = ? AND src_uidvalidity = ? AND src_uid = ?`

	res, err := d.db.ExecContext(ctx, update, flags, folderID, srcUIDValidity, srcUID)
	if err != nil {
		return fmt.Errorf("recording flags of UID %d: %w", srcUID, err)
	}
	return expectOneRow(res, srcUID)
}

// BeginAppend records a message as in flight and commits before returning.
//
// The commit must happen before the APPEND is issued. Recording afterwards
// would leave a crash in that window invisible, and the message would be
// appended a second time on the next run.
func (d *DB) BeginAppend(ctx context.Context, m Message) error {
	const upsert = `
INSERT INTO messages (
  folder_id, src_uidvalidity, src_uid, ident_hash, stamp_id, state, size, flags, internaldate
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (folder_id, src_uidvalidity, src_uid) DO UPDATE SET
  ident_hash   = excluded.ident_hash,
  stamp_id     = excluded.stamp_id,
  state        = excluded.state,
  size         = excluded.size,
  flags        = excluded.flags,
  internaldate = excluded.internaldate,
  last_error   = ''`

	_, err := d.db.ExecContext(ctx, upsert,
		m.FolderID, m.SrcUIDValidity, m.SrcUID, m.IdentHash, m.StampID,
		StateInFlight, m.Size, m.Flags, m.InternalDate.Unix(),
	)
	if err != nil {
		return fmt.Errorf("recording append of UID %d as in flight: %w", m.SrcUID, err)
	}
	return nil
}

// CompleteAppend records the destination UID an APPEND produced.
func (d *DB) CompleteAppend(ctx context.Context, folderID int64, srcUIDValidity, srcUID, dstUIDValidity, dstUID uint32) error {
	const update = `
UPDATE messages SET state = ?, dst_uidvalidity = ?, dst_uid = ?, last_error = ''
WHERE folder_id = ? AND src_uidvalidity = ? AND src_uid = ?`

	res, err := d.db.ExecContext(ctx, update, StateDone, dstUIDValidity, dstUID, folderID, srcUIDValidity, srcUID)
	if err != nil {
		return fmt.Errorf("recording completion of UID %d: %w", srcUID, err)
	}
	return expectOneRow(res, srcUID)
}

// FailAppend records that a copy was abandoned, keeping the reason for the
// operator rather than discarding it.
func (d *DB) FailAppend(ctx context.Context, folderID int64, srcUIDValidity, srcUID uint32, reason string) error {
	const update = `
UPDATE messages SET state = ?, last_error = ?
WHERE folder_id = ? AND src_uidvalidity = ? AND src_uid = ?`

	res, err := d.db.ExecContext(ctx, update, StateFailed, reason, folderID, srcUIDValidity, srcUID)
	if err != nil {
		return fmt.Errorf("recording failure of UID %d: %w", srcUID, err)
	}
	return expectOneRow(res, srcUID)
}

// InFlight returns every message whose APPEND outcome is unknown. On startup
// each one is a suspect: it may or may not have reached the destination.
func (d *DB) InFlight(ctx context.Context, folderID int64) ([]Message, error) {
	const query = `
SELECT src_uidvalidity, src_uid, ident_hash, stamp_id, size, flags, internaldate, last_error
FROM messages WHERE folder_id = ? AND state = ?
ORDER BY src_uid`

	rows, err := d.db.QueryContext(ctx, query, folderID, StateInFlight)
	if err != nil {
		return nil, fmt.Errorf("reading in-flight messages for folder %d: %w", folderID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Message
	for rows.Next() {
		m := Message{FolderID: folderID, State: StateInFlight}
		var internalDate int64
		if err := rows.Scan(&m.SrcUIDValidity, &m.SrcUID, &m.IdentHash, &m.StampID, &m.Size, &m.Flags, &internalDate, &m.LastError); err != nil {
			return nil, fmt.Errorf("reading in-flight messages for folder %d: %w", folderID, err)
		}
		if internalDate != 0 {
			m.InternalDate = time.Unix(internalDate, 0).UTC()
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading in-flight messages for folder %d: %w", folderID, err)
	}
	return out, nil
}

// Failures returns every abandoned copy with the reason it was abandoned, so a
// run can end by telling the operator what did not make it rather than only how
// many messages did not.
func (d *DB) Failures(ctx context.Context, folderID int64) ([]Message, error) {
	const query = `
SELECT src_uidvalidity, src_uid, ident_hash, size, last_error
FROM messages WHERE folder_id = ? AND state = ?
ORDER BY src_uid`

	rows, err := d.db.QueryContext(ctx, query, folderID, StateFailed)
	if err != nil {
		return nil, fmt.Errorf("reading failures for folder %d: %w", folderID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Message
	for rows.Next() {
		m := Message{FolderID: folderID, State: StateFailed}
		if err := rows.Scan(&m.SrcUIDValidity, &m.SrcUID, &m.IdentHash, &m.Size, &m.LastError); err != nil {
			return nil, fmt.Errorf("reading failures for folder %d: %w", folderID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading failures for folder %d: %w", folderID, err)
	}
	return out, nil
}

// Counts summarises a folder's rows by state, for progress and for status.
func (d *DB) Counts(ctx context.Context, folderID int64) (map[State]int, error) {
	const query = `SELECT state, COUNT(*) FROM messages WHERE folder_id = ? GROUP BY state`

	rows, err := d.db.QueryContext(ctx, query, folderID)
	if err != nil {
		return nil, fmt.Errorf("counting messages in folder %d: %w", folderID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[State]int)
	for rows.Next() {
		var (
			st State
			n  int
		)
		if err := rows.Scan(&st, &n); err != nil {
			return nil, fmt.Errorf("counting messages in folder %d: %w", folderID, err)
		}
		out[st] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counting messages in folder %d: %w", folderID, err)
	}
	return out, nil
}

// ErrNotRecorded means an update targeted a message the store has never seen,
// which indicates the caller skipped BeginAppend.
var ErrNotRecorded = errors.New("message not recorded")

func expectOneRow(res sql.Result, srcUID uint32) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking update of UID %d: %w", srcUID, err)
	}
	if n == 0 {
		return fmt.Errorf("UID %d: %w", srcUID, ErrNotRecorded)
	}
	return nil
}

// ForgetMessages drops the rows for messages that are no longer on either side.
//
// Once a message has been deleted from the destination because the source no
// longer lists it, the row describes nothing: the source UID has no message and
// the destination UID has been expunged. Leaving it would make the next run
// consider deleting an already-deleted message, and a UID the destination may
// eventually reissue under a new UIDVALIDITY.
func (d *DB) ForgetMessages(ctx context.Context, folderID int64, srcUIDValidity uint32, srcUIDs []uint32) error {
	if len(srcUIDs) == 0 {
		return nil
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning forget: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`DELETE FROM messages WHERE folder_id = ? AND src_uidvalidity = ? AND src_uid = ?`)
	if err != nil {
		return fmt.Errorf("preparing forget: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, uid := range srcUIDs {
		if _, err := stmt.ExecContext(ctx, folderID, srcUIDValidity, uid); err != nil {
			return fmt.Errorf("forgetting message %d: %w", uid, err)
		}
	}
	return tx.Commit()
}

// escapeDSNPath hides from SQLite's URI parser the three characters that mean
// something to it, so that a filesystem path is read as itself.
var escapeDSNPath = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace
