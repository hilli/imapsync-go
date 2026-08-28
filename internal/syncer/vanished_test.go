package syncer_test

import (
	"context"
	"io"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// ghostSource lists UIDs the mailbox does not contain.
//
// This is not a hypothetical. iCloud's SEARCH ALL on a 414,053-message INBOX
// returns 503,786 distinct UIDs, and a UID FETCH of 500 of them comes back with
// 358. The extra numbers are not errors and not expunges observed mid-run: the
// server simply lists them and then has nothing to say about them.
type ghostSource struct {
	imapx.Conn
	ghosts []uint32
	metaOf *atomic.Int32
}

func (g ghostSource) AllUIDs(ctx context.Context) ([]uint32, error) {
	real, err := g.Conn.AllUIDs(ctx)
	if err != nil {
		return nil, err
	}
	return append(slices.Clone(real), g.ghosts...), nil
}

// FetchMeta counts how many UIDs were asked about, which is the cost a phantom
// imposes on every later run if nothing writes down that it is not there.
func (g ghostSource) FetchMeta(ctx context.Context, uids []uint32, fields []string) ([]imapx.MessageMeta, error) {
	if g.metaOf != nil {
		g.metaOf.Add(int32(len(uids)))
	}
	return g.Conn.FetchMeta(ctx, uids, fields)
}

func haunted(ghosts []uint32, metaOf *atomic.Int32) func(imapx.Conn) imapx.Conn {
	return func(c imapx.Conn) imapx.Conn {
		return ghostSource{Conn: c, ghosts: ghosts, metaOf: metaOf}
	}
}

// TestAUIDWithNoMessageBehindItIsNotAFailure.
//
// The distinction matters twice over. A failure goes back in the queue on the
// next run, and a folder with one keeps its old watermark for ever — so calling
// this a failure would mean iCloud's INBOX is re-diffed on every run until the
// end of time, on account of ninety thousand numbers that will never have
// anything behind them.
func TestAUIDWithNoMessageBehindItIsNotAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 12)

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, haunted([]uint32{9001, 9002, 9003}, nil), nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	fr := folderReport(t, rep, "INBOX")
	if fr.Failed != 0 {
		t.Errorf("reported %d failed, want 0: a UID with no message behind it is not a failure", fr.Failed)
	}
	if fr.Copied != len(want) {
		t.Errorf("copied %d of %d real messages", fr.Copied, len(want))
	}
	if fr.Vanished != 3 {
		t.Errorf("reported %d vanished, want 3", fr.Vanished)
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// TestAPhantomUIDIsNotAskedForTwice is the whole point of writing it down.
//
// Before this, the numbers were silently dropped from the fetch response and
// nothing recorded them, so every run re-enumerated them and asked the server
// for their headers again — on iCloud, ninety thousand of them, in chunks, for
// ever.
func TestAPhantomUIDIsNotAskedForTwice(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 12)

	ghosts := []uint32{9001, 9002, 9003, 9004}
	asked := &atomic.Int32{}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, haunted(ghosts, asked), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := asked.Load()
	if first == 0 {
		t.Fatal("the first run never fetched metadata; the decorator is not in the path")
	}

	// --full so the fast path cannot hide the answer: this must hold because
	// the phantoms are recorded, not because the folder was skipped wholesale.
	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{Full: true}, haunted(ghosts, asked), nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := asked.Load(); got != first {
		t.Errorf("the second run asked about %d more UIDs, want 0: the phantoms were not remembered", got-first)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Vanished != len(ghosts) {
		t.Errorf("second run reported %d vanished, want %d: the count must reconcile on every run, not just the first", fr.Vanished, len(ghosts))
	}
	if fr.Copied != 0 || fr.Failed != 0 {
		t.Errorf("second run copied %d and failed %d, want 0 and 0", fr.Copied, fr.Failed)
	}
}

// TestAPhantomDoesNotStopAFolderBeingSkipped.
//
// The watermark means everything the source held is on the destination, and a
// number the source does not hold cannot stand in the way of that. If it did,
// the folder iCloud makes this worst on — the 414k INBOX — would be the one
// folder that never gets the fast path.
func TestAPhantomDoesNotStopAFolderBeingSkipped(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 8)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}
	wrap := func(c imapx.Conn) imapx.Conn {
		return ghostSource{Conn: modseq(seq, uids)(c), ghosts: []uint32{9001, 9002}}
	}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, wrap, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := uids.Load()

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, wrap, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := uids.Load(); got != before {
		t.Errorf("the second run listed the source UIDs again; the phantoms stopped the watermark advancing")
	}
	if fr := folderReport(t, rep, "INBOX"); fr.AlreadyDone != 8 {
		t.Errorf("second run reported %d already done, want 8", fr.AlreadyDone)
	}
}

// vanishingBody hands over metadata and then loses the message, which is the
// genuine mid-run expunge as opposed to a UID that was never real.
type vanishingBody struct {
	imapx.Conn
	uid *atomic.Uint32
}

func (v vanishingBody) FetchBody(ctx context.Context, uid uint32, w io.Writer) (int64, error) {
	if uid == v.uid.Load() {
		return 0, imapx.ErrMessageGone
	}
	return v.Conn.FetchBody(ctx, uid, w)
}

// TestAMessageExpungedMidRunIsNotAFailureEither.
//
// This path recorded a failure and its comment said it was doing so "to stop
// the next run retrying forever" — which is exactly what recording a failure
// does not achieve, since a failed UID goes back in the queue every run and
// holds the folder's watermark down with it.
func TestAMessageExpungedMidRunIsNotAFailureEither(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 6)

	doomed := &atomic.Uint32{}
	metaOnly := func(c imapx.Conn) imapx.Conn { return vanishingBody{Conn: c, uid: doomed} }

	// Learn a real UID, then make that one disappear between metadata and body.
	ctx := context.Background()
	probe := h.src.dial(t)
	if _, err := probe.Select(ctx, "INBOX", imapx.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("selecting source: %v", err)
	}
	all, err := probe.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("listing source UIDs: %v", err)
	}
	doomed.Store(all[2])

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, metaOnly, nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Failed != 0 {
		t.Errorf("reported %d failed, want 0: an expunged message is gone, not a failed copy", fr.Failed)
	}
	if fr.Vanished != 1 {
		t.Errorf("reported %d vanished, want 1", fr.Vanished)
	}
	if fr.Copied != 5 {
		t.Errorf("copied %d, want 5", fr.Copied)
	}

	// And it is not tried again. The body would now be readable, so a second
	// copy is exactly what a UID left in the queue would produce.
	doomed.Store(0)
	second, err := syncFlaky(t, h, 1, 1, syncer.Options{Full: true}, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if fr := folderReport(t, second, "INBOX"); fr.Copied != 0 {
		t.Errorf("second run copied %d, want 0: the expunged UID went back in the queue", fr.Copied)
	}
}
