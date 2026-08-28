// Package syncer copies messages from one IMAP account to another.
//
// Correctness is decided the same way whether one connection is in flight or a
// hundred: what counts as already copied, what a crash leaves behind, and what
// a UIDVALIDITY change means are all properties of the state database, not of
// the connection that happened to observe them.
//
// The concurrency is shaped to keep it that way. Work moves through two stages
// that never overlap in what they hold: fetch workers lease a source
// connection and nothing else, append workers lease a destination connection
// and nothing else, and a channel of fetched bodies joins them. Because no
// goroutine ever holds a connection to both accounts at once, the two pools
// cannot deadlock against each other however they are sized — which matters,
// because they are deliberately sized very differently (§4.1).
package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hilli/imapsync-go/internal/budget"
	"github.com/hilli/imapsync-go/internal/folder"
	"github.com/hilli/imapsync-go/internal/ident"
	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/pool"
	"github.com/hilli/imapsync-go/internal/retry"
	"github.com/hilli/imapsync-go/internal/state"
)

// metaBatch is how many messages are described in one FETCH.
//
// The whole batch's headers are held in memory at once, so this trades round
// trips against footprint. iCloud's INBOX holds 414k messages (§6.3); fetching
// their metadata in one command would be neither.
const metaBatch = 500

// stateWriteGrace bounds a write that records work already done on the server,
// which has to run even when the run itself has been cancelled.
const stateWriteGrace = 10 * time.Second

// copyChunk is how many messages one fetch worker claims at a time.
//
// Work is handed out in chunks rather than split up front because message sizes
// vary by orders of magnitude: a static partition gives one worker a run of
// 50 MiB attachments and leaves everyone waiting for it. A worker that draws a
// heavy chunk simply draws fewer chunks.
//
// The size is a compromise between two costs that pull in opposite directions.
// Large chunks amortise the one metadata FETCH that opens each chunk over more
// bodies. Small chunks divide the folder more finely, and the number of workers
// that can share a folder is the number of chunks in it — at a thousand a
// folder of nine hundred messages would be copied by exactly one connection.
//
// Fifty puts the metadata FETCH at one round trip in fifty-one, and a few per
// cent of the bytes, while letting a folder of a few hundred messages spread
// across every connection there is.
const copyChunk = 50

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

	// Retry is how hard the run tries again when the network or the server
	// misbehaves. The zero value means retry.Default.
	Retry retry.Policy

	// GiveUpAfter is how many failures in a row, with nothing copied in
	// between, end the run. Zero means the default.
	//
	// It exists because retrying is only worth doing when something might
	// still succeed. A destination that has stopped accepting mail fails every
	// message identically, and without a ceiling the run would spend hours
	// discovering that one message at a time.
	GiveUpAfter int

	// Full ignores the CONDSTORE fast path and diffs every folder, however
	// certain the source is that nothing has changed.
	Full bool

	// NoResyncFlags turns off copying flag changes onto messages that are
	// already there. Negative, so that the zero Options is the default
	// behaviour: imapsync resynchronises flags unless told not to, and a field
	// whose zero value disagreed with the product's default would be a bug
	// waiting for someone to construct Options without thinking about it.
	NoResyncFlags bool

	// Delete2 removes destination messages whose source counterpart is gone.
	//
	// It only ever touches messages this tool copied and recorded. Mail that
	// was on the destination before the first sync has no row in the state
	// database, so it is not a candidate however long it sits there — which is
	// narrower than imapsync's --delete2, deliberately.
	Delete2 bool

	// DeleteCeiling is the fraction of a folder's recorded messages that may be
	// deleted in one run before the run refuses and asks to be told again.
	// Zero means defaultDeleteCeiling.
	//
	// The failure it exists to catch is a source that answers a UID listing
	// with nothing, or with a fraction of the truth. Copying nothing is
	// harmless and self-correcting; deleting everything is neither.
	DeleteCeiling float64

	// Force carries out deletions the ceiling would otherwise refuse.
	Force bool

	// ProgressEvery is how often the run says what it has done so far. Zero
	// silences it, which is the right default for a caller that has its own
	// idea of what to report; the command line sets its own interval.
	ProgressEvery time.Duration

	Logger *slog.Logger
}

// Syncer copies one account onto another.
type Syncer struct {
	src, dst *pool.Pool
	bytes    *budget.Budget
	db       *state.DB
	opts     Options
	log      *slog.Logger
}

// New returns a syncer over two connection pools.
//
// bytes bounds how much message data may be in memory at once and may be nil
// for no limit, in which case the pool sizes are the only bound.
func New(src, dst *pool.Pool, db *state.DB, bytes *budget.Budget, opts Options) *Syncer {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.PairID == "" {
		opts.PairID = "default"
	}
	if opts.Retry.Attempts == 0 {
		opts.Retry = retry.Default()
	}
	if opts.GiveUpAfter == 0 {
		opts.GiveUpAfter = 50
	}
	return &Syncer{src: src, dst: dst, bytes: bytes, db: db, opts: opts, log: log}
}

// Report is the outcome of a run.
type Report struct {
	Folders []FolderReport
	// Skips are source mailboxes the plan left out, with the reason.
	Skips []folder.Skip
	// Created are destination mailboxes this run brought into existence.
	Created []string
}

// replace swaps in a later attempt at a folder already reported on.
func (r *Report) replace(fr FolderReport) {
	for i := range r.Folders {
		if r.Folders[i].Source == fr.Source {
			// The counts a folder accumulated before it failed are still true,
			// and the second attempt starts from what the state database
			// already records, so its own AlreadyDone covers them. Only the
			// error is worth carrying over when it is still there.
			r.Folders[i] = fr
			return
		}
	}
	r.Folders = append(r.Folders, fr)
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
	// Vanished is how many UIDs the source listed and then had no message for.
	// Not a failure: there is nothing at that number to copy, and there never
	// will be.
	Vanished int
	// Reflagged is how many had their flags brought back into line on the
	// destination without being copied again.
	Reflagged int
	// Deleted is how many were removed from the destination because the source
	// no longer has them.
	Deleted int
	// Refused is how many deletions the safety ceiling declined to carry out.
	// They are not failures: nothing went wrong, and the run is asking to be
	// told again rather than guessing.
	Refused int
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

// Vanished sums the UIDs the sources listed and had no message for.
//
// Reported separately because it is neither work done nor work failed, and
// because on some servers it is large enough that leaving it out makes the
// other numbers look like loss.
func (r Report) Vanished() int {
	n := 0
	for _, f := range r.Folders {
		n += f.Vanished
	}
	return n
}

// Deleted is how many messages the run removed from the destination.
func (r Report) Deleted() int {
	n := 0
	for _, f := range r.Folders {
		n += f.Deleted
	}
	return n
}

// Refused is how many deletions the safety ceiling declined to carry out.
func (r Report) Refused() int {
	n := 0
	for _, f := range r.Folders {
		n += f.Refused
	}
	return n
}

// health tells the difference between a run meeting the ordinary friction of a
// long copy and a run shouting into a void.
//
// Retrying assumes something might still succeed. When a destination stops
// accepting mail every message fails identically, and each one costs its full
// allowance of attempts and backoff. Counting failures that have nothing
// between them puts a bound on how long that can go on: a run that has failed
// fifty times in a row without copying anything is not going to be rescued by
// the fifty-first.
//
// The counter is shared by every folder in flight rather than kept per folder,
// because the thing being detected is a property of the server, not of a
// mailbox. Per-folder counters would each have to fill up separately before the
// run wound down.
type health struct {
	ceiling int64
	inARow  atomic.Int64
	stop    context.CancelCauseFunc

	// copied, adopted and folders are run-wide totals, for the progress the
	// run prints as it goes. They live here because this is already the one
	// object every folder shares and every success reports to, and because
	// what has been copied lately is the same signal the ceiling watches.
	copied  atomic.Int64
	adopted atomic.Int64
	folders atomic.Int64

	mu     sync.Mutex
	reason error
}

// progress notes that something worked, which is what makes the count
// consecutive rather than cumulative. Scattered failures against a working
// server never accumulate.
func (h *health) progress(copied, adopted int) {
	if copied == 0 && adopted == 0 {
		return
	}
	h.copied.Add(int64(copied))
	h.adopted.Add(int64(adopted))
	h.inARow.Store(0)
}

// trouble notes a failure and ends the run if there have been too many with
// nothing in between.
func (h *health) trouble(err error) {
	if h.inARow.Add(1) < h.ceiling {
		return
	}

	h.mu.Lock()
	if h.reason == nil {
		h.reason = fmt.Errorf("stopping after %d failures in a row with nothing copied in between; the last was: %w",
			h.ceiling, err)
	}
	reason := h.reason
	h.mu.Unlock()

	h.stop(reason)
}

// verdict is the reason the run was ended, or nil if it was not.
func (h *health) verdict() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason
}

