// Package syncer copies messages from one IMAP account to another.
//
// This is the M1 engine: one connection per side, folders in sequence, messages
// in sequence. It is deliberately not concurrent. Correctness here — what counts
// as already copied, what a crash leaves behind, what a UIDVALIDITY change
// means — is what the concurrent engine will be built on, and none of it gets
// easier to reason about with a hundred connections in flight.
package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hilli/imapsync-go/internal/folder"
	"github.com/hilli/imapsync-go/internal/ident"
	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/state"
)

// metaBatch is how many messages are described in one FETCH.
//
// The whole batch's headers are held in memory at once, so this trades round
// trips against footprint. iCloud's INBOX holds 414k messages (§6.3); fetching
// their metadata in one command would be neither.
const metaBatch = 500

// Options configures a run.
type Options struct {
	// PairID names this source/destination pairing in the state database, so
	// one database can hold several migrations without them colliding.
	PairID string

	// Folders controls how source mailboxes map onto destination ones. The
	// delimiters and prefixes are filled in from the servers when left unset.
	Folders folder.Options

	// DryRun reports what would be copied without writing anything to the
	// destination or to the state database.
	DryRun bool

	Logger *slog.Logger
}

// Syncer copies one account onto another.
type Syncer struct {
	src, dst imapx.Conn
	db       *state.DB
	opts     Options
	log      *slog.Logger
}

// New returns a syncer over two established connections.
func New(src, dst imapx.Conn, db *state.DB, opts Options) *Syncer {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.PairID == "" {
		opts.PairID = "default"
	}
	return &Syncer{src: src, dst: dst, db: db, opts: opts, log: log}
}

// Report is the outcome of a run.
type Report struct {
	Folders []FolderReport
	// Skips are source mailboxes the plan left out, with the reason.
	Skips []folder.Skip
	// Created are destination mailboxes this run brought into existence.
	Created []string
}

// FolderReport is the outcome for one mailbox.
type FolderReport struct {
	Source string
	Dest   string

	// Messages is how many the source holds.
	Messages int
	// Copied is how many this run appended to the destination.
	Copied int
	// Adopted is how many were found already present, either from a previous
	// run or from an append whose outcome was unknown until now.
	Adopted int
	// AlreadyDone is how many the state database had already recorded. On a
	// second run of an unchanged account this is everything.
	AlreadyDone int
	// Failed is how many were abandoned; see Errors.
	Failed int

	// Errors describes the abandoned copies, at most a few, for reporting.
	Errors []string
	// Err is a failure of the folder as a whole, which stops that folder but
	// not the run.
	Err error
}

// Totals sums the per-folder counts.
func (r Report) Totals() (copied, adopted, failed int) {
	for _, f := range r.Folders {
		copied += f.Copied
		adopted += f.Adopted
		failed += f.Failed
	}
	return copied, adopted, failed
}

// Run copies every mapped folder.
//
// A folder that fails is recorded and the run continues: one unreadable mailbox
// should not strand the rest of an account. Only a failure to plan the run at
// all returns an error.
func (s *Syncer) Run(ctx context.Context) (Report, error) {
	plan, err := s.plan(ctx)
	if err != nil {
		return Report{}, err
	}

	report := Report{Skips: plan.Skips}
	for _, name := range plan.Creates {
		if s.opts.DryRun {
			report.Created = append(report.Created, name)
			continue
		}
		if err := s.dst.CreateFolder(ctx, name); err != nil {
			return report, fmt.Errorf("creating destination folder %q: %w", name, err)
		}
		report.Created = append(report.Created, name)
		s.log.Info("created destination folder", "folder", name)
	}

	for _, pair := range plan.Pairs {
		if err := ctx.Err(); err != nil {
			return report, err
		}

		fr, err := s.syncFolder(ctx, pair)
		if err != nil {
			fr.Err = err
			s.log.Error("folder failed", "source", pair.Source, "dest", pair.Dest, "error", err)
		}
		report.Folders = append(report.Folders, fr)
	}
	return report, nil
}

