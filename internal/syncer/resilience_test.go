package syncer_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/retry"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// brisk is the retry policy these tests use: the same shape as the real one,
// with the waiting taken out. What is under test is which failures are retried
// and what happens in between, not how long the pauses are, which the retry
// package measures on its own.
func brisk() retry.Policy {
	return retry.Policy{Attempts: 4, Base: time.Millisecond, Slow: time.Millisecond, Max: 5 * time.Millisecond}
}

// syncFlaky runs a sync against decorated connections with a given retry
// policy.
func syncFlaky(
	t *testing.T,
	h *harness,
	srcCap, dstCap int,
	opts syncer.Options,
	wrapSrc, wrapDst func(imapx.Conn) imapx.Conn,
) (syncer.Report, error) {
	t.Helper()
	return syncFlakyCtx(t, context.Background(), h, srcCap, dstCap, opts, wrapSrc, wrapDst)
}

// syncFlakyCtx is syncFlaky under a context the caller can cancel.
func syncFlakyCtx(
	t *testing.T,
	ctx context.Context,
	h *harness,
	srcCap, dstCap int,
	opts syncer.Options,
	wrapSrc, wrapDst func(imapx.Conn) imapx.Conn,
) (syncer.Report, error) {
	t.Helper()

	if opts.PairID == "" {
		opts.PairID = "test"
	}
	if opts.Retry.Attempts == 0 {
		opts.Retry = brisk()
	}
	s := syncer.New(
		pooled(t, srcCap, readOnly, h.src.dialFunc(t, wrapSrc)),
		pooled(t, dstCap, imapx.SelectOptions{}, h.dst.dialFunc(t, wrapDst)),
		h.db, nil, opts,
	)
	return s.Run(ctx)
}

// droppingSource breaks the connection on selected body fetches.
type droppingSource struct {
	imapx.Conn
	// drop reports whether the nth fetch of this run should fail.
	drop  func(n int32) bool
	count *atomic.Int32
}

func (d droppingSource) FetchBody(ctx context.Context, uid uint32, w io.Writer) (int64, error) {
	if d.drop(d.count.Add(1)) {
		_ = d.Close()
		return 0, imapx.ErrConnectionBroken
	}
	return d.Conn.FetchBody(ctx, uid, w)
}

