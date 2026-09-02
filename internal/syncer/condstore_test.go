package syncer_test

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// modseqSource gives a mailbox the modification sequence the test wants.
//
// The in-process server does not implement CONDSTORE, so it reports no
// HIGHESTMODSEQ at all. What is under test is not the server's arithmetic but
// what the engine does with the number, and a decorator states the number
// exactly — including the case a real server makes hard to arrange, where the
// sequence has genuinely not moved.
type modseqSource struct {
	imapx.Conn
	seq  *atomic.Uint64
	uids *atomic.Int32
}

func (m modseqSource) Select(ctx context.Context, mailbox string, opts imapx.SelectOptions) (imapx.Mailbox, error) {
	box, err := m.Conn.Select(ctx, mailbox, opts)
	box.HighestModSeq = m.seq.Load()
	return box, err
}

// AllUIDs is counted because it is the thing the fast path exists to avoid: on
// a mailbox of 414k messages it is the expensive part of finding out there is
// nothing to do.
func (m modseqSource) AllUIDs(ctx context.Context) ([]uint32, error) {
	m.uids.Add(1)
	return m.Conn.AllUIDs(ctx)
}

func modseq(seq *atomic.Uint64, uids *atomic.Int32) func(imapx.Conn) imapx.Conn {
	return func(c imapx.Conn) imapx.Conn {
		return modseqSource{Conn: c, seq: seq, uids: uids}
	}
}

// TestAFolderNothingHasTouchedIsSkipped is the fast path.
//
// A modification sequence that has not moved means nothing was added, removed
// or reflagged. On an account of 144 folders the alternative is listing every
// UID of every mailbox to learn the same thing, against a server charging by
// the round trip.
func TestAFolderNothingHasTouchedIsSkipped(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 30)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := uids.Load()
	if first == 0 {
		t.Fatal("the first run never listed the source UIDs; the decorator is not in the path")
	}

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if got := uids.Load(); got != first {
		t.Errorf("the second run listed the source UIDs %d more times; the folder was not skipped", got-first)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 0 || fr.AlreadyDone != len(want) {
		t.Errorf("second run reported %d copied and %d already done, want 0 and %d", fr.Copied, fr.AlreadyDone, len(want))
	}
}

// TestAMailboxThatMovedIsNotSkipped checks the fast path is a fast path and not
// a way of never looking again.
func TestAMailboxThatMovedIsNotSkipped(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 10)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// New mail, and the sequence moves with it, which is what a server that
	// implements CONDSTORE guarantees.
	later := fill(t, h.src, "INBOX", 5)
	seq.Store(140)

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Copied != len(later) {
		t.Fatalf("second run copied %d, want the %d new messages", fr.Copied, len(later))
	}
}

// TestAFolderThatLeftMessagesBehindIsNotSkipped is the trap the fast path sets.
//
// A message that failed is not a tombstone: the next run tries it again. But
// the watermark says "everything the source held is at the destination", and
// writing it after a folder that left messages behind would make the failure
// permanent — the folder would be skipped from then on and those messages would
// never be looked at again.
func TestAFolderThatLeftMessagesBehindIsNotSkipped(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 12)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	// One message the destination will not take, in a way no retry can fix.
	var n atomic.Int32
	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), func(c imapx.Conn) imapx.Conn {
		return refuseNth{Conn: c, nth: 4, n: &n}
	})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Failed != 1 {
		t.Fatalf("first run reported %d failed, want exactly 1", fr.Failed)
	}

	// Same sequence, working destination. The folder must be looked at again.
	rep, err = syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Copied != 1 {
		t.Fatalf("second run copied %d, want the 1 message that failed", fr.Copied)
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// refuseNth rejects one message outright, in a way that is not worth retrying.
type refuseNth struct {
	imapx.Conn
	nth int32
	n   *atomic.Int32
}

func (d refuseNth) Append(ctx context.Context, mailbox string, msg imapx.AppendMessage) (imapx.AppendResult, error) {
	if d.n.Add(1) == d.nth {
		return imapx.AppendResult{}, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: "message too big",
		}
	}
	return d.Conn.Append(ctx, mailbox, msg)
}