// plan lists both sides and maps them onto each other.
func (s *Syncer) plan(ctx context.Context) (folder.Plan, error) {
	srcFolders, err := s.src.ListFolders(ctx, imapx.ListOptions{})
	if err != nil {
		return folder.Plan{}, fmt.Errorf("listing source folders: %w", err)
	}
	dstFolders, err := s.dst.ListFolders(ctx, imapx.ListOptions{})
	if err != nil {
		return folder.Plan{}, fmt.Errorf("listing destination folders: %w", err)
	}

	opts := s.opts.Folders
	if opts.SourceDelim == "" {
		opts.SourceDelim = s.delim(ctx, s.src, srcFolders)
	}
	if opts.DestDelim == "" {
		opts.DestDelim = s.delim(ctx, s.dst, dstFolders)
	}

	plan, err := folder.Build(srcFolders, dstFolders, opts)
	if err != nil {
		return folder.Plan{}, fmt.Errorf("planning folders: %w", err)
	}
	s.log.Info("planned run",
		"folders", len(plan.Pairs), "skipped", len(plan.Skips), "to_create", len(plan.Creates))
	return plan, nil
}

// delim finds a server's hierarchy delimiter, preferring what LIST reported.
//
// NAMESPACE is the fallback rather than the first choice because LIST answers
// per mailbox, and a server whose namespaces disagree with its own LIST output
// is likelier to be right in LIST.
func (s *Syncer) delim(ctx context.Context, c imapx.Conn, folders []imapx.Folder) string {
	for _, f := range folders {
		if f.Delim != "" {
			return f.Delim
		}
	}
	if ns, err := c.Namespaces(ctx); err == nil && ns.Delim != "" {
		return ns.Delim
	}
	return ""
}

// adoption is a destination folder indexed by message identity, used to
// recognise messages that are already there.
//
// It exists because the alternative does not scale: a SEARCH per source message
// is one round trip per message, and iCloud's INBOX holds 414k of them (§6.3).
// One bulk pass over the destination's headers costs the size of the
// destination, which on a genuine first sync is nothing at all.
//
// Digests map to a *list* of UIDs, and adopting consumes one. Two identical
// messages in the source are two messages, not one, and must end up as two at
// the destination.
type adoption map[string][]uint32

// take claims a destination message for a source identity.
//
// Weak identities are excluded when the index is built rather than here, so
// nothing thin enough to match the wrong message is ever in it.
func (a adoption) take(id ident.Identity) (uint32, bool) {
	uids := a[id.Digest]
	if len(uids) == 0 {
		return 0, false
	}
	a[id.Digest] = uids[1:]
	return uids[0], true
}

// indexDestination reads the destination folder's headers and digests them.
//
// A stamped copy digests the same as its unstamped source, because the stamp
// header is deliberately not part of the digest, so one index covers both
// stamped and unstamped messages.
func (s *Syncer) indexDestination(ctx context.Context, folderID int64, dst imapx.Mailbox) (adoption, error) {
	uids, err := s.dst.AllUIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerating destination messages: %w", err)
	}
	if len(uids) == 0 {
		return nil, nil
	}

	claimed, err := s.db.ClaimedDestUIDs(ctx, folderID, dst.UIDValidity)
	if err != nil {
		return nil, err
	}

	index := make(adoption)
	var indexed int
	for start := 0; start < len(uids); start += metaBatch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+metaBatch, len(uids))

		metas, err := s.dst.FetchMeta(ctx, uids[start:end], ident.Fields)
		if err != nil {
			return nil, fmt.Errorf("reading destination headers: %w", err)
		}
		for _, meta := range metas {
			if _, taken := claimed[meta.UID]; taken {
				continue
			}
			// A weak identity is one built from almost no header. It cannot
			// tell two messages apart, so a match on it would adopt an
			// unrelated message and drop this one — worse than the duplicate
			// that copying risks. Such messages are left out of the index
			// entirely, on both sides.
			id := ident.Parse(meta.Header)
			if id.Weak {
				continue
			}
			index[id.Digest] = append(index[id.Digest], meta.UID)
			indexed++
		}
	}

	s.log.Info("indexed destination folder for adoption",
		"folder", dst.Name, "messages", len(uids), "indexed", indexed, "already_claimed", len(claimed))
	return index, nil
}