// Run copies every mapped folder.
//
// A folder that fails is recorded and the run continues: one unreadable mailbox
// should not strand the rest of an account. Only a failure to plan the run at
// all, or cancellation, returns an error.
func (s *Syncer) Run(ctx context.Context) (Report, error) {
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	hp := &health{ceiling: int64(s.opts.GiveUpAfter), stop: stop}

	plan, err := s.plan(ctx)
	if err != nil {
		return Report{}, err
	}

	report := Report{Skips: plan.Skips}
	if err := s.createFolders(ctx, plan.Creates, &report); err != nil {
		return report, err
	}

	// As many folders at once as there are source connections, which is as many
	// as can make progress: a folder beyond that would only queue for one.
	//
	// Both levels of concurrency are needed and neither substitutes for the
	// other. iCloud's INBOX holds 53% of that account (§6.3), so splitting the
	// work only by folder leaves one worker with half the job, while splitting
	// it only within a folder leaves 143 smaller mailboxes to trickle through
	// one at a time.
	//
	// The group carries no errors of its own: a folder failure belongs in that
	// folder's report, not in a cancellation that would abandon every other
	// folder mid-copy. Cancellation comes from the caller's context, which
	// every worker already watches.
	done := s.announce(ctx, hp, len(plan.Pairs))
	report.Folders = s.pass(ctx, plan.Pairs, hp)

	// One more go at whatever failed. A folder is abandoned for reasons that
	// are usually gone by the time the rest of the run has finished — a server
	// that was restarting, a mailbox that was locked, a burst of throttling —
	// and by now the pools have been rebuilt around fresh connections and the
	// load that provoked it has stopped. It costs one attempt on the folders
	// that failed, and saves running the whole thing again to catch them.
	if again := retryable(report.Folders); len(again) > 0 && ctx.Err() == nil {
		s.log.Info("retrying folders that failed", "folders", len(again))
		for _, fr := range s.pass(ctx, again, hp) {
			report.replace(fr)
		}
	}
	done()

	// Folders finish in whatever order their sizes dictate. Sorting restores
	// the plan's order so that a report reads the same way twice.
	slices.SortFunc(report.Folders, func(a, b FolderReport) int {
		return strings.Compare(a.Source, b.Source)
	})

	// The run's own verdict outranks the cancellation it caused, which would
	// otherwise be reported as though the caller had asked for it.
	if err := hp.verdict(); err != nil {
		return report, err
	}
	return report, ctx.Err()
}

