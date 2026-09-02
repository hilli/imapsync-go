package syncer_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// removeFrom expunges a message from a mailbox on the given side, which is how
// these tests make the source "no longer have" something.
func removeFrom(t *testing.T, a account, mailbox string, uid uint32) {
	t.Helper()

	ctx := context.Background()
	conn := a.dial(t)
	if _, err := conn.Select(ctx, mailbox, imapx.SelectOptions{}); err != nil {
		t.Fatalf("selecting %q: %v", mailbox, err)
	}
	if err := conn.DeleteMessages(ctx, []uint32{uid}); err != nil {
		t.Fatalf("deleting UID %d from %q: %v", uid, mailbox, err)
	}
}

// uidsIn lists the UIDs a mailbox currently holds.
func uidsIn(t *testing.T, a account, mailbox string) []uint32 {
	t.Helper()

	ctx := context.Background()
	conn := a.dial(t)
	if _, err := conn.Select(ctx, mailbox, imapx.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("selecting %q: %v", mailbox, err)
	}
	uids, err := conn.AllUIDs(ctx)
	if err != nil {
		t.Fatalf("listing %q: %v", mailbox, err)
	}
	return uids
}

// newDeleteHarness builds a pair whose two sides do not number their messages
// alike.
//
// Every deletion here turns on matching a recorded source UID against the
// source's own listing, and a harness where source UID 7 is also destination
// UID 7 cannot tell that apart from matching the destination UID by mistake —
// which in the field would delete whichever messages happened to sit at those
// numbers. Shifting the destination's numbering makes the confusion visible.
func newDeleteHarness(t *testing.T) *harness {
	t.Helper()

	h := newHarness(t, rev2Caps())
	for i := range 5 {
		h.dst.stuff(t, "INBOX", testMessage(fmt.Sprintf("shim-%04d", i), fmt.Sprintf("shim%d@nowhere.test", i)))
	}
	for _, uid := range uidsIn(t, h.dst, "INBOX") {
		removeFrom(t, h.dst, "INBOX", uid)
	}
	return h
}