// syncFolder copies one mailbox.
func (s *Syncer) syncFolder(ctx context.Context, pair folder.Pair) (FolderReport, error) {
	fr := FolderReport{Source: pair.Source, Dest: pair.Dest}

	src, err := s.src.Select(ctx, pair.Source, imapx.SelectOptions{ReadOnly: true})
	if err != nil {
		return fr, fmt.Errorf("selecting source mailbox: %w", err)
	}
	fr.Messages = int(src.NumMessages)

	if s.opts.DryRun {
		return s.dryRunFolder(ctx, pair, src, fr)
	}

	dst, err := s.dst.Select(ctx, pair.Dest, imapx.SelectOptions{})
	if err != nil {
		return fr, fmt.Errorf("selecting destination mailbox: %w", err)
	}
	if dst.ReadOnly {
		return fr, fmt.Errorf("destination mailbox %q is read-only", pair.Dest)
	}

	row, err := s.db.EnsureFolder(ctx, s.opts.PairID, pair.Source, pair.Dest)
	if err != nil {
		return fr, fmt.Errorf("recording folder: %w", err)
	}

	kept, err := s.db.FenceUIDValidity(ctx, row.ID, src.UIDValidity, dst.UIDValidity)
	if err != nil {
		return fr, fmt.Errorf("fencing UIDVALIDITY: %w", err)
	}
	if !kept {
		// Not an error: the server renumbered the mailbox, which it is entitled
		// to do. Every UID we hold now refers to a different message or to
		// none, so the run falls back to identity matching for this folder.
		s.log.Warn("UIDVALIDITY changed; falling back to identity matching",
			"source", pair.Source, "src_uidvalidity", src.UIDValidity, "dst_uidvalidity", dst.UIDValidity)
	}

	// Suspects first. An in-flight row may already exist on the destination,
	// and copying it again is exactly the duplication this tool exists to
	// avoid.
	adopted, err := s.recover(ctx, row.ID, src, dst)
	if err != nil {
		return fr, fmt.Errorf("recovering in-flight messages: %w", err)
	}
	fr.Adopted += adopted

	known, err := s.db.SyncedUIDs(ctx, row.ID, src.UIDValidity)
	if err != nil {
		return fr, fmt.Errorf("reading recorded messages: %w", err)
	}

	uids, err := s.src.AllUIDs(ctx)
	if err != nil {
		return fr, fmt.Errorf("enumerating source messages: %w", err)
	}

	var todo []uint32
	for _, uid := range uids {
		if known[uid] == state.StateDone {
			fr.AlreadyDone++
			continue
		}
		todo = append(todo, uid)
	}

	s.log.Info("folder diffed",
		"source", pair.Source, "dest", pair.Dest,
		"messages", len(uids), "to_copy", len(todo), "already_done", fr.AlreadyDone)

	// Tier 3 in bulk, and only when tier 1 has nothing to say about the folder
	// as a whole: a first sync onto a destination that is not empty, a sync
	// resumed before one ever completed, a lost database, or a UIDVALIDITY
	// change, which the fence has just turned into the same thing.
	//
	// When the folder has completed a sync before, the UID map answers for
	// everything except the in-flight suspects, and those were settled above by
	// a handful of targeted searches. Indexing then would mean reading every
	// header in a 400k-message folder (§6.3) to learn nothing.
	var index adoption
	if len(todo) > 0 && dst.NumMessages > 0 && (row.LastSync.IsZero() || !kept) {
		index, err = s.indexDestination(ctx, row.ID, dst)
		if err != nil {
			return fr, err
		}
	}

	for start := 0; start < len(todo); start += metaBatch {
		if err := ctx.Err(); err != nil {
			return fr, err
		}
		end := min(start+metaBatch, len(todo))

		metas, err := s.src.FetchMeta(ctx, todo[start:end], ident.Fields)
		if err != nil {
			return fr, fmt.Errorf("fetching message metadata: %w", err)
		}
		for _, meta := range metas {
			if err := s.copyOne(ctx, row.ID, pair.Dest, src, dst, meta, index, &fr); err != nil {
				return fr, err
			}
		}
	}

	if err := s.db.MarkSynced(ctx, row.ID, src.HighestModSeq, time.Now()); err != nil {
		return fr, fmt.Errorf("recording folder completion: %w", err)
	}
	return fr, nil
}

// dryRunFolder reports what a real run would copy, without writing anything.
func (s *Syncer) dryRunFolder(ctx context.Context, pair folder.Pair, src imapx.Mailbox, fr FolderReport) (FolderReport, error) {
	if pair.CreateDest {
		// The mailbox does not exist yet, so everything in the source is new
		// and there is no state to consult.
		fr.Copied = fr.Messages
		return fr, nil
	}

	row, err := s.db.EnsureFolder(ctx, s.opts.PairID, pair.Source, pair.Dest)
	if err != nil {
		return fr, fmt.Errorf("reading folder state: %w", err)
	}
	known, err := s.db.SyncedUIDs(ctx, row.ID, src.UIDValidity)
	if err != nil {
		return fr, fmt.Errorf("reading recorded messages: %w", err)
	}

	uids, err := s.src.AllUIDs(ctx)
	if err != nil {
		return fr, fmt.Errorf("enumerating source messages: %w", err)
	}
	for _, uid := range uids {
		if known[uid] == state.StateDone {
			fr.AlreadyDone++
		} else {
			fr.Copied++
		}
	}
	return fr, nil
}