// TestADroppedSourceConnectionIsRetried is the headline of this milestone.
//
// Before it, one dropped connection abandoned the whole folder. On a mailbox of
// four hundred thousand messages against a server nobody controls, that made
// the difference between starting a sync and walking away, and nursing it.
func TestADroppedSourceConnectionIsRetried(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 120)

	count := &atomic.Int32{}
	report, err := syncFlaky(t, h, 3, 4, syncer.Options{}, func(c imapx.Conn) imapx.Conn {
		// Every seventeenth read, on every connection, for the whole run.
		return droppingSource{Conn: c, count: count, drop: func(n int32) bool { return n%17 == 0 }}
	}, nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	copied, _, failed := report.Totals()
	if copied != len(want) || failed != 0 {
		t.Errorf("copied %d and failed %d, want %d copied and none failed", copied, failed, len(want))
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// TestARetryResumesAtTheMessageThatFailed is the correctness half of the same
// feature, and the reason the retry is not simply "run the chunk again".
//
// Messages fetched before the failure are already recorded as in flight and may
// be sitting in the hand-off queue or already on the destination. Restarting
// the chunk would fetch and append every one of them a second time. The
// duplicate count below is what says the difference.
func TestARetryResumesAtTheMessageThatFailed(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	// Two and a bit chunks, so a failure lands mid-chunk with a substantial
	// run of messages already fetched behind it.
	want := fill(t, h.src, "INBOX", 120)

	count := &atomic.Int32{}
	report, err := syncFlaky(t, h, 1, 2, syncer.Options{}, func(c imapx.Conn) imapx.Conn {
		return droppingSource{Conn: c, count: count, drop: func(n int32) bool { return n == 30 || n == 31 }}
	}, nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if copied, _, _ := report.Totals(); copied != len(want) {
		t.Errorf("copied %d, want %d", copied, len(want))
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// lyingDest stores the message and then says the connection broke.
//
// This is the failure that makes retrying an append dangerous, and it is not
// exotic: an APPEND is complete on the wire before its response comes back, so
// any connection lost in between leaves a message stored and a client that
// believes it was not.
type lyingDest struct {
	imapx.Conn
	lies *atomic.Int32
}

func (d lyingDest) Append(ctx context.Context, mailbox string, msg imapx.AppendMessage) (imapx.AppendResult, error) {
	res, err := d.Conn.Append(ctx, mailbox, msg)
	if err != nil || d.lies.Add(-1) < 0 {
		return res, err
	}
	return imapx.AppendResult{}, imapx.ErrConnectionBroken
}

// TestAnAppendThatSurvivedALostConnectionIsNotSentTwice is the one test here
// that would be missed by any amount of care about connections.
//
// The retry is correct, the message is stored, and appending it again would
// still be wrong. The only way to tell is to ask the destination whether it
// already has it, which is what the retry does before trying again.
func TestAnAppendThatSurvivedALostConnectionIsNotSentTwice(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 40)

	lies := &atomic.Int32{}
	lies.Store(12)
	report, err := syncFlaky(t, h, 2, 3, syncer.Options{}, nil, func(c imapx.Conn) imapx.Conn {
		return lyingDest{Conn: c, lies: lies}
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Every message is on the destination exactly once, including the twelve
	// whose appends were reported as failures.
	assertExactly(t, h.dst, "INBOX", want)

	copied, _, failed := report.Totals()
	if copied != len(want) || failed != 0 {
		t.Errorf("copied %d and failed %d, want %d copied and none failed", copied, failed, len(want))
	}
}

// refusingDest rejects one message for good, and accepts everything else.
type refusingDest struct {
	imapx.Conn
	subject string
	tries   *atomic.Int32
}

func (d refusingDest) Append(ctx context.Context, mailbox string, msg imapx.AppendMessage) (imapx.AppendResult, error) {
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return imapx.AppendResult{}, err
	}
	if strings.Contains(string(body), d.subject) {
		d.tries.Add(1)
		return imapx.AppendResult{}, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: "message too large",
		}
	}
	msg.Body = bytes.NewReader(body)
	return d.Conn.Append(ctx, mailbox, msg)
}

// TestAMessageTheServerWillNeverTakeIsNotRetried checks the other half of the
// classification: a refusal that is final should cost one attempt, not four,
// and should take one message down rather than the folder.
func TestAMessageTheServerWillNeverTakeIsNotRetried(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 30)

	tries := &atomic.Int32{}
	report, err := syncFlaky(t, h, 2, 2, syncer.Options{}, nil, func(c imapx.Conn) imapx.Conn {
		return refusingDest{Conn: c, subject: want[7], tries: tries}
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	copied, _, failed := report.Totals()
	if copied != len(want)-1 || failed != 1 {
		t.Errorf("copied %d and failed %d, want %d copied and 1 failed", copied, failed, len(want)-1)
	}
	if got := tries.Load(); got != 1 {
		t.Errorf("the server was asked to take the same rejected message %d times; a final refusal is not worth repeating", got)
	}

	// Everything else arrived, so one bad message did not take the folder down.
	assertExactly(t, h.dst, "INBOX", slices.Concat(want[:7], want[8:]))
}

// TestAFailedMessageIsTriedAgainOnTheNextRun keeps the failure above from being
// permanent. Only messages recorded as done are skipped when a run is repeated,
// which is what makes it safe to give up on one message rather than the folder.
func TestAFailedMessageIsTriedAgainOnTheNextRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 10)

	tries := &atomic.Int32{}
	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, func(c imapx.Conn) imapx.Conn {
		return refusingDest{Conn: c, subject: want[3], tries: tries}
	}); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}

	report, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, nil)
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if copied, _, _ := report.Totals(); copied != 1 {
		t.Errorf("the second run copied %d messages, want the 1 that failed in the first", copied)
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// deadDest refuses everything, in a way that looks worth retrying.
type deadDest struct {
	imapx.Conn
	attempts *atomic.Int32
}

func (d deadDest) Append(context.Context, string, imapx.AppendMessage) (imapx.AppendResult, error) {
	d.attempts.Add(1)
	return imapx.AppendResult{}, imapx.ErrConnectionBroken
}

// TestARunAgainstADeadServerGivesUp is the bound on everything above.
//
// Retrying assumes something might still succeed. A destination that has
// stopped accepting mail fails every message identically, and each failure
// costs a full allowance of attempts and pauses. Without a ceiling a large
// folder would spend hours establishing, one message at a time, a fact the
// first fifty already established.
func TestARunAgainstADeadServerGivesUp(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 400)

	attempts := &atomic.Int32{}
	opts := syncer.Options{GiveUpAfter: 10}
	_, err := syncFlaky(t, h, 2, 2, opts, nil, func(c imapx.Conn) imapx.Conn {
		return deadDest{Conn: c, attempts: attempts}
	})

	if err == nil {
		t.Fatal("Run() returned no error against a destination that refused every message")
	}
	if !strings.Contains(err.Error(), "in a row") {
		t.Errorf("Run() error = %v, want it to say the run was given up on", err)
	}

	// Ten failures, each costing four attempts, is forty appends. The bound is
	// loose because workers in flight when the ceiling trips finish what they
	// were doing; what matters is that it is nothing like four hundred.
	if got := attempts.Load(); got > 200 {
		t.Errorf("the destination was asked %d times before the run gave up, out of 400 messages", got)
	}
}

// TestSteadyTroubleDoesNotEndAHealthyRun is the other side of the ceiling, and
// the reason it counts failures in a row rather than failures.
//
// A flaky link produces a steady trickle of failures over hours without ever
// meaning the server is unreachable. Counting them cumulatively would abandon
// an account of three quarters of a million messages over a fault rate of a
// hundredth of a per cent — the run would be killed by the very durability it
// was given.
func TestSteadyTroubleDoesNotEndAHealthyRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 200)

	// Forty failures over the run, twenty times the ceiling, but never two in
	// a row for the same message.
	count := &atomic.Int32{}
	report, err := syncFlaky(t, h, 1, 2, syncer.Options{GiveUpAfter: 10}, func(c imapx.Conn) imapx.Conn {
		return droppingSource{Conn: c, count: count, drop: func(n int32) bool { return n%5 == 0 }}
	}, nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want a run that survives intermittent trouble", err)
	}

	if copied, _, failed := report.Totals(); copied != len(want) || failed != 0 {
		t.Errorf("copied %d and failed %d, want %d copied and none failed", copied, failed, len(want))
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// TestGivingUpDoesNotLoseWhatWasAlreadyCopied checks that the ceiling stops the
// run without spoiling it. Giving up is only tolerable if the work already done
// survives, so that running again resumes rather than starts over — and, more
// importantly, does not copy anything twice.
func TestGivingUpDoesNotLoseWhatWasAlreadyCopied(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 200)

	// Accept the first thirty messages, then refuse everything.
	remaining := &atomic.Int32{}
	remaining.Store(30)
	attempts := &atomic.Int32{}
	first, err := syncFlaky(t, h, 1, 1, syncer.Options{GiveUpAfter: 5}, nil, func(c imapx.Conn) imapx.Conn {
		return failAfter{Conn: c, remaining: remaining, attempts: attempts}
	})
	if err == nil {
		t.Fatal("Run() returned no error against a destination that stopped accepting")
	}
	copied, _, _ := first.Totals()
	if copied < 25 {
		t.Fatalf("the abandoned run recorded %d messages copied, want about the 30 the destination accepted", copied)
	}

	// The same accounts again, with the destination working.
	second, err := syncFlaky(t, h, 2, 2, syncer.Options{}, nil, nil)
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}

	var already int
	for _, fr := range second.Folders {
		already += fr.AlreadyDone
	}
	if already != copied {
		t.Errorf("the second run treated %d messages as already copied, want the %d the first one recorded",
			already, copied)
	}
	assertExactly(t, h.dst, "INBOX", want)
}

