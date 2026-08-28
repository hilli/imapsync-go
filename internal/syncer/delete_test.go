package syncer_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// TestDeletingNeverTouchesWhatItDidNotPut is the promise that makes this safe to
// point at a destination that is not empty.
//
// The state database is the only record of what this tool is responsible for. A
// message it never copied has no row, and so can never be nominated however
// long the source goes without mentioning anything like it.
//
// The strangers have to be genuinely unlike anything on the source. A
// destination message identical to a source one is adopted on the first run —
// recorded as the copy that would otherwise have been made — and from then on it
// is ours, so following the source in deleting it is right.
func TestDeletingNeverTouchesWhatItDidNotPut(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	fill(t, h.src, "INBOX", 10)

	// Mail that was on the destination first, and has no counterpart anywhere.
	var stranger []string
	for i := range 4 {
		subject := fmt.Sprintf("stranger-%04d", i)
		h.dst.stuff(t, "INBOX", testMessage(subject, fmt.Sprintf("x%d@elsewhere.test", i)))
		stranger = append(stranger, subject)
	}

	opts := syncer.Options{Delete2: true, Full: true}
	if _, err := syncFlaky(t, h, 1, 1, opts, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Empty the source of everything, and force past the ceiling, so the only
	// thing standing between the strangers and deletion is that we never put
	// them there.
	for _, uid := range uidsIn(t, h.src, "INBOX") {
		removeFrom(t, h.src, "INBOX", uid)
	}

	forced := syncer.Options{Delete2: true, Full: true, Force: true}
	if _, err := syncFlaky(t, h, 1, 1, forced, nil, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}

	left := subjects(h.dst.contents(t, "INBOX"))
	for _, s := range stranger {
		if !slices.Contains(left, s) {
			t.Errorf("deleted %q, which this tool never copied", s)
		}
	}
	if len(left) != len(stranger) {
		t.Errorf("destination holds %d messages, want only the %d strangers", len(left), len(stranger))
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