// copyOne copies a single message, recording its progress before it is made.
//
// The ordering is the whole point: the state row is committed as in-flight
// *before* the APPEND is issued, so a crash at any instant leaves a row that
// says "this may or may not have landed" rather than no evidence at all.
func (s *Syncer) copyOne(
	ctx context.Context,
	folderID int64,
	dstName string,
	src, dst imapx.Mailbox,
	meta imapx.MessageMeta,
	index adoption,
	fr *FolderReport,
) error {
	id := ident.Parse(meta.Header)
	flags := copyableFlags(meta.Flags)

	row := state.Message{
		FolderID:       folderID,
		SrcUIDValidity: src.UIDValidity,
		SrcUID:         meta.UID,
		IdentHash:      id.Digest,
		Size:           meta.Size,
		Flags:          strings.Join(flags, " "),
		InternalDate:   meta.InternalDate,
	}
	if id.NeedsStamp() {
		row.StampID = id.StampValue()
	}

	if err := s.db.BeginAppend(ctx, row); err != nil {
		return fmt.Errorf("recording message %d as in flight: %w", meta.UID, err)
	}

	// Already there — from an earlier run whose database was lost, or from a
	// destination that was not empty to begin with. Record the mapping instead
	// of making a second copy.
	if dstUID, ok := index.take(id); ok {
		if err := s.db.CompleteAppend(ctx, folderID, src.UIDValidity, meta.UID, dst.UIDValidity, dstUID); err != nil {
			return fmt.Errorf("recording message %d as adopted: %w", meta.UID, err)
		}
		fr.Adopted++
		return nil
	}

	// Spooled to memory. That is defensible at one connection and one message
	// at a time; the concurrent engine needs the byte-budget semaphore of §4.3
	// before it can do the same across hundreds.
	var buf bytes.Buffer
	if row.StampID != "" {
		buf.Write(ident.StampBytes(row.StampID))
	}
	stampLen := int64(buf.Len())

	n, err := s.src.FetchBody(ctx, meta.UID, &buf)
	switch {
	case errors.Is(err, imapx.ErrMessageGone):
		// Expunged between enumeration and fetch. Nothing was appended, and
		// nothing can be: record it so the next run does not retry forever.
		return s.fail(ctx, folderID, src.UIDValidity, meta.UID, "source message expunged before it could be read", fr)
	case err != nil:
		return fmt.Errorf("fetching message %d: %w", meta.UID, err)
	}

	// The APPEND literal is sized from what was actually read, never from
	// RFC822.SIZE: a short literal cannot be retracted and desynchronises the
	// connection (§3.7).
	res, err := s.dst.Append(ctx, dstName, imapx.AppendMessage{
		Size:         n + stampLen,
		Flags:        flags,
		InternalDate: meta.InternalDate,
		Body:         bytes.NewReader(buf.Bytes()),
	})
	switch {
	case errors.Is(err, imapx.ErrConnectionBroken):
		return fmt.Errorf("appending message %d: %w", meta.UID, err)
	case err != nil:
		// The server refused this message — oversized, rejected by policy, or
		// malformed beyond what it will store. The connection is intact, so the
		// rest of the folder can still be copied.
		return s.fail(ctx, folderID, src.UIDValidity, meta.UID, err.Error(), fr)
	}

	dstUIDValidity, dstUID := res.UIDValidity, res.UID
	if !res.Assigned() {
		// No UIDPLUS. The append succeeded — the tagged OK says so — but the
		// destination UID has to be found, and may not be findable at all.
		dstUIDValidity = dst.UIDValidity
		dstUID = s.locate(ctx, id, row.StampID != "")
	}

	if err := s.db.CompleteAppend(ctx, folderID, src.UIDValidity, meta.UID, dstUIDValidity, dstUID); err != nil {
		return fmt.Errorf("recording message %d as copied: %w", meta.UID, err)
	}
	fr.Copied++
	return nil
}