type failAfter struct {
	imapx.Conn
	remaining *atomic.Int32
	attempts  *atomic.Int32
}

func (d failAfter) Append(ctx context.Context, mailbox string, msg imapx.AppendMessage) (imapx.AppendResult, error) {
	if d.remaining.Add(-1) >= 0 {
		return d.Conn.Append(ctx, mailbox, msg)
	}
	d.attempts.Add(1)
	return imapx.AppendResult{}, imapx.ErrConnectionBroken
}

// sulkingSource refuses to open a mailbox until it has been asked often enough.
type sulkingSource struct {
	imapx.Conn
	mailbox string
	until   int32
	count   *atomic.Int32
}

func (s sulkingSource) Select(ctx context.Context, mailbox string, opts imapx.SelectOptions) (imapx.Mailbox, error) {
	if mailbox == s.mailbox && s.count.Add(1) <= s.until {
		return imapx.Mailbox{}, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "mailbox is locked by another process"}
	}
	return s.Conn.Select(ctx, mailbox, opts)
}

// TestAFolderThatFailedIsTriedOnceMore is the second pass.
//
// A folder is usually abandoned for a reason that has stopped being true by the
// time the rest of the run has finished: a server restarting, a mailbox locked
// by another client, a burst of throttling. Trying again at the end costs one
// attempt and saves scheduling the entire run a second time.
func TestAFolderThatFailedIsTriedOnceMore(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps(), "Locked")
	want := fill(t, h.src, "Locked", 10)
	fill(t, h.src, "INBOX", 5)

	// Exactly one refusal, which is all it takes: nothing retries the opening
	// of a mailbox, so the folder is abandoned there and then. If the second
	// pass did not exist, Locked would end the run uncopied.
	count := &atomic.Int32{}
	rep, err := syncFlaky(t, h, 2, 2, syncer.Options{}, func(c imapx.Conn) imapx.Conn {
		return sulkingSource{Conn: c, mailbox: "Locked", until: 1, count: count}
	}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := folderReport(t, rep, "Locked"); got.Err != nil {
		t.Fatalf("Locked still failed after a second attempt: %v", got.Err)
	}
	assertExactly(t, h.dst, "Locked", want)
}