// TestDeletingIsOffUnlessAskedFor.
//
// The default has to be that nothing is destroyed. A tool that mirrors
// deletions by default turns a mistaken deletion on the source into a
// permanent one everywhere, which is the failure mode a backup exists to
// prevent.
func TestDeletingIsOffUnlessAskedFor(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	want := fill(t, h.src, "INBOX", 6)

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	removeFrom(t, h.src, "INBOX", uidsIn(t, h.src, "INBOX")[0])

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{Full: true}, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Deleted != 0 {
		t.Errorf("deleted %d messages without being asked to", fr.Deleted)
	}
	// Nothing refused either. Off has to mean the machinery never ran, not that
	// it ran and the ceiling happened to catch it — the source listing is not
	// even gathered when deletion is off, so every recorded message would look
	// like a candidate, and only the ceiling would be standing in the way.
	if fr.Refused != 0 {
		t.Errorf("nominated %d messages for deletion without being asked to", fr.Refused)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != len(want) {
		t.Errorf("destination holds %d messages, want %d untouched", got, len(want))
	}
}

// TestDeletingFollowsTheSourceWhenAskedFor is the feature working.
func TestDeletingFollowsTheSourceWhenAskedFor(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 20)

	opts := syncer.Options{Delete2: true, Full: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := len(uidsIn(t, h.dst, "INBOX"))

	removeFrom(t, h.src, "INBOX", uidsIn(t, h.src, "INBOX")[0])

	rep, err := syncFlaky(t, h, 1, 1, opts, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 1 {
		t.Errorf("reported %d deleted, want 1", fr.Deleted)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != before-1 {
		t.Errorf("destination holds %d messages, want %d", got, before-1)
	}

	// And it settles. A third run must find nothing left to do, which is what
	// proves the state row went with the message rather than lingering to
	// nominate an already-deleted UID every run thereafter.
	third, err := syncFlaky(t, h, 1, 1, opts, nil, nil)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if fr := folderReport(t, third, "INBOX"); fr.Deleted != 0 || fr.Failed != 0 {
		t.Errorf("third run deleted %d and failed %d, want 0 and 0", fr.Deleted, fr.Failed)
	}
}

// TestDeletingReachesMailItDidNotCopy is imapsync's --delete2, and the reason
// it is worth the extra care: somebody moving over from imapsync expects a
// destination that ends up looking like the source. A tool that quietly left
// behind every message it had not personally copied would be a worse surprise
// than one that deletes, because the surprise arrives silently and only shows
// up as a mailbox that never quite matches.
//
// The handle on mail nobody recorded copying is its identity — the same digest
// adoption matches on. If the source has nothing that looks like it, it goes.
func TestDeletingReachesMailItDidNotCopy(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 10)

	// Mail that was on the destination first, and has no counterpart anywhere.
	for i := range 4 {
		h.dst.stuff(t, "INBOX", testMessage(fmt.Sprintf("stranger-%04d", i), fmt.Sprintf("x%d@elsewhere.test", i)))
	}

	opts := syncer.Options{Delete2: true, Full: true, Force: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}

	left := subjects(h.dst.contents(t, "INBOX"))
	for _, name := range left {
		if strings.HasPrefix(name, "stranger-") {
			t.Errorf("%q is still there; the destination does not look like the source", name)
		}
	}
	if len(left) != 10 {
		t.Errorf("destination holds %d messages, want the source's 10", len(left))
	}
}

// TestAMessageTooThinToIdentifyIsLeftAlone is where this stops short of
// imapsync, deliberately.
//
// Identity is the only handle on mail nobody recorded copying, and some
// messages carry too little header to be told apart from any other. Adoption
// already refuses to match on those, because a wrong match drops mail. Deleting
// on one is the same mistake with no way back: "the source has nothing like
// this" cannot be concluded from a digest that could not tell the difference
// either way.
func TestAMessageTooThinToIdentifyIsLeftAlone(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 10)

	// No Message-ID, no Date, no From — nothing to be sure with.
	h.dst.stuff(t, "INBOX", []byte("Subject: thin\r\n\r\nbody\r\n"))

	opts := syncer.Options{Delete2: true, Full: true, Force: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	if left := subjects(h.dst.contents(t, "INBOX")); !slices.Contains(left, "thin") {
		t.Errorf("deleted a message it could not identify, on the strength of not recognising it")
	}
}

// TestDeletingStopsAtTheCeiling.
//
// The catastrophe this guards against is a source that answers a UID listing
// with nothing, or with a fraction of the truth. Copying nothing is harmless
// and fixes itself on the next run; deleting everything does neither.
func TestDeletingStopsAtTheCeiling(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 80)

	opts := syncer.Options{Delete2: true, Full: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := len(uidsIn(t, h.dst, "INBOX"))

	// Twenty of eighty is a quarter, over the tenth allowed unasked, and well
	// clear of the handful the floor lets through whatever the share.
	for _, uid := range uidsIn(t, h.src, "INBOX")[:20] {
		removeFrom(t, h.src, "INBOX", uid)
	}

	rep, err := syncFlaky(t, h, 1, 1, opts, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Deleted != 0 {
		t.Errorf("deleted %d messages past the ceiling", fr.Deleted)
	}
	if fr.Refused != 20 {
		t.Errorf("reported %d refused, want 20", fr.Refused)
	}
	if fr.Failed != 0 {
		t.Errorf("reported %d failed: refusing is not a failure", fr.Failed)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != before {
		t.Errorf("destination holds %d messages, want %d untouched", got, before)
	}

	// Told again, it goes ahead.
	forced := syncer.Options{Delete2: true, Full: true, Force: true}
	second, err := syncFlaky(t, h, 1, 1, forced, nil, nil)
	if err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if fr := folderReport(t, second, "INBOX"); fr.Deleted != 20 {
		t.Errorf("forced run deleted %d, want 20", fr.Deleted)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != before-20 {
		t.Errorf("destination holds %d messages, want %d", got, before-20)
	}
}

// TestAnEmptySourceListingDeletesNothing is the ceiling doing the job it was
// built for, stated as the disaster rather than as a percentage.
func TestAnEmptySourceListingDeletesNothing(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 12)

	opts := syncer.Options{Delete2: true, Full: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}

	for _, uid := range uidsIn(t, h.src, "INBOX") {
		removeFrom(t, h.src, "INBOX", uid)
	}

	rep, err := syncFlaky(t, h, 1, 1, opts, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 0 || fr.Refused != 12 {
		t.Errorf("deleted %d and refused %d, want 0 and 12", fr.Deleted, fr.Refused)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != 12 {
		t.Errorf("destination holds %d messages, want 12: a source that lists nothing emptied the mirror", got)
	}
}

// TestADryRunDeletesNothingButSaysWhatItWould.
//
// Previewing a destructive run is the reason dry runs exist, so a preview that
// omits the destruction is worse than none: it reads as reassurance.
func TestADryRunDeletesNothingButSaysWhatItWould(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 20)

	opts := syncer.Options{Delete2: true, Full: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := len(uidsIn(t, h.dst, "INBOX"))
	removeFrom(t, h.src, "INBOX", uidsIn(t, h.src, "INBOX")[0])

	dry := syncer.Options{Delete2: true, Full: true, DryRun: true}
	rep, err := syncFlaky(t, h, 1, 1, dry, nil, nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 1 {
		t.Errorf("dry run reported %d deletions, want 1", fr.Deleted)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != before {
		t.Errorf("the dry run deleted %d messages", before-got)
	}
}

// TestADryRunReportsWhatTheCeilingWouldRefuse, so that the number a user is
// being asked to approve is the one they will get.
func TestADryRunReportsWhatTheCeilingWouldRefuse(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 80)

	opts := syncer.Options{Delete2: true, Full: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	for _, uid := range uidsIn(t, h.src, "INBOX")[:20] {
		removeFrom(t, h.src, "INBOX", uid)
	}

	dry := syncer.Options{Delete2: true, Full: true, DryRun: true}
	rep, err := syncFlaky(t, h, 1, 1, dry, nil, nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Refused != 20 || fr.Deleted != 0 {
		t.Errorf("dry run reported %d refused and %d deleted, want 20 and 0", fr.Refused, fr.Deleted)
	}
}

// TestTheCeilingIsAdjustable, because ten per cent of a mailbox is a guess and
// a run that legitimately drops a third of a folder should be able to say so
// without waiving the guard entirely.
func TestTheCeilingIsAdjustable(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 80)

	opts := syncer.Options{Delete2: true, Full: true, DeleteCeiling: 0.5}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	for _, uid := range uidsIn(t, h.src, "INBOX")[:20] {
		removeFrom(t, h.src, "INBOX", uid)
	}

	rep, err := syncFlaky(t, h, 1, 1, opts, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 20 || fr.Refused != 0 {
		t.Errorf("deleted %d and refused %d, want 20 and 0 under a half ceiling", fr.Deleted, fr.Refused)
	}
}

// TestAFolderThatFailedIsNotDeletedFrom.
//
// A folder that could not copy everything has a picture of the source that is
// already known to be incomplete, and deleting on the strength of an incomplete
// picture is precisely backwards.
func TestAFolderThatFailedIsNotDeletedFrom(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 20)

	opts := syncer.Options{Delete2: true, Full: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := len(uidsIn(t, h.dst, "INBOX"))

	// One source message goes, and one new one arrives that the destination
	// will refuse, so the folder both wants to delete and fails to copy.
	removeFrom(t, h.src, "INBOX", uidsIn(t, h.src, "INBOX")[0])
	newcomer := fill(t, h.src, "INBOX", 1)

	tries := &atomic.Int32{}
	refuse := func(c imapx.Conn) imapx.Conn {
		return refusingDest{Conn: c, subject: newcomer[0], tries: tries}
	}
	rep, err := syncFlaky(t, h, 1, 1, opts, nil, refuse)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Failed == 0 {
		t.Fatal("the append was not refused; this test is not testing what it claims")
	}
	if fr.Deleted != 0 {
		t.Errorf("deleted %d messages from a folder that failed to copy", fr.Deleted)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != before {
		t.Errorf("destination holds %d messages, want %d untouched", got, before)
	}
}

// cannotExpunge is a destination whose server has no UID EXPUNGE.
type cannotExpunge struct{ imapx.Conn }

func (c cannotExpunge) DeleteMessages(context.Context, []uint32) error {
	return imapx.ErrNoUIDExpunge
}

// TestDeletingStopsWhenTheServerCannotExpungeByUID.
//
// Without UID EXPUNGE the only way to purge is a plain EXPUNGE, which is
// defined to remove every message carrying \\Deleted — including whatever the
// account's owner marked and has not yet got round to. The client refuses; what
// this checks is that the engine passes that refusal up rather than shrugging
// and carrying on as though the mirror were in step.
func TestDeletingStopsWhenTheServerCannotExpungeByUID(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 10)

	opts := syncer.Options{Delete2: true, Full: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := len(uidsIn(t, h.dst, "INBOX"))
	removeFrom(t, h.src, "INBOX", uidsIn(t, h.src, "INBOX")[0])

	refuse := func(c imapx.Conn) imapx.Conn { return cannotExpunge{Conn: c} }
	rep, err := syncFlaky(t, h, 1, 1, opts, nil, refuse)
	if err != nil {
		t.Fatalf("Run() error = %v: one folder's refusal should not end the run", err)
	}
	fr := folderReport(t, rep, "INBOX")
	if !errors.Is(fr.Err, imapx.ErrNoUIDExpunge) {
		t.Errorf("folder error = %v, want ErrNoUIDExpunge", fr.Err)
	}
	if fr.Deleted != 0 {
		t.Errorf("reported %d deleted, want 0", fr.Deleted)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != before {
		t.Errorf("destination holds %d messages, want %d untouched", got, before)
	}
}

// TestASmallFolderMayStillLoseAMessage pins the floor.
//
// A percentage guard is worthless on small numbers: one message out of six is
// 16.7%, and refusing that would mean every small folder needs --force, which
// in practice means --force is always on and the guard protects nothing. The
// floor is what keeps the ceiling credible.
func TestASmallFolderMayStillLoseAMessage(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 6)

	opts := syncer.Options{Delete2: true, Full: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	removeFrom(t, h.src, "INBOX", uidsIn(t, h.src, "INBOX")[0])

	rep, err := syncFlaky(t, h, 1, 1, opts, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Deleted != 1 || fr.Refused != 0 {
		t.Errorf("deleted %d and refused %d: one of six is 16.7%%, and refusing it would make the guard unusable", fr.Deleted, fr.Refused)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != 5 {
		t.Errorf("destination holds %d messages, want 5", got)
	}
}

// TestTurningDeletionOnLooksAtFoldersThatLookUnchanged is a regression test for
// a hole that only real servers found.
//
// The natural way to adopt --delete2 is to have been syncing without it for a
// while and then add the flag. Every folder's watermark is current at that
// point, so a fast path that trusts the watermark alone skips all of them —
// and any deletion the source made in the meantime is never carried out, on
// that run or any later one. The mirror is silently, permanently wrong.
//
// A watermark records that copying is up to date. It says nothing about
// deleting, and this is the difference.
func TestTurningDeletionOnLooksAtFoldersThatLookUnchanged(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 20)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	// Months of syncing without --delete2.
	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// A message leaves the source. A real server bumps its modseq for this,
	// as RFC 7162 requires of any server that keeps them.
	removeFrom(t, h.src, "INBOX", uidsIn(t, h.src, "INBOX")[0])
	seq.Store(200)

	// One more run without the flag, which brings the watermark up to date and
	// carries out no deletion at all. This is the run that used to poison the
	// state.
	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil); err != nil {
		t.Fatalf("catch-up run: %v", err)
	}

	// Now the flag goes on, and nothing about the source has changed since.
	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{Delete2: true}, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("first deleting run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 1 {
		t.Errorf("deleted %d, want 1: the folder looked unchanged and the deletion was lost", fr.Deleted)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != 19 {
		t.Errorf("destination holds %d messages, want 19", got)
	}

	// And once deletion has been carried out at that watermark, the folder is
	// allowed to be skipped again — otherwise --delete2 would mean giving up
	// the fast path for ever.
	before := uids.Load()
	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{Delete2: true}, modseq(seq, uids), nil); err != nil {
		t.Fatalf("second deleting run: %v", err)
	}
	if uids.Load() != before {
		t.Errorf("the source was listed again; --delete2 gave up the fast path permanently")
	}
}

// TestARefusalDoesNotBecomePermanent is the same hole seen from the other side.
//
// If a refused deletion advanced the deletion watermark, the folder would look
// settled on the next run, be skipped, and never be offered again — so the
// safety valve would quietly turn into the thing it was protecting against.
func TestARefusalDoesNotBecomePermanent(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 80)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}
	opts := syncer.Options{Delete2: true}

	if _, err := syncFlaky(t, h, 1, 1, opts, modseq(seq, uids), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	for _, uid := range uidsIn(t, h.src, "INBOX")[:20] {
		removeFrom(t, h.src, "INBOX", uid)
	}
	seq.Store(200)

	if rep, err := syncFlaky(t, h, 1, 1, opts, modseq(seq, uids), nil); err != nil {
		t.Fatalf("refusing run: %v", err)
	} else if fr := folderReport(t, rep, "INBOX"); fr.Refused != 20 {
		t.Fatalf("refused %d, want 20", fr.Refused)
	}

	// Nothing has changed on the source since the refusal, so a watermark-only
	// fast path would skip this folder and the refusal would be final.
	forced := syncer.Options{Delete2: true, Force: true}
	rep, err := syncFlaky(t, h, 1, 1, forced, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 20 {
		t.Errorf("deleted %d, want 20: the refusal was never offered again", fr.Deleted)
	}
}

// TestASecondCopyOfSomethingTheSourceHasIsKept guards the other direction.
//
// Identity is what decides, and a destination holding two copies of a message
// the source holds once has one claimed copy and one nobody recorded. The
// unrecorded one still matches something the source has, so it stays. The
// source is not being mirrored message-for-message; it is being asked whether
// it has anything like this.
//
// This also proves the identity set is actually populated. If it came back
// empty — the failure mode where nothing on the destination can be recognised —
// this message would be deleted and the test would say so.
func TestASecondCopyOfSomethingTheSourceHasIsKept(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 10)
	twin := testMessage("twin", "twin@example.test")
	h.src.stuff(t, "INBOX", twin)

	// Duplicate removal is off, because this test is about the other rule.
	// --delete2 implies --delete2duplicates, which would take the second copy
	// away for repeating the first rather than for anything to do with the
	// source, and a test that cannot say which rule acted proves neither.
	// TestDelete2ImpliesRemovingDuplicates covers the implication.
	opts := syncer.Options{Delete2: true, Full: true, Force: true, NoDelete2Duplicates: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// A second copy of one the source still holds, arriving by some other
	// route. Same message, so the same identity.
	h.dst.stuff(t, "INBOX", twin)

	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}

	left := subjects(h.dst.contents(t, "INBOX"))
	if got := len(left); got != 12 {
		t.Errorf("destination holds %d messages, want 12: a copy of something the source has was deleted", got)
	}
}

// TestBothCopiesGoWhenTheSourceLosesTheOriginal is where the two paths cross,
// and the only place the difference between "the source holds this" and "the
// source once held this" is visible.
//
// A destination with two copies of a message has one the state database knows
// about and one it does not. When the source deletes the message, the recorded
// copy goes because its UID is no longer listed. The unrecorded copy is judged
// on identity — and if that judgement were made against every identity ever
// recorded rather than the ones the source still holds, it would match a row
// describing a message that no longer exists and stay behind for ever.
func TestBothCopiesGoWhenTheSourceLosesTheOriginal(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 10)
	twin := testMessage("twin", "twin@example.test")
	h.src.stuff(t, "INBOX", twin)

	opts := syncer.Options{Delete2: true, Full: true, Force: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// A second copy nothing recorded, then the source loses the original.
	h.dst.stuff(t, "INBOX", twin)
	for _, uid := range uidsIn(t, h.src, "INBOX") {
		if subjectOf(t, h.src, uid) == "twin" {
			removeFrom(t, h.src, "INBOX", uid)
		}
	}

	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}

	left := subjects(h.dst.contents(t, "INBOX"))
	if slices.Contains(left, "twin") {
		t.Errorf("a copy of a message the source no longer has is still there: %v", left)
	}
	if len(left) != 10 {
		t.Errorf("destination holds %d messages, want 10", len(left))
	}
}

// subjectOf reads one message's subject, so a test can name the message it
// means rather than guessing at a UID.
func subjectOf(t *testing.T, a account, uid uint32) string {
	t.Helper()

	ctx := context.Background()
	c := a.dial(t)
	if _, err := c.Select(ctx, "INBOX", imapx.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("selecting: %v", err)
	}
	metas, err := c.FetchMeta(ctx, []uint32{uid}, []string{"SUBJECT"})
	if err != nil || len(metas) == 0 {
		t.Fatalf("reading subject of %d: %v", uid, err)
	}
	return strings.TrimSpace(strings.TrimPrefix(string(metas[0].Header), "Subject:"))
}

// TestTheShareIsOfTheWholeDestination pins the denominator of the safety valve.
//
// It is the destination folder, not the map of what was copied, because the
// destination folder is what is at stake now that mail nobody recorded is in
// scope. Measuring against the map instead would report a larger share than is
// really going and refuse ordinary runs; measuring against a folder full of
// mail from elsewhere is the honest fraction of what stands to be lost.
func TestTheShareIsOfTheWholeDestination(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 80)
	for i := range 20 {
		h.dst.stuff(t, "INBOX", testMessage(fmt.Sprintf("elsewhere-%04d", i), fmt.Sprintf("e%d@elsewhere.test", i)))
	}

	// Twenty of a hundred is a fifth; twenty of the eighty that were copied is
	// a quarter. A ceiling between the two only holds if the denominator is the
	// folder.
	opts := syncer.Options{Delete2: true, Full: true, DeleteCeiling: 0.22}
	rep, err := syncFlaky(t, h, 1, 1, opts, nil, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Deleted != 20 || fr.Refused != 0 {
		t.Errorf("deleted %d and refused %d, want 20 and 0: the share was measured against the wrong population", fr.Deleted, fr.Refused)
	}
}
