package syncer_test

import (
	"context"
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