// TestTheSecondPassDoesNotStartOver checks the retry is a retry and not a
// second copy: the folders that already succeeded are not touched again.
func TestTheSecondPassDoesNotStartOver(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps(), "Locked")
	fill(t, h.src, "Locked", 4)
	want := fill(t, h.src, "INBOX", 20)

	count := &atomic.Int32{}
	rep, err := syncFlaky(t, h, 2, 2, syncer.Options{}, func(c imapx.Conn) imapx.Conn {
		return sulkingSource{Conn: c, mailbox: "Locked", until: 8, count: count}
	}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// INBOX succeeded the first time. If the second pass had re-run every
	// folder rather than the failed ones, its report would have been replaced
	// by one whose messages were all already done.
	inbox := folderReport(t, rep, "INBOX")
	if inbox.Copied != len(want) {
		t.Fatalf("INBOX reports %d copied, want %d — the second pass re-ran a folder that had succeeded", inbox.Copied, len(want))
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// TestGivingUpIsNotWorthASecondPass: when the run has been given up on, or
// cancelled, every folder failed for the same reason and that reason is still
// true. Trying again only produces the same errors more slowly.
func TestGivingUpIsNotWorthASecondPass(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 40)

	var appends atomic.Int32
	_, err := syncFlaky(t, h, 1, 1, syncer.Options{GiveUpAfter: 5}, nil, func(c imapx.Conn) imapx.Conn {
		return deadDest{Conn: c, attempts: &appends}
	})
	if err == nil {
		t.Fatal("want the run to give up")
	}

	// Five failures at four attempts each is twenty appends, plus whatever was
	// already in flight when the ceiling tripped. A second pass would double
	// it.
	if n := appends.Load(); n > 40 {
		t.Fatalf("%d append attempts after giving up: the run tried the folder again", n)
	}
}

// folderReport finds one folder in a report.
func folderReport(t *testing.T, rep syncer.Report, name string) syncer.FolderReport {
	t.Helper()
	for _, fr := range rep.Folders {
		if fr.Source == name {
			return fr
		}
	}
	t.Fatalf("no report for %q", name)
	return syncer.FolderReport{}
}

// records captures log records so a test can read what a run said.
type records struct {
	mu  sync.Mutex
	got []slog.Record
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }
func (r *records) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *records) WithGroup(string) slog.Handler            { return r }