// announce reports what the run has done so far, at intervals, and returns the
// function that stops it.
//
// A sync of this size runs for hours, and without this it says nothing between
// folders — and the largest folder in the account holds half of it, so that
// silence can last most of the run. This is not a progress bar and does not try
// to be one: no total is known until the last folder is diffed, and the useful
// question during a long run is not "how far along" but "is it still moving".
func (s *Syncer) announce(ctx context.Context, hp *health, folders int) func() {
	if s.opts.ProgressEvery <= 0 {
		return func() {}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	start := time.Now()

	go func() {
		defer close(done)
		t := time.NewTicker(s.opts.ProgressEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				elapsed := time.Since(start)
				copied := hp.copied.Load()
				s.log.Info("still going",
					"folders", fmt.Sprintf("%d/%d", hp.folders.Load(), folders),
					"copied", copied,
					"adopted", hp.adopted.Load(),
					"rate", fmt.Sprintf("%.1f msg/s", float64(copied)/elapsed.Seconds()),
					// A duration is rendered as a bare nanosecond count by
					// the JSON handler, which is unreadable in exactly the
					// place this line is meant to be read.
					"elapsed", elapsed.Round(time.Second).String())
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

// pass copies a set of folders, as many at once as the source pool is wide.
//
// The group carries no errors of its own: a folder failure belongs in that
// folder's report, not in a cancellation that would abandon every other folder
// mid-copy. Cancellation comes from the context, which every worker watches.
func (s *Syncer) pass(ctx context.Context, pairs []folder.Pair, hp *health) []FolderReport {
	var mu sync.Mutex
	out := make([]FolderReport, 0, len(pairs))

	var g errgroup.Group
	g.SetLimit(s.src.Cap())
	for _, pair := range pairs {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			fr, err := s.syncFolder(ctx, pair, hp)
			if err != nil {
				fr.Err = err
				s.log.Error("folder failed", "source", pair.Source, "dest", pair.Dest, "error", err)
			}
			mu.Lock()
			out = append(out, fr)
			mu.Unlock()
			hp.folders.Add(1)
			return nil
		})
	}
	_ = g.Wait()
	return out
}

// retryable is the folders worth a second attempt.
//
// A folder that was cut short by the run being cancelled or given up on is not
// among them: whatever stopped it is still true, and asking again would only
// produce the same error more slowly.
func retryable(reports []FolderReport) []folder.Pair {
	var out []folder.Pair
	for _, fr := range reports {
		if fr.Err == nil || errors.Is(fr.Err, context.Canceled) || errors.Is(fr.Err, context.DeadlineExceeded) {
			continue
		}
		out = append(out, folder.Pair{Source: fr.Source, Dest: fr.Dest})
	}
	return out
}

// createFolders brings the missing destination mailboxes into existence.
//
// This runs before any copying and on one connection: the mailboxes are created
// in hierarchy order, and a server that must materialise a parent before its
// child will not do so reliably if asked for both at once.
func (s *Syncer) createFolders(ctx context.Context, names []string, report *Report) error {
	if len(names) == 0 {
		return nil
	}
	if s.opts.DryRun {
		report.Created = append(report.Created, names...)
		return nil
	}

	lease, err := s.dst.Acquire(ctx, "")
	if err != nil {
		return fmt.Errorf("acquiring destination connection: %w", err)
	}
	defer func() { lease.Release(err) }()

	for _, name := range names {
		if err = lease.Conn().CreateFolder(ctx, name); err != nil {
			return fmt.Errorf("creating destination folder %q: %w", name, err)
		}
		report.Created = append(report.Created, name)
		s.log.Info("created destination folder", "folder", name)
	}
	return nil
}

// plan lists both sides and maps them onto each other.
func (s *Syncer) plan(ctx context.Context) (folder.Plan, error) {
	srcFolders, srcDelim, err := s.list(ctx, s.src)
	if err != nil {
		return folder.Plan{}, fmt.Errorf("listing source folders: %w", err)
	}
	dstFolders, dstDelim, err := s.list(ctx, s.dst)
	if err != nil {
		return folder.Plan{}, fmt.Errorf("listing destination folders: %w", err)
	}

	opts := s.opts.Folders
	if opts.SourceDelim == "" {
		opts.SourceDelim = srcDelim
	}
	if opts.DestDelim == "" {
		opts.DestDelim = dstDelim
	}

	plan, err := folder.Build(srcFolders, dstFolders, opts)
	if err != nil {
		return folder.Plan{}, fmt.Errorf("planning folders: %w", err)
	}
	s.log.Info("planned run",
		"folders", len(plan.Pairs), "skipped", len(plan.Skips), "to_create", len(plan.Creates))
	return plan, nil
}

// list enumerates one account's mailboxes and settles its hierarchy delimiter.
func (s *Syncer) list(ctx context.Context, p *pool.Pool) (folders []imapx.Folder, delim string, err error) {
	lease, err := p.Acquire(ctx, "")
	if err != nil {
		return nil, "", fmt.Errorf("acquiring connection: %w", err)
	}
	defer func() { lease.Release(err) }()

	folders, err = lease.Conn().ListFolders(ctx, imapx.ListOptions{})
	if err != nil {
		return nil, "", err
	}
	return folders, s.delim(ctx, lease.Conn(), folders), nil
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
func (s *Syncer) indexDestination(ctx context.Context, dst imapx.Conn, folderID int64, box imapx.Mailbox) (adoption, error) {
	uids, err := dst.AllUIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerating destination messages: %w", err)
	}
	if len(uids) == 0 {
		return nil, nil
	}

	claimed, err := s.db.ClaimedDestUIDs(ctx, folderID, box.UIDValidity)
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

		metas, err := dst.FetchMeta(ctx, uids[start:end], ident.Fields)
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
		"folder", box.Name, "messages", len(uids), "indexed", indexed, "already_claimed", len(claimed))
	return index, nil
}

// live is the mutable state a folder's workers share.
//
// One lock covers both the adoption index and the counters because they are
// touched together and neither is held for longer than a map operation. The
// contended work — fetching and appending — happens outside it.
type live struct {
	mu     sync.Mutex
	report FolderReport
	index  adoption

	// health is shared with every other folder in the run.
	health *health
}

// adopt claims a destination message for a source identity, if one is free.
func (l *live) adopt(id ident.Identity) (uint32, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.index.take(id)
}

func (l *live) copied() {
	l.mu.Lock()
	l.report.Copied++
	l.mu.Unlock()
	l.health.progress(1, 0)
}

func (l *live) adopted(n int) {
	l.mu.Lock()
	l.report.Adopted += n
	l.mu.Unlock()
	l.health.progress(0, n)
}

func (l *live) vanished(n int) {
	l.mu.Lock()
	l.report.Vanished += n
	l.mu.Unlock()
}

func (l *live) deleted(n int) {
	l.mu.Lock()
	l.report.Deleted += n
	l.mu.Unlock()
}

func (l *live) refused(n int) {
	l.mu.Lock()
	l.report.Refused += n
	l.mu.Unlock()
}

func (l *live) reflagged() {
	l.mu.Lock()
	l.report.Reflagged++
	l.mu.Unlock()
}

// failed records an abandoned message. Only the first few reasons are kept:
// a folder that fails ten thousand times has one problem, not ten thousand.
func (l *live) failed(uid uint32, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.report.Failed++
	if len(l.report.Errors) < 10 {
		l.report.Errors = append(l.report.Errors, fmt.Sprintf("uid %d: %s", uid, reason))
	}
}

// snapshot copies the report out, safe to read once the workers have stopped
// and while they are still running.
func (l *live) snapshot() FolderReport {
	l.mu.Lock()
	defer l.mu.Unlock()
	fr := l.report
	fr.Errors = slices.Clone(fr.Errors)
	return fr
}

// prepared is everything the copy stage needs that the setup stage worked out.
type prepared struct {
	folderID int64
	src, dst imapx.Mailbox
	// todo is the source UIDs still to copy, in the order the server gave them.
	todo []uint32
	// since is the modification sequence the last completed sync earned, which
	// is what a flag delta is asked for relative to. Zero means ask for
	// everything.
	since uint64
	// skipped means the fast path recognised the folder and nothing below the
	// SELECT ran, so there is no destination mailbox here — and, because there
	// is none, no lease on it either. It is a barrier rather than an
	// optimisation: no test can distinguish it today, since the zero
	// destination mailbox already makes the flag resync find no mapping to work
	// through. What it prevents is a later change to that query turning a
	// skipped folder into STOREs issued on a connection that never selected the
	// mailbox.
	skipped bool
	// live is the source UIDs as the server listed them, sorted, kept only when
	// deletion is on. Deletion asks the opposite question from copying — not
	// "what is here that we lack" but "what have we got that is no longer
	// there" — and that needs the listing itself, not the difference.
	live []uint32
	// mirror is the message map, loaded at most once. Both the flag resync and
	// the deletion pass want it, and on a folder of 414,000 messages reading it
	// twice is a cost worth one field.
	mirror []state.Mirror
	loaded bool
}

// mirrored reads the folder's message map, once.
func (s *Syncer) mirrored(ctx context.Context, p *prepared) ([]state.Mirror, error) {
	if p.loaded {
		return p.mirror, nil
	}
	rows, err := s.db.Mirrored(ctx, p.folderID, p.src.UIDValidity, p.dst.UIDValidity)
	if err != nil {
		return nil, err
	}
	p.mirror, p.loaded = rows, true
	return rows, nil
}

// syncFolder copies one mailbox.
func (s *Syncer) syncFolder(ctx context.Context, pair folder.Pair, hp *health) (FolderReport, error) {
	if s.opts.DryRun {
		return s.dryRunFolder(ctx, pair)
	}

	lv := &live{report: FolderReport{Source: pair.Source, Dest: pair.Dest}, health: hp}
	p, err := s.prepareFolder(ctx, pair, lv)
	if err != nil {
		return lv.snapshot(), err
	}
	// A folder the fast path recognised arrives here with nothing to copy, and
	// copyFolder is a no-op on an empty list, so the two paths need no
	// distinguishing beyond that.
	if err := s.copyFolder(ctx, pair, p, lv); err != nil {
		return lv.snapshot(), err
	}

	if err := s.resyncFlags(ctx, pair, p, lv); err != nil {
		return lv.snapshot(), err
	}

	if err := s.deleteVanished(ctx, pair, p, lv); err != nil {
		return lv.snapshot(), err
	}

	// The stored modseq is what the next run's fast path trusts, and it means
	// one specific thing: everything the source held at that point is on the
	// destination. Advancing it after a folder that left messages behind would
	// turn a failure into a tombstone — the folder would be skipped from then
	// on and those messages would never be tried again. So a folder that
	// failed anything keeps its old watermark and gets diffed properly next
	// time.
	fr := lv.snapshot()
	modseq := p.src.HighestModSeq
	if fr.Failed > 0 {
		modseq = 0
	}
	// Deletions are watermarked separately, because they are a separate
	// promise. A run without --delete2 keeps the folder's copies current and
	// advances the modseq, but leaves every deletion the source made
	// uncarried-out — and if that advanced watermark were taken to cover
	// deletion too, the next --delete2 run would find the folder unchanged,
	// skip it, and lose those deletions for good.
	deleted := uint64(0)
	if s.opts.Delete2 && fr.Failed == 0 && fr.Refused == 0 {
		deleted = p.src.HighestModSeq
	}
	if err := s.db.MarkSynced(ctx, p.folderID, modseq, deleted, time.Now()); err != nil {
		return lv.snapshot(), fmt.Errorf("recording folder completion: %w", err)
	}
	return fr, nil
}

// watermark is the modification sequence a flag delta can be asked for relative
// to.
//
// A sequence only means anything within one UIDVALIDITY. After a renumbering
// the stored one describes a mailbox the server no longer has, and asking what
// changed since it invites an answer that is wrong in the quiet direction:
// nothing comes back, so nothing is updated, and the flags the fence has just
// made the state forget stay forgotten.
func watermark(row state.Folder, kept bool) uint64 {
	if !kept {
		return 0
	}
	return row.SrcHighestModSeq
}

// resyncFlags brings the destination's flags back into line with the source's
// for messages that are already copied.
//
// imapsync does this by default, and the reason is that a mailbox is not just
// its messages: read, answered and flagged are the state a person built up over
// years, and a mirror that loses them has lost something that cannot be
// recomputed. So the default is on.
//
// The expensive part is finding out what changed. Asking a 414k-message INBOX
// for every flag on every run, to learn that three of them moved, is the cost
// CONDSTORE removes: with a watermark from the last completed sync the server
// is asked only for what has changed since, and on a quiet folder that is an
// empty response. Without CONDSTORE the fallback is the full enumeration, which
// is what imapsync has always paid.
//
// Note what does not reach here at all: a folder the fast path recognised. An
// unchanged modification sequence means no flag moved either, so the cheapest
// flag resync is the one that is never started.
func (s *Syncer) resyncFlags(ctx context.Context, pair folder.Pair, p *prepared, lv *live) (err error) {
	if s.opts.NoResyncFlags || p.skipped {
		return nil
	}

	// The destination UIDVALIDITY is part of this question. If it moved, every
	// dst_uid we hold names a message that no longer exists, and a STORE
	// against one would land on whatever occupies that number now. Mirrored
	// returns nothing in that case, which ends the work here.
	mirrors, err := s.mirrored(ctx, p)
	if err != nil {
		return err
	}
	if len(mirrors) == 0 {
		return nil
	}
	byUID := make(map[uint32]state.Mirror, len(mirrors))
	for _, m := range mirrors {
		byUID[m.SrcUID] = m
	}

	current, err := s.sourceFlags(ctx, pair.Source, p.since)
	if err != nil {
		return err
	}

	type change struct {
		srcUID, dstUID uint32
		flags          []string
		text           string
	}
	var changes []change
	for _, fs := range current {
		m, ok := byUID[fs.UID]
		if !ok {
			continue
		}
		flags := copyableFlags(fs.Flags)
		text := flagText(flags)
		if text == flagText(strings.Fields(m.Flags)) {
			continue
		}
		changes = append(changes, change{srcUID: fs.UID, dstUID: m.DstUID, flags: flags, text: text})
	}
	if len(changes) == 0 {
		return nil
	}

	// One destination connection for the whole batch, held across the STOREs.
	// STORE needs the mailbox selected, unlike APPEND, so this cannot borrow
	// the pool per message the way the copy path does. Serial is deliberate:
	// the delta a CONDSTORE server hands back is normally a handful of
	// messages, and spending several connections on it would take them from
	// folders that have real work.
	lease, err := s.dst.Acquire(ctx, pair.Dest)
	if err != nil {
		return fmt.Errorf("acquiring destination connection: %w", err)
	}
	defer func() { lease.Release(err) }()
	dst := lease.Conn()

	if _, err = dst.Select(ctx, pair.Dest, imapx.SelectOptions{}); err != nil {
		return fmt.Errorf("selecting destination mailbox: %w", err)
	}

	for _, c := range changes {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = dst.StoreFlags(ctx, c.dstUID, c.flags); err != nil {
			if errors.Is(err, imapx.ErrConnectionBroken) {
				return fmt.Errorf("storing flags on UID %d: %w", c.dstUID, err)
			}
			// One message's flags failing is not the folder's problem, but it
			// must count: a folder that failed anything keeps its old
			// watermark, so this will be tried again rather than lost.
			lv.failed(c.srcUID, fmt.Sprintf("storing flags: %v", err))
			err = nil
			continue
		}
		// Detached for the same reason the copy path detaches: the flags are
		// on the destination by now, and an interrupt must not leave the state
		// disagreeing with the server about what was already done.
		settled, done := context.WithTimeout(context.WithoutCancel(ctx), stateWriteGrace)
		err = s.db.RecordFlags(settled, p.folderID, p.src.UIDValidity, c.srcUID, c.text)
		done()
		if err != nil {
			return fmt.Errorf("recording flags of message %d: %w", c.srcUID, err)
		}
		lv.reflagged()
	}

	s.log.Info("flags resynchronised", "source", pair.Source, "dest", pair.Dest, "messages", len(changes))
	return nil
}

// sourceFlags reads the flags the source currently holds, on a borrowed
// connection, asking only for what changed since the given sequence when the
// server can answer that question.
func (s *Syncer) sourceFlags(ctx context.Context, name string, since uint64) (_ []imapx.FlagSet, err error) {
	lease, err := s.src.Acquire(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("acquiring source connection: %w", err)
	}
	defer func() { lease.Release(err) }()

	if _, err = lease.Conn().Select(ctx, name, imapx.SelectOptions{ReadOnly: true}); err != nil {
		return nil, fmt.Errorf("selecting source mailbox: %w", err)
	}
	return lease.Conn().FetchFlags(ctx, since)
}

// flagText is the canonical form of a flag set, for storing and for comparing.
//
// Sorted, because servers are free to list flags in any order and an unsorted
// join would report a change every run for a message nobody touched.
func flagText(flags []string) string {
	out := slices.Clone(flags)
	slices.Sort(out)
	return strings.Join(out, " ")
}

// unchanged reports whether the fast path applies to this folder.
//
// Every condition here is load-bearing. A watermark of zero is a server that
// does not report modification sequences, or a folder that has never completed;
// a UIDVALIDITY that has moved means the stored sequence describes a mailbox
// that no longer exists; and --full is how someone who suspects the destination
// has drifted asks for everything to be checked, since nothing about the source
// can reveal a message deleted at the far end.
func (s *Syncer) unchanged(row state.Folder, src imapx.Mailbox) bool {
	// A folder whose deletions are not carried out up to this same point has
	// changed in a way this run cares about, whatever the modseq says.
	if s.opts.Delete2 && row.SrcDeletedThrough != src.HighestModSeq {
		return false
	}
	if s.opts.Full || row.SrcHighestModSeq == 0 || src.HighestModSeq == 0 {
		return false
	}
	return row.SrcUIDValidity == src.UIDValidity && row.SrcHighestModSeq == src.HighestModSeq
}

// prepareFolder works out what has to be copied, on one connection per side.
//
// This is the only place that holds a lease on both accounts at once, and it
// takes the source first. Nothing anywhere takes a source connection while
// holding a destination one, so the two pools cannot deadlock against each
// other no matter how differently they are sized.
func (s *Syncer) prepareFolder(ctx context.Context, pair folder.Pair, lv *live) (_ *prepared, err error) {
	srcLease, err := s.src.Acquire(ctx, pair.Source)
	if err != nil {
		return nil, fmt.Errorf("acquiring source connection: %w", err)
	}
	defer func() { srcLease.Release(err) }()
	src := srcLease.Conn()

	srcBox, err := src.Select(ctx, pair.Source, imapx.SelectOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("selecting source mailbox: %w", err)
	}
	lv.report.Messages = int(srcBox.NumMessages)

	row, err := s.db.EnsureFolder(ctx, s.opts.PairID, pair.Source, pair.Dest)
	if err != nil {
		return nil, fmt.Errorf("recording folder: %w", err)
	}

	// The fast path. A modification sequence that has not moved means nothing
	// in this mailbox has been added, deleted or reflagged since the watermark
	// was earned, and that watermark is only written when a folder finished
	// with every message copied. So there is nothing to do, and finding that
	// out cost one SELECT rather than a UID listing of the whole mailbox.
	//
	// On an account of 144 folders and 776k messages this is most of a repeat
	// run: without it every folder pays for enumerating every UID it holds,
	// against a server that is charging by the round trip.
	if s.unchanged(row, srcBox) {
		lv.report.AlreadyDone = int(srcBox.NumMessages)
		s.log.Info("folder unchanged", "source", pair.Source, "dest", pair.Dest,
			"messages", srcBox.NumMessages, "modseq", srcBox.HighestModSeq)
		return &prepared{folderID: row.ID, src: srcBox, skipped: true}, nil
	}

	dstLease, err := s.dst.Acquire(ctx, pair.Dest)
	if err != nil {
		return nil, fmt.Errorf("acquiring destination connection: %w", err)
	}
	defer func() { dstLease.Release(err) }()
	dst := dstLease.Conn()

	dstBox, err := dst.Select(ctx, pair.Dest, imapx.SelectOptions{})
	if err != nil {
		return nil, fmt.Errorf("selecting destination mailbox: %w", err)
	}
	if dstBox.ReadOnly {
		return nil, fmt.Errorf("destination mailbox %q is read-only", pair.Dest)
	}

	kept, err := s.db.FenceUIDValidity(ctx, row.ID, srcBox.UIDValidity, dstBox.UIDValidity)
	if err != nil {
		return nil, fmt.Errorf("fencing UIDVALIDITY: %w", err)
	}
	if !kept {
		// Not an error: the server renumbered the mailbox, which it is entitled
		// to do. Every UID we hold now refers to a different message or to
		// none, so the run falls back to identity matching for this folder.
		s.log.Warn("UIDVALIDITY changed; falling back to identity matching",
			"source", pair.Source, "src_uidvalidity", srcBox.UIDValidity, "dst_uidvalidity", dstBox.UIDValidity)
	}

	// Suspects first. An in-flight row may already exist on the destination,
	// and copying it again is exactly the duplication this tool exists to
	// avoid.
	adopted, err := s.recover(ctx, src, dst, row.ID, srcBox, dstBox)
	if err != nil {
		return nil, fmt.Errorf("recovering in-flight messages: %w", err)
	}
	lv.report.Adopted += adopted

	known, err := s.db.SyncedUIDs(ctx, row.ID, srcBox.UIDValidity)
	if err != nil {
		return nil, fmt.Errorf("reading recorded messages: %w", err)
	}

	uids, err := src.AllUIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerating source messages: %w", err)
	}

	p := &prepared{folderID: row.ID, src: srcBox, dst: dstBox, since: watermark(row, kept)}
	p.todo = triage(uids, known, lv)
	if s.opts.Delete2 {
		// Sorted so the deletion pass can ask "is this UID still there" by
		// binary search rather than by building a second map the size of the
		// mailbox. Most servers list in order already; sorting a sorted slice
		// costs a scan.
		slices.Sort(uids)
		p.live = uids
	}

	// Nothing below reads the source, and indexing a large destination can take
	// a while. Handing the connection back now lets another folder use it.
	srcLease.Release(nil)

	s.log.Info("folder diffed",
		"source", pair.Source, "dest", pair.Dest,
		"messages", len(uids), "to_copy", len(p.todo), "already_done", lv.report.AlreadyDone)

	// Tier 3 in bulk, and only when tier 1 has nothing to say about the folder
	// as a whole: a first sync onto a destination that is not empty, a sync
	// resumed before one ever completed, a lost database, or a UIDVALIDITY
	// change, which the fence has just turned into the same thing.
	//
	// When the folder has completed a sync before, the UID map answers for
	// everything except the in-flight suspects, and those were settled above by
	// a handful of targeted searches. Indexing then would mean reading every
	// header in a 400k-message folder (§6.3) to learn nothing.
	if len(p.todo) > 0 && dstBox.NumMessages > 0 && (row.LastSync.IsZero() || !kept) {
		lv.index, err = s.indexDestination(ctx, dst, row.ID, dstBox)
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

// triage sorts what the source listed into what still needs copying, counting
// the rest as it goes.
//
// The three outcomes are not two-and-an-oddity. "Already on the destination"
// and "the source has no message here" are both settled, and the only reason
// they are counted apart is that one of them is a number the source made up.
func triage(uids []uint32, known map[uint32]state.State, lv *live) []uint32 {
	var todo []uint32
	for _, uid := range uids {
		switch known[uid] {
		case state.StateDone:
			lv.report.AlreadyDone++
		case state.StateGone:
			// The source listed it again and still has no message for it.
			lv.report.Vanished++
		default:
			todo = append(todo, uid)
		}
	}
	return todo
}

// pendingAppend is a message that has been read from the source and is waiting
// for a destination connection.
type pendingAppend struct {
	uid          uint32
	id           ident.Identity
	stamped      bool
	flags        []string
	internalDate time.Time
	// body is the message as it will be appended, stamp included. Its length is
	// the literal size: it is what was actually read, never RFC822.SIZE (§3.7).
	body []byte
	// release returns this message's share of the byte budget. It is idempotent.
	release func()
}

// copyFolder copies every message the setup stage listed.
//
// Fetching and appending are separate stages joined by a channel, which is what
// lets the two accounts run at their own speeds: a source that answers slowly
// does not idle thirty destination connections, and a destination that commits
// slowly does not stop the source being read. It also means no goroutine here
// holds a connection to both accounts, so the pools cannot deadlock.
func (s *Syncer) copyFolder(ctx context.Context, pair folder.Pair, p *prepared, lv *live) error {
	if len(p.todo) == 0 {
		return nil
	}

	chunks := make(chan []uint32)
	pending := make(chan *pendingAppend, s.dst.Cap())

	chunkCount := (len(p.todo) + copyChunk - 1) / copyChunk
	g, gctx := errgroup.WithContext(ctx)

	// Chunks are handed out rather than dealt out, so a worker that draws a run
	// of huge messages simply takes fewer of them instead of becoming the
	// straggler the folder waits on.
	g.Go(func() error {
		defer close(chunks)
		for chunk := range slices.Chunk(p.todo, copyChunk) {
			select {
			case chunks <- chunk:
			case <-gctx.Done():
				return gctx.Err()
			}
		}
		return nil
	})

	var fetchers errgroup.Group
	for range min(s.src.Cap(), chunkCount) {
		fetchers.Go(func() error {
			for chunk := range chunks {
				if err := s.fetchChunk(gctx, pair, p, chunk, pending, lv); err != nil {
					return err
				}
			}
			return nil
		})
	}
	g.Go(func() error {
		err := fetchers.Wait()
		// Closing this is what stops the append workers. It has to happen once
		// every fetcher has finished, whether they finished the folder or gave
		// up on it, or the appenders would wait for messages nobody is coming
		// to fetch.
		close(pending)
		return err
	})

	for range min(s.dst.Cap(), len(p.todo)) {
		g.Go(func() error {
			for item := range pending {
				err := s.deliver(gctx, p, pair.Dest, item, lv)
				item.release()
				if err != nil {
					return err
				}
			}
			return nil
		})
	}

	err := g.Wait()

	// An append worker that stopped early leaves fetched messages behind, each
	// still holding its share of the byte budget. Nothing will append them now,
	// but the bytes have to come back or the next folder starts short.
	for item := range pending {
		item.release()
	}
	return err
}

// errRenumbered means the source mailbox was renumbered while the folder was
// being copied. It is not retryable and not skippable: every UID chosen for
// this folder now names a different message or none at all.
var errRenumbered = errors.New("source mailbox renumbered mid-run")

// fetchChunk reads one run of messages from the source, replacing the
// connection under itself as often as the network requires.
//
// The retry resumes at the message that failed instead of restarting the chunk.
// That is a correctness requirement rather than an optimisation: everything
// already fetched is recorded as in flight and may be sitting in the append
// queue or already on the destination, so re-fetching it would copy it twice.
func (s *Syncer) fetchChunk(
	ctx context.Context,
	pair folder.Pair,
	p *prepared,
	chunk []uint32,
	pending chan<- *pendingAppend,
	lv *live,
) error {
	metas, err := s.chunkMeta(ctx, pair, p, chunk, lv)
	if err != nil {
		return err
	}
	if err := s.markGone(ctx, p, chunk, metas, lv); err != nil {
		return err
	}

	attempt := 0
	for done := 0; done < len(metas); {
		n, err := s.fetchRun(ctx, pair, p, metas[done:], pending, lv)
		done += n
		if n > 0 {
			attempt = 0
		}
		if err == nil {
			continue
		}
		if errors.Is(err, errRenumbered) || done >= len(metas) {
			return err
		}
		stuck := metas[done]

		kind := retry.Classify(err)
		lv.health.trouble(err)
		if kind == retry.Stop {
			return err
		}
		if attempt++; (kind == retry.Again || kind == retry.Slower) && attempt < s.opts.Retry.Attempts {
			s.log.Warn("retrying message",
				"folder", pair.Source, "uid", stuck.UID, "attempt", attempt, "action", kind.String(), "error", err)
			if err := s.opts.Retry.Wait(ctx, kind, attempt-1); err != nil {
				return err
			}
			continue
		}

		// Out of attempts, or nothing an attempt could fix. Recording the
		// message and moving on keeps the rest of the folder copyable; a run
		// that is failing systemically is stopped by the ceiling instead.
		if err := s.fail(ctx, p.folderID, p.src.UIDValidity, stuck.UID, err.Error(), lv); err != nil {
			return err
		}
		done++
		attempt = 0
	}
	return nil
}

// markGone records the UIDs of a chunk the server had no message for.
//
// A UID FETCH that completes successfully and says nothing about a message is
// the server saying there is no such message: FETCH is not an error for a UID
// that does not exist, it simply returns nothing. A short response therefore
// means those UIDs are not there, not that the command went wrong — a command
// that went wrong fails, and this is only reached when it did not.
//
// iCloud makes this routine rather than exceptional. Its SEARCH ALL on a
// 414k-message INBOX returns just over half a million UIDs, of which roughly
// ninety thousand have nothing behind them. Silently dropping those, which is
// what happened before, left a report that did not add up and a folder that
// asked for the same ninety thousand on every run for ever.
func (s *Syncer) markGone(ctx context.Context, p *prepared, chunk []uint32, metas []imapx.MessageMeta, lv *live) error {
	if len(metas) == len(chunk) {
		return nil
	}

	got := make(map[uint32]struct{}, len(metas))
	for _, m := range metas {
		got[m.UID] = struct{}{}
	}

	n := 0
	for _, uid := range chunk {
		if _, ok := got[uid]; ok {
			continue
		}
		if err := s.db.MarkGone(ctx, p.folderID, p.src.UIDValidity, uid); err != nil {
			return err
		}
		n++
	}
	if n > 0 {
		lv.vanished(n)
	}
	return nil
}

// defaultDeleteCeiling is the fraction of a folder's recorded messages that may
// be deleted in one run without being asked twice.
const defaultDeleteCeiling = 0.10

// defaultDeleteFloor is the number of messages a folder may always lose,
// whatever share of it they are.
//
// A proportion is meaningless on small numbers. One message out of six is 16.7%
// and would trip a 10% ceiling, but one message out of six leaving a mailbox is
// the single most ordinary thing that happens to mail. A guard that fires on
// the ordinary case gets --force written into the cron line permanently, and
// then it is not a guard at all. So a small absolute number of deletions is
// always allowed: a source that lies about a six-message folder costs you six
// messages, while one that lies about a four-hundred-thousand-message folder is
// the disaster actually worth stopping, and that one is nowhere near the floor.
const defaultDeleteFloor = 10

// condemned reports which recorded messages the source no longer lists, and
// whether the safety ceiling will let them go.
//
// Both the real run and the dry run go through here, which is the only reason
// the dry run is worth anything: a preview computed by a second implementation
// previews that implementation.
func (s *Syncer) condemned(mirrors []state.Mirror, live []uint32) (victims []state.Mirror, allowed bool, share float64) {
	for _, m := range mirrors {
		if _, found := slices.BinarySearch(live, m.SrcUID); !found {
			victims = append(victims, m)
		}
	}
	if len(victims) == 0 || len(mirrors) == 0 {
		return victims, true, 0
	}

	// The denominator is what we manage, not what the folder holds. The failure
	// this catches is a source that answers a UID listing with nothing or with
	// a fraction of the truth, and that shows up as a large share of the
	// message map going at once — which a destination-sized denominator would
	// dilute into looking reasonable.
	share = float64(len(victims)) / float64(len(mirrors))
	if len(victims) <= defaultDeleteFloor {
		return victims, true, share
	}
	return victims, share <= s.ceiling() || s.opts.Force, share
}

// deleteVanished removes destination messages whose source counterpart is gone.
//
// The question is asked only of messages this tool copied and recorded. Mail
// that was on the destination before the first sync has no row here, so no
// amount of divergence makes it a candidate. That is narrower than imapsync's
// --delete2, which will empty a destination of anything the source lacks, and
// the narrowness is the point: the state database is the only record of what we
// are actually responsible for.
func (s *Syncer) deleteVanished(ctx context.Context, pair folder.Pair, p *prepared, lv *live) (err error) {
	// p.skipped is a barrier, like the one in resyncFlags. No test can
	// distinguish it: a fast-pathed folder never reached the code that gathers
	// the source listing, and its zero destination mailbox makes the message
	// map come back empty, so nothing would be nominated anyway. It is kept
	// against a later change that populates either of those earlier, which
	// would otherwise turn "the server says nothing has changed" into a folder
	// full of deletions.
	if !s.opts.Delete2 || p.skipped {
		return nil
	}
	// A folder that could not copy everything is a folder whose picture of the
	// source is already known to be incomplete. Deleting from it is exactly the
	// wrong response to that.
	if lv.report.Failed > 0 {
		s.log.Warn("not deleting from a folder that failed to copy everything",
			"dest", pair.Dest, "failed", lv.report.Failed)
		return nil
	}

	mirrors, err := s.mirrored(ctx, p)
	if err != nil {
		return err
	}

	doomed, allowed, share := s.condemned(mirrors, p.live)
	if len(doomed) == 0 {
		return nil
	}
	if !allowed {
		lv.refused(len(doomed))
		s.log.Error("refusing to delete this many messages at once",
			"dest", pair.Dest, "would_delete", len(doomed), "of", len(mirrors),
			"share", pct(share), "ceiling", pct(s.ceiling()))
		return nil
	}

	lease, err := s.dst.Acquire(ctx, pair.Dest)
	if err != nil {
		return fmt.Errorf("acquiring destination connection for deletion: %w", err)
	}
	defer func() { lease.Release(err) }()
	dst := lease.Conn()

	if _, err = dst.Select(ctx, pair.Dest, imapx.SelectOptions{}); err != nil {
		return fmt.Errorf("selecting destination mailbox for deletion: %w", err)
	}

	uids := make([]uint32, 0, len(doomed))
	forget := make([]uint32, 0, len(doomed))
	for _, m := range doomed {
		uids = append(uids, m.DstUID)
		forget = append(forget, m.SrcUID)
	}

	if err = dst.DeleteMessages(ctx, uids); err != nil {
		return fmt.Errorf("deleting %d messages from %q: %w", len(uids), pair.Dest, err)
	}

	// Only now. A row removed before the expunge that followed it failed would
	// leave a message on the destination that nothing remembers putting there,
	// and so nothing would ever remove.
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateWriteGrace)
	defer cancel()
	if err = s.db.ForgetMessages(write, p.folderID, p.src.UIDValidity, forget); err != nil {
		return err
	}

	lv.deleted(len(doomed))
	s.log.Info("deleted messages the source no longer has",
		"dest", pair.Dest, "messages", len(doomed))
	return nil
}

// ceiling is the configured deletion ceiling, or the default.
func (s *Syncer) ceiling() float64 {
	if s.opts.DeleteCeiling == 0 {
		return defaultDeleteCeiling
	}
	return s.opts.DeleteCeiling
}

func pct(f float64) string { return fmt.Sprintf("%.1f%%", f*100) }

// chunkMeta reads the headers that open a chunk, retrying on a fresh
// connection.
//
// A permanent failure here abandons the folder rather than the chunk. Without
// headers there is no identity for any of these messages, so they could only be
// recorded as failed en masse, and fifty identical failures say nothing the one
// underlying error did not.
func (s *Syncer) chunkMeta(
	ctx context.Context,
	pair folder.Pair,
	p *prepared,
	chunk []uint32,
	lv *live,
) ([]imapx.MessageMeta, error) {
	for attempt := 0; ; attempt++ {
		metas, err := s.metaOnce(ctx, pair, p, chunk)
		if err == nil {
			return metas, nil
		}
		if errors.Is(err, errRenumbered) {
			return nil, err
		}

		kind := retry.Classify(err)
		lv.health.trouble(err)
		if kind != retry.Again && kind != retry.Slower {
			return nil, err
		}
		if attempt+1 >= s.opts.Retry.Attempts {
			return nil, err
		}
		s.log.Warn("retrying metadata fetch",
			"folder", pair.Source, "messages", len(chunk), "attempt", attempt+1, "error", err)
		if err := s.opts.Retry.Wait(ctx, kind, attempt); err != nil {
			return nil, err
		}
	}
}

func (s *Syncer) metaOnce(
	ctx context.Context,
	pair folder.Pair,
	p *prepared,
	chunk []uint32,
) (_ []imapx.MessageMeta, err error) {
	lease, err := s.src.Acquire(ctx, pair.Source)
	if err != nil {
		return nil, fmt.Errorf("acquiring source connection: %w", err)
	}
	defer func() { lease.Release(err) }()

	if err = checkNumbering(lease, p, pair.Source); err != nil {
		return nil, err
	}
	metas, err := lease.Conn().FetchMeta(ctx, chunk, ident.Fields)
	if err != nil {
		return nil, fmt.Errorf("fetching message metadata: %w", err)
	}
	return metas, nil
}

// fetchRun reads as many of metas as one connection manages, and reports how
// many it finished. The message that stopped it is metas[n].
func (s *Syncer) fetchRun(
	ctx context.Context,
	pair folder.Pair,
	p *prepared,
	metas []imapx.MessageMeta,
	pending chan<- *pendingAppend,
	lv *live,
) (n int, err error) {
	lease, err := s.src.Acquire(ctx, pair.Source)
	if err != nil {
		return 0, fmt.Errorf("acquiring source connection: %w", err)
	}
	defer func() { lease.Release(err) }()

	if err = checkNumbering(lease, p, pair.Source); err != nil {
		return 0, err
	}

	for i, meta := range metas {
		if err = s.fetchOne(ctx, p, lease.Conn(), meta, pending, lv); err != nil {
			return i, err
		}
	}
	return len(metas), nil
}

// checkNumbering refuses to use a connection that is looking at a renumbered
// mailbox.
//
// A single long-lived connection can never see a mailbox renumbered, because it
// never selects it again. A pool selects on every lease, so it can — and every
// UID chosen for this folder was chosen against the old numbering, meaning each
// one now names a different message or none at all. Stopping here costs a
// re-sync; carrying on would file real messages under keys belonging to other
// ones, and mark them done.
func checkNumbering(lease *pool.Lease, p *prepared, name string) error {
	got := lease.UIDValidity()
	if got == p.src.UIDValidity {
		return nil
	}
	return fmt.Errorf("%w: %q had UIDVALIDITY %d and now has %d; it will be resynced on the next run",
		errRenumbered, name, p.src.UIDValidity, got)
}

// fetchOne records a message as in flight and reads it, unless the destination
// already has it.
//
// The ordering is the whole point: the state row is committed as in-flight
// *before* the APPEND is issued, so a crash at any instant leaves a row that
// says "this may or may not have landed" rather than no evidence at all.
func (s *Syncer) fetchOne(
	ctx context.Context,
	p *prepared,
	src imapx.Conn,
	meta imapx.MessageMeta,
	pending chan<- *pendingAppend,
	lv *live,
) error {
	id := ident.Parse(meta.Header)
	flags := copyableFlags(meta.Flags)

	row := state.Message{
		FolderID:       p.folderID,
		SrcUIDValidity: p.src.UIDValidity,
		SrcUID:         meta.UID,
		IdentHash:      id.Digest,
		Size:           meta.Size,
		Flags:          flagText(flags),
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
	// of making a second copy. This happens before the body is fetched, not
	// after: on a 400k-message folder the difference is hours of transfer.
	if dstUID, ok := lv.adopt(id); ok {
		if err := s.db.CompleteAppend(ctx, p.folderID, p.src.UIDValidity, meta.UID, p.dst.UIDValidity, dstUID); err != nil {
			return fmt.Errorf("recording message %d as adopted: %w", meta.UID, err)
		}
		lv.adopted(1)
		return nil
	}

	// Charged before the body is read and refunded once it has been appended,
	// so what bounds the memory in flight is this budget rather than however
	// far ahead the fetchers happen to have run (§4.3).
	release, err := s.bytes.Acquire(ctx, meta.Size)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	if row.StampID != "" {
		buf.Write(ident.StampBytes(row.StampID))
	}

	if _, err := src.FetchBody(ctx, meta.UID, &buf); err != nil {
		release()
		if errors.Is(err, imapx.ErrMessageGone) {
			// Expunged between enumeration and fetch. Nothing was appended and
			// nothing can be, so this is not a failure — recording it as one
			// would put the UID back in the queue on every later run and stop
			// the folder's watermark ever advancing, which is the opposite of
			// "do not retry forever".
			if err := s.db.MarkGone(ctx, p.folderID, p.src.UIDValidity, meta.UID); err != nil {
				return err
			}
			lv.vanished(1)
			s.log.Info("source message expunged before it could be read", "uid", meta.UID)
			return nil
		}
		return fmt.Errorf("fetching message %d: %w", meta.UID, err)
	}

	item := &pendingAppend{
		uid:          meta.UID,
		id:           id,
		stamped:      row.StampID != "",
		flags:        flags,
		internalDate: meta.InternalDate,
		body:         buf.Bytes(),
		release:      release,
	}
	select {
	case pending <- item:
		return nil
	case <-ctx.Done():
		release()
		return ctx.Err()
	}
}

// deliver appends one fetched message to the destination and records where it
// landed.
func (s *Syncer) deliver(ctx context.Context, p *prepared, dstName string, item *pendingAppend, lv *live) error {
	for attempt := 0; ; attempt++ {
		res, err := s.appendOne(ctx, dstName, item)
		if err == nil {
			return s.settle(ctx, p, dstName, item, res, lv)
		}

		kind := retry.Classify(err)
		lv.health.trouble(err)
		if kind == retry.Stop {
			return err
		}
		if kind == retry.Skip || attempt+1 >= s.opts.Retry.Attempts {
			// The server refused this message — oversized, rejected by policy,
			// or malformed beyond what it will store — or it has refused often
			// enough. The rest of the folder can still be copied.
			return s.fail(ctx, p.folderID, p.src.UIDValidity, item.uid, err.Error(), lv)
		}

		// A connection lost during an APPEND says nothing about whether the
		// message arrived: the literal may have been complete on the wire when
		// the response was lost. Appending again without looking is how one
		// interruption becomes two copies, so the destination is asked first.
		landed, lerr := s.locateOnDest(ctx, dstName, item.id, item.stamped)
		if lerr != nil {
			return lerr
		}
		if landed != 0 {
			s.log.Info("append survived a lost connection", "folder", dstName, "uid", item.uid)
			return s.record(ctx, p, item, p.dst.UIDValidity, landed, lv)
		}

		s.log.Warn("retrying append",
			"folder", dstName, "uid", item.uid, "attempt", attempt+1, "action", kind.String(), "error", err)
		if err := s.opts.Retry.Wait(ctx, kind, attempt); err != nil {
			return err
		}
	}
}

// settle works out where an accepted message landed and records it.
func (s *Syncer) settle(
	ctx context.Context,
	p *prepared,
	dstName string,
	item *pendingAppend,
	res imapx.AppendResult,
	lv *live,
) error {
	dstUIDValidity, dstUID := res.UIDValidity, res.UID
	if !res.Assigned() {
		// No UIDPLUS. The append succeeded — the tagged OK says so — but the
		// destination UID has to be found, and may not be findable at all.
		var err error
		dstUIDValidity = p.dst.UIDValidity
		dstUID, err = s.locateOnDest(ctx, dstName, item.id, item.stamped)
		if err != nil {
			return err
		}
	}
	return s.record(ctx, p, item, dstUIDValidity, dstUID, lv)
}

// record commits a copy as done.
func (s *Syncer) record(
	ctx context.Context,
	p *prepared,
	item *pendingAppend,
	dstUIDValidity, dstUID uint32,
	lv *live,
) error {
	// The message is on the destination by the time this runs, so the write
	// that says so must not be abandoned because the run was interrupted.
	// Cancellation here would leave the copy done and unrecorded — the next run
	// would find it by searching the destination, which is slower, and for a
	// message too weak to search for would copy it a second time. Detached from
	// the caller's context and bounded on its own, so an interrupt cannot make
	// it hang either.
	settled, done := context.WithTimeout(context.WithoutCancel(ctx), stateWriteGrace)
	defer done()

	if err := s.db.CompleteAppend(settled, p.folderID, p.src.UIDValidity, item.uid, dstUIDValidity, dstUID); err != nil {
		return fmt.Errorf("recording message %d as copied: %w", item.uid, err)
	}
	lv.copied()
	return nil
}

// appendOne holds a destination connection for exactly one APPEND.
//
// The lease names no mailbox: APPEND carries its own target, so a connection
// can serve any folder without a SELECT, which is what lets a single
// destination pool be shared by every folder in flight.
func (s *Syncer) appendOne(ctx context.Context, dstName string, item *pendingAppend) (res imapx.AppendResult, err error) {
	lease, err := s.dst.Acquire(ctx, "")
	if err != nil {
		return res, fmt.Errorf("acquiring destination connection: %w", err)
	}
	defer func() { lease.Release(err) }()

	res, err = lease.Conn().Append(ctx, dstName, imapx.AppendMessage{
		Size:         int64(len(item.body)),
		Flags:        item.flags,
		InternalDate: item.internalDate,
		Body:         bytes.NewReader(item.body),
	})
	if errors.Is(err, imapx.ErrConnectionBroken) {
		err = fmt.Errorf("appending message %d: %w", item.uid, err)
	}
	return res, err
}

// locateOnDest searches the destination folder for a message just appended.
//
// It takes a second, separate lease rather than reusing the one that did the
// APPEND: SEARCH answers for the selected mailbox, and an APPEND lease names
// none. Holding both at once would be a destination connection waiting on the
// destination pool, which on a small pool is a deadlock.
func (s *Syncer) locateOnDest(ctx context.Context, dstName string, id ident.Identity, stamped bool) (_ uint32, err error) {
	if !searchable(id, stamped) {
		return 0, nil
	}
	lease, err := s.dst.Acquire(ctx, dstName)
	if err != nil {
		return 0, fmt.Errorf("acquiring destination connection: %w", err)
	}
	defer func() { lease.Release(err) }()
	return s.locate(ctx, lease.Conn(), id, stamped), nil
}

// dryRunFolder reports what a real run would copy, without writing anything.
func (s *Syncer) dryRunFolder(ctx context.Context, pair folder.Pair) (_ FolderReport, err error) {
	fr := FolderReport{Source: pair.Source, Dest: pair.Dest}

	lease, err := s.src.Acquire(ctx, pair.Source)
	if err != nil {
		return fr, fmt.Errorf("acquiring source connection: %w", err)
	}
	defer func() { lease.Release(err) }()
	src := lease.Conn()

	srcBox, err := src.Select(ctx, pair.Source, imapx.SelectOptions{ReadOnly: true})
	if err != nil {
		return fr, fmt.Errorf("selecting source mailbox: %w", err)
	}
	fr.Messages = int(srcBox.NumMessages)

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
	known, err := s.db.SyncedUIDs(ctx, row.ID, srcBox.UIDValidity)
	if err != nil {
		return fr, fmt.Errorf("reading recorded messages: %w", err)
	}

	uids, err := src.AllUIDs(ctx)
	if err != nil {
		return fr, fmt.Errorf("enumerating source messages: %w", err)
	}
	for _, uid := range uids {
		switch known[uid] {
		case state.StateDone:
			fr.AlreadyDone++
		case state.StateGone:
			fr.Vanished++
		default:
			fr.Copied++
		}
	}

	if s.opts.Delete2 {
		if err := s.dryRunDeletions(ctx, pair, row.ID, srcBox, uids, &fr); err != nil {
			return fr, err
		}
	}
	return fr, nil
}

// dryRunDeletions works out what a real run would remove, and says so without
// removing it.
//
// This costs a destination connection that a dry run would otherwise not need,
// and it is worth it: the whole point of previewing a destructive run is to see
// the destruction. It selects read-only, because the UIDVALIDITY it reads is
// half the question — if the destination has been renumbered, the real run
// would find no message map and delete nothing, and so must this.
func (s *Syncer) dryRunDeletions(ctx context.Context, pair folder.Pair, folderID int64, srcBox imapx.Mailbox, uids []uint32, fr *FolderReport) (err error) {
	lease, err := s.dst.Acquire(ctx, pair.Dest)
	if err != nil {
		return fmt.Errorf("acquiring destination connection: %w", err)
	}
	defer func() { lease.Release(err) }()

	dstBox, err := lease.Conn().Select(ctx, pair.Dest, imapx.SelectOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("selecting destination mailbox: %w", err)
	}

	mirrors, err := s.db.Mirrored(ctx, folderID, srcBox.UIDValidity, dstBox.UIDValidity)
	if err != nil {
		return err
	}

	live := slices.Clone(uids)
	slices.Sort(live)
	doomed, allowed, share := s.condemned(mirrors, live)
	if len(doomed) == 0 {
		return nil
	}
	if !allowed {
		fr.Refused = len(doomed)
		s.log.Error("would refuse to delete this many messages at once",
			"dest", pair.Dest, "would_delete", len(doomed), "of", len(mirrors),
			"share", pct(share), "ceiling", pct(s.ceiling()))
		return nil
	}
	fr.Deleted = len(doomed)
	return nil
}

// fail records an abandoned copy and keeps the run going.
func (s *Syncer) fail(ctx context.Context, folderID int64, uidValidity, uid uint32, reason string, lv *live) error {
	if err := s.db.FailAppend(ctx, folderID, uidValidity, uid, reason); err != nil {
		return fmt.Errorf("recording message %d as failed: %w", uid, err)
	}
	lv.failed(uid, reason)
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
//
// It runs before any of the folder's workers start, on the caller's two leases,
// so nothing else is appending to this folder while it looks.
func (s *Syncer) recover(ctx context.Context, src, dst imapx.Conn, folderID int64, srcBox, dstBox imapx.Mailbox) (int, error) {
	suspects, err := s.db.InFlight(ctx, folderID)
	if err != nil {
		return 0, err
	}
	if len(suspects) == 0 {
		return 0, nil
	}
	s.log.Info("settling messages left in flight", "count", len(suspects), "folder", srcBox.Name)

	var adopted int
	for _, m := range suspects {
		if err := ctx.Err(); err != nil {
			return adopted, err
		}

		id, ok, err := s.identify(ctx, src, m)
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

		uid := s.locate(ctx, dst, id, m.StampID != "")
		if uid == 0 {
			continue // never landed, or cannot be searched for: retry the copy
		}
		if err := s.db.CompleteAppend(ctx, folderID, m.SrcUIDValidity, m.SrcUID, dstBox.UIDValidity, uid); err != nil {
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
func (s *Syncer) identify(ctx context.Context, src imapx.Conn, m state.Message) (ident.Identity, bool, error) {
	metas, err := src.FetchMeta(ctx, []uint32{m.SrcUID}, ident.Fields)
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

// searchable reports whether a message can be looked for on the destination.
//
// Weak identities are never acted on, stamp or no stamp: the stamp is the
// digest, so a digest too thin to distinguish two messages makes a stamp that
// is too thin as well.
func searchable(id ident.Identity, stamped bool) bool {
	if id.Weak {
		return false
	}
	_, _, ok := ident.SearchTerms(id, stamped)
	return ok
}

// locate finds a message on the destination, returning 0 when it cannot.
//
// The connection must already have the destination folder selected, because
// SEARCH answers only for the selected mailbox.
//
// A failure to find is not an error: the commonest reason is that the message
// genuinely is not there. Nor is a search failure, which costs a re-copy rather
// than a lost message.
func (s *Syncer) locate(ctx context.Context, dst imapx.Conn, id ident.Identity, stamped bool) uint32 {
	if !searchable(id, stamped) {
		return 0
	}
	field, value, _ := ident.SearchTerms(id, stamped)

	uids, err := dst.SearchHeader(ctx, field, value)
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