// fail records an abandoned copy and keeps the run going.
func (s *Syncer) fail(ctx context.Context, folderID int64, uidValidity, uid uint32, reason string, fr *FolderReport) error {
	if err := s.db.FailAppend(ctx, folderID, uidValidity, uid, reason); err != nil {
		return fmt.Errorf("recording message %d as failed: %w", uid, err)
	}
	fr.Failed++
	if len(fr.Errors) < 10 {
		fr.Errors = append(fr.Errors, fmt.Sprintf("uid %d: %s", uid, reason))
	}
	s.log.Warn("message not copied", "uid", uid, "reason", reason)
	return nil
}

// recover settles every message whose APPEND outcome is unknown.
//
// Each in-flight row is a suspect, not a failure: the process may have died
// between the commit and the append, or between the append and its commit. The
// only way to tell is to look. Anything not settled here stays in flight and is
// retried by the ordinary diff, which is safe because a suspect that is not on
// the destination has to be copied anyway.
func (s *Syncer) recover(ctx context.Context, folderID int64, src, dst imapx.Mailbox) (int, error) {
	suspects, err := s.db.InFlight(ctx, folderID)
	if err != nil {
		return 0, err
	}
	if len(suspects) == 0 {
		return 0, nil
	}
	s.log.Info("settling messages left in flight", "count", len(suspects), "folder", src.Name)

	var adopted int
	for _, m := range suspects {
		if err := ctx.Err(); err != nil {
			return adopted, err
		}

		id, ok, err := s.identify(ctx, m)
		if err != nil {
			return adopted, err
		}
		if !ok {
			// The source message is gone, so it cannot be identified and the
			// destination cannot be searched for it. Leaving the row in flight
			// would make every future run repeat this, so it is settled here.
			if err := s.db.FailAppend(ctx, folderID, m.SrcUIDValidity, m.SrcUID,
				"source message expunged while its copy was in flight; destination state unknown"); err != nil {
				return adopted, err
			}
			continue
		}

		uid := s.locate(ctx, id, m.StampID != "")
		if uid == 0 {
			continue // never landed, or cannot be searched for: retry the copy
		}
		if err := s.db.CompleteAppend(ctx, folderID, m.SrcUIDValidity, m.SrcUID, dst.UIDValidity, uid); err != nil {
			return adopted, err
		}
		adopted++
	}

	s.log.Info("in-flight messages settled", "adopted", adopted, "retrying", len(suspects)-adopted)
	return adopted, nil
}

// identify re-derives a suspect's identity from the source.
//
// The stored row holds the digest but not the Message-ID it may be searchable
// by, and re-reading one header is cheaper than carrying a column that only
// recovery would ever read. The set is bounded by how many appends were in
// flight when the process died.
func (s *Syncer) identify(ctx context.Context, m state.Message) (ident.Identity, bool, error) {
	metas, err := s.src.FetchMeta(ctx, []uint32{m.SrcUID}, ident.Fields)
	if err != nil {
		if errors.Is(err, imapx.ErrMessageGone) {
			return ident.Identity{}, false, nil
		}
		return ident.Identity{}, false, fmt.Errorf("re-reading in-flight message %d: %w", m.SrcUID, err)
	}
	if len(metas) == 0 {
		return ident.Identity{}, false, nil
	}
	return ident.Parse(metas[0].Header), true, nil
}

// locate finds a message on the destination, returning 0 when it cannot.
//
// A failure to find is not an error: the commonest reason is that the message
// genuinely is not there. Nor is a search failure, which costs a re-copy rather
// than a lost message.
func (s *Syncer) locate(ctx context.Context, id ident.Identity, stamped bool) uint32 {
	// Weak identities are never acted on, stamp or no stamp: the stamp is the
	// digest, so a digest too thin to distinguish two messages makes a stamp
	// that is too thin as well.
	if id.Weak {
		return 0
	}
	field, value, ok := ident.SearchTerms(id, stamped)
	if !ok {
		return 0
	}

	uids, err := s.dst.SearchHeader(ctx, field, value)
	if err != nil {
		s.log.Warn("destination search failed", "field", field, "error", err)
		return 0
	}
	if len(uids) == 0 {
		return 0
	}

	// Several matches means the destination already held copies of this
	// message before the run. The newest is the one just appended.
	var highest uint32
	for _, uid := range uids {
		highest = max(highest, uid)
	}
	return highest
}

// copyableFlags drops the flags that belong to the server rather than to the
// message.
//
// \Recent is per-session and assigned by the destination; sending it back is
// meaningless at best and rejected at worst.
func copyableFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		if strings.EqualFold(f, "\\Recent") {
			continue
		}
		out = append(out, f)
	}
	return out
}