func (r *records) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, rec.Clone())
	return nil
}

// find returns the values of one attribute across every record with a message.
func (r *records) find(msg, attr string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []string
	for _, rec := range r.got {
		if rec.Message != msg {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == attr {
				out = append(out, a.Value.String())
			}
			return true
		})
	}
	return out
}

// TestALongRunSaysWhatItIsDoing.
//
// The account this is built for takes hours, and its largest folder holds half
// of it, so a run that only speaks when a folder finishes can be silent for
// most of its life. There is no way to tell that apart from a hang.
func TestALongRunSaysWhatItIsDoing(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 60)

	log := &records{}
	_, err := syncFlaky(t, h, 1, 1, syncer.Options{
		ProgressEvery: time.Millisecond,
		Logger:        slog.New(log),
	}, func(c imapx.Conn) imapx.Conn {
		return slowSource{Conn: c, delay: 2 * time.Millisecond}
	}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	said := log.find("still going", "copied")
	if len(said) == 0 {
		t.Fatal("the run said nothing while it worked")
	}

	// The counts have to move, or the line is a heartbeat pretending to be
	// progress: it would look identical against a server that had stopped.
	if said[len(said)-1] == "0" {
		t.Fatalf("progress reported %v: the count never moved", said)
	}
}

// TestASilentRunCanBeAsked checks the reporting can be turned off, since a
// short interactive sync does not want a running commentary.
func TestASilentRunCanBeAsked(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 20)

	log := &records{}
	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{
		ProgressEvery: 0,
		Logger:        slog.New(log),
	}, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	if said := log.find("still going", "copied"); len(said) > 0 {
		t.Fatalf("progress was reported %d times with it switched off", len(said))
	}
}

// interruptingDest cancels the run once it has stored a message, which is the
// one instant where an abandoned state write costs something.
type interruptingDest struct {
	imapx.Conn
	after  int32
	n      *atomic.Int32
	cancel context.CancelFunc
}

func (d interruptingDest) Append(ctx context.Context, mailbox string, msg imapx.AppendMessage) (imapx.AppendResult, error) {
	res, err := d.Conn.Append(ctx, mailbox, msg)
	if err == nil && d.n.Add(1) == d.after {
		d.cancel()
	}
	return res, err
}

// TestAnInterruptedRunRecordsWhatItCopied.
//
// An append is finished on the server before the database hears about it, and
// an interrupt lands wherever it lands. If the write that records the copy is
// abandoned because the run was cancelled, the message is on the destination
// and the state database does not know: the next run has to rediscover it by
// searching, which is slower, and a message too weak to search for would be
// copied twice.
//
// The signature of the bug is adoption. A second run that has to adopt anything
// is a run recovering from a write that should have happened.
func TestAnInterruptedRunRecordsWhatItCopied(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 60)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := &atomic.Int32{}
	if _, err := syncFlakyCtx(t, ctx, h, 1, 1, syncer.Options{}, nil, func(c imapx.Conn) imapx.Conn {
		return interruptingDest{Conn: c, after: 20, n: n, cancel: cancel}
	}); err == nil {
		t.Fatal("an interrupted run reported success")
	}

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if got := folderReport(t, rep, "INBOX"); got.Adopted > 0 {
		t.Fatalf("the second run adopted %d messages: the first left them on the destination unrecorded",
			got.Adopted)
	}
	assertExactly(t, h.dst, "INBOX", want)
}