// TestFullLooksAnyway is for a destination someone suspects has drifted.
// Nothing about the source can reveal a message deleted at the far end.
func TestFullLooksAnyway(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 10)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := uids.Load()

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{Full: true}, modseq(seq, uids), nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if uids.Load() == first {
		t.Fatal("--full skipped the folder anyway")
	}
}

// TestARenumberedMailboxIsNotSkipped.
//
// Modification sequences are only comparable within one UIDVALIDITY. A server
// that renumbers a mailbox is entitled to hand back a sequence that looks
// familiar, and every UID we hold now means something else or nothing. Skipping
// on the strength of the number alone would leave a whole mailbox unexamined.
func TestARenumberedMailboxIsNotSkipped(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 8)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}
	bump := &atomic.Uint32{}

	wrap := func(c imapx.Conn) imapx.Conn {
		return renumbered{Conn: modseqSource{Conn: c, seq: seq, uids: uids}, by: bump}
	}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, wrap, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := uids.Load()

	// Same sequence, different mailbox.
	bump.Store(1)

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, wrap, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if uids.Load() == first {
		t.Fatal("a renumbered mailbox was skipped on the strength of its modification sequence")
	}

	// The fence threw away every UID mapping, so the messages are found again
	// at the destination and adopted rather than copied twice.
	if fr := folderReport(t, rep, "INBOX"); fr.Adopted != len(want) {
		t.Errorf("second run adopted %d of %d after renumbering", fr.Adopted, len(want))
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// renumbered shifts a mailbox's UIDVALIDITY, which is how a server says the
// UIDs you remember are no longer the ones it means.
type renumbered struct {
	imapx.Conn
	by *atomic.Uint32
}

func (r renumbered) Select(ctx context.Context, mailbox string, opts imapx.SelectOptions) (imapx.Mailbox, error) {
	box, err := r.Conn.Select(ctx, mailbox, opts)
	box.UIDValidity += r.by.Load()
	return box, err
}

// TestTheFastPathDoesNotHideAMissingCopy is the failure a real server found and
// this package could not.
//
// Every test of the destination check was written without CONDSTORE, so every
// one of them took the slow path, and all five passed against an engine that
// did nothing whatever against mox: the fast path returned before the check ran,
// and a real server has CONDSTORE. Worse, the fast path is at its most confident
// exactly when the check matters most — deleting a copy from the destination
// moves nothing on the source, so the modification sequence sits where the last
// run left it and the folder looks untouched.
//
// A sequence that does not move is therefore the whole point of this test
// rather than a convenience.
func TestTheFastPathDoesNotHideAMissingCopy(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.src.stuff(t, "INBOX", testMessage("two", "two@example.test"))

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// The fast path has to be reachable, or this test proves nothing about it.
	before := uids.Load()
	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if uids.Load() != before {
		t.Fatal("an untouched folder still listed the source UIDs; the fast path was not " +
			"taken, so this test cannot see the bug it exists for")
	}

	removeFrom(t, h.dst, "INBOX", uidOf(t, h.dst, "INBOX", "two"))

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Missing != 1 {
		t.Errorf("reported %d missing copies, want 1", fr.Missing)
	}
	if fr.Copied != 1 {
		t.Errorf("copied %d, want the missing message back", fr.Copied)
	}
	if got := sortedSubjects(t, h.dst); !slices.Equal(got, []string{"one", "two"}) {
		t.Errorf("destination holds %v, want exactly one copy of each message", got)
	}
}

// TestTheFastPathStillSkipsAHealthyFolder keeps the check honest about its cost.
//
// The case for taking the destination's message count from the LIST the plan
// already does — rather than a SELECT per folder — is that a healthy folder
// pays nothing. A healthy folder that starts enumerating has given that away,
// and the source UID count is the same measure the fast path's own test uses.
//
// The stranger matters: it pushes the destination's count above what is
// claimed, which is the direction that proves nothing. Reacting to it would
// mean enumerating every folder of every run on any account whose destination
// holds mail this tool did not put there.
func TestTheFastPathStillSkipsAHealthyFolder(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	h.dst.stuff(t, "INBOX", testMessage("stranger", "stranger@example.test"))

	before := uids.Load()
	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := uids.Load(); got != before {
		t.Errorf("a folder that lost nothing listed the source UIDs %d more times; "+
			"the destination's count must only ever be read as a floor", got-before)
	}
	// AlreadyDone is 1 rather than 0 because a skipped folder still reports what
	// it holds as settled, which is what TestAFolderNothingHasTouchedIsSkipped
	// pins. The measure of skipping here is the UID listing above.
	fr := folderReport(t, rep, "INBOX")
	if fr.Missing != 0 || fr.Copied != 0 || fr.AlreadyDone != 1 {
		t.Errorf("missing=%d copied=%d already=%d; a healthy folder must still be skipped whole",
			fr.Missing, fr.Copied, fr.AlreadyDone)
	}
}

// TestTheFastPathIgnoresTheDestinationWhenVerificationIsOff gives the fast path
// back exactly as it was, blindness included. Someone who prunes the
// destination deliberately is asking for precisely that.
func TestTheFastPathIgnoresTheDestinationWhenVerificationIsOff(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.src.stuff(t, "INBOX", testMessage("two", "two@example.test"))

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	opts := syncer.Options{NoVerifyDest: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, modseq(seq, uids), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	removeFrom(t, h.dst, "INBOX", uidOf(t, h.dst, "INBOX", "two"))

	rep, err := syncFlaky(t, h, 1, 1, opts, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Missing != 0 || fr.Copied != 0 {
		t.Errorf("missing=%d copied=%d; --verify-dest=false must leave the deletion alone",
			fr.Missing, fr.Copied)
	}
	if got := len(h.dst.contents(t, "INBOX")); got != 1 {
		t.Errorf("destination holds %d messages, want the deletion left alone", got)
	}
}

// silentStatus is a destination that lists its mailboxes but will not say how
// many messages they hold.
//
// LIST-STATUS is an extension, and STATUS itself may be answered for some
// mailboxes and not others. What the count is used for is deciding whether a
// folder that looks untouched is worth a closer look, so a server that will not
// answer must cost nothing: the fast path has to work exactly as it did before
// any of this existed.
type silentStatus struct {
	imapx.Conn
	asked *atomic.Int32
}

func (s silentStatus) ListFolders(ctx context.Context, opts imapx.ListOptions) ([]imapx.Folder, error) {
	if opts.WithStatus {
		s.asked.Add(1)
	}
	folders, err := s.Conn.ListFolders(ctx, opts)
	for i := range folders {
		folders[i].NumMessages = nil
	}
	return folders, err
}

// TestAServerThatWillNotCountItsMessagesKeepsTheFastPath pins the fallback.
//
// Reading "no answer" as "something may be missing" would be the safe-looking
// choice and the wrong one: it would put every folder of every run down the
// slow path on any server without a usable STATUS, turning a check that is
// supposed to be free into the most expensive thing in the run. --full remains
// the answer for anyone on such a server who suspects drift.
func TestAServerThatWillNotCountItsMessagesKeepsTheFastPath(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}
	asked := &atomic.Int32{}
	quiet := func(c imapx.Conn) imapx.Conn { return silentStatus{Conn: c, asked: asked} }

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), quiet); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if asked.Load() == 0 {
		t.Fatal("the run never asked the destination for message counts; the decorator is not in the path")
	}

	before := uids.Load()
	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), quiet)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := uids.Load(); got != before {
		t.Errorf("a server that would not count its messages cost %d extra source UID listings; "+
			"an unanswered STATUS must leave the fast path exactly as it was", got-before)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Copied != 0 || fr.Missing != 0 {
		t.Errorf("copied=%d missing=%d, want an untouched folder skipped", fr.Copied, fr.Missing)
	}
}
