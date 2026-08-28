package syncer_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// setFlags changes a message's flags on an account, the way a person reading
// their mail would.
func setFlags(t *testing.T, a account, mailbox, subject string, flags ...string) {
	t.Helper()

	ctx := context.Background()
	conn := a.dial(t)
	if _, err := conn.Select(ctx, mailbox, imapx.SelectOptions{}); err != nil {
		t.Fatalf("selecting %q: %v", mailbox, err)
	}
	uids, err := conn.SearchHeader(ctx, "Subject", subject)
	if err != nil {
		t.Fatalf("searching for %q: %v", subject, err)
	}
	if len(uids) != 1 {
		t.Fatalf("searching for %q found %d messages, want 1", subject, len(uids))
	}
	if err := conn.StoreFlags(ctx, uids[0], flags); err != nil {
		t.Fatalf("storing flags on %q: %v", subject, err)
	}
}

// flagsOf reads back the flags of one message, by subject.
func flagsOf(t *testing.T, a account, mailbox, subject string) []string {
	t.Helper()

	ctx := context.Background()
	conn := a.dial(t)
	if _, err := conn.Select(ctx, mailbox, imapx.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatalf("selecting %q: %v", mailbox, err)
	}
	uids, err := conn.SearchHeader(ctx, "Subject", subject)
	if err != nil {
		t.Fatalf("searching for %q: %v", subject, err)
	}
	if len(uids) != 1 {
		t.Fatalf("searching for %q found %d messages, want 1", subject, len(uids))
	}
	all, err := conn.FetchFlags(ctx, 0)
	if err != nil {
		t.Fatalf("fetching flags: %v", err)
	}
	for _, fs := range all {
		if fs.UID == uids[0] {
			out := slices.Clone(fs.Flags)
			slices.Sort(out)
			return out
		}
	}
	t.Fatalf("no flags came back for %q", subject)
	return nil
}

// countingDest counts the STOREs a run issues, so a test can tell the
// difference between "the flags agree" and "the flags were written again".
type countingDest struct {
	imapx.Conn
	stores *atomic.Int32
}

func (c countingDest) StoreFlags(ctx context.Context, uid uint32, flags []string) error {
	c.stores.Add(1)
	return c.Conn.StoreFlags(ctx, uid, flags)
}

func counting(stores *atomic.Int32) func(imapx.Conn) imapx.Conn {
	return func(c imapx.Conn) imapx.Conn {
		return countingDest{Conn: c, stores: stores}
	}
}

// countingSource counts flag fetches, which is the expensive half of a resync:
// on a 414k-message mailbox it is the enumeration CONDSTORE exists to avoid.
type countingSource struct {
	imapx.Conn
	fetches *atomic.Int32
}

func (c countingSource) FetchFlags(ctx context.Context, since uint64) ([]imapx.FlagSet, error) {
	c.fetches.Add(1)
	return c.Conn.FetchFlags(ctx, since)
}

// TestAFlagSetOnTheSourceReachesTheDestination.
//
// A mailbox is not just its messages. Read, answered and flagged are state a
// person built up over years, and a mirror that drops them has lost something
// that cannot be recomputed from the bodies.
func TestAFlagSetOnTheSourceReachesTheDestination(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	subjects := fill(t, h.src, "INBOX", 3)

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := flagsOf(t, h.dst, "INBOX", subjects[1]); len(got) != 0 {
		t.Fatalf("the message arrived with flags %v, want none", got)
	}

	setFlags(t, h.src, "INBOX", subjects[1], "\\Seen", "\\Answered")

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if got := flagsOf(t, h.dst, "INBOX", subjects[1]); !slices.Equal(got, []string{"\\Answered", "\\Seen"}) {
		t.Errorf("destination flags = %v, want [\\Answered \\Seen]", got)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Reflagged != 1 {
		t.Errorf("reported %d messages reflagged, want 1", fr.Reflagged)
	}
}

// TestAFlagClearedOnTheSourceIsClearedOnTheDestination is the direction that
// separates a mirror from an accumulator. A message marked read and then marked
// unread again must come back unread at the far end, which only a replacement
// of the whole set achieves.
func TestAFlagClearedOnTheSourceIsClearedOnTheDestination(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	subjects := fill(t, h.src, "INBOX", 2)
	setFlags(t, h.src, "INBOX", subjects[0], "\\Seen", "\\Flagged")

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := flagsOf(t, h.dst, "INBOX", subjects[0]); !slices.Equal(got, []string{"\\Flagged", "\\Seen"}) {
		t.Fatalf("the copy did not carry the flags across: got %v", got)
	}

	setFlags(t, h.src, "INBOX", subjects[0], "\\Flagged")

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}

	if got := flagsOf(t, h.dst, "INBOX", subjects[0]); !slices.Equal(got, []string{"\\Flagged"}) {
		t.Errorf("destination flags = %v, want [\\Flagged]: \\Seen was never taken off", got)
	}
}

// TestFlagsThatAgreeAreNotWrittenAgain.
//
// The comparison has to survive a server listing flags in a different order
// from the one they were stored in, or every run would rewrite every flagged
// message in the account and call it work.
func TestFlagsThatAgreeAreNotWrittenAgain(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	subjects := fill(t, h.src, "INBOX", 4)
	setFlags(t, h.src, "INBOX", subjects[0], "\\Seen", "\\Answered", "\\Flagged")
	setFlags(t, h.src, "INBOX", subjects[2], "\\Flagged", "\\Seen")

	stores := &atomic.Int32{}
	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, counting(stores)); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := stores.Load(); got != 0 {
		t.Errorf("the run that copied the messages also stored flags %d times; the APPEND already carried them", got)
	}

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, counting(stores))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := stores.Load(); got != 0 {
		t.Errorf("a run with nothing to do stored flags %d times", got)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Reflagged != 0 {
		t.Errorf("reported %d messages reflagged, want 0", fr.Reflagged)
	}
}

// TestNoResyncFlagsLeavesTheDestinationAlone.
//
// The option has to be distinguishable from its absence, or it is a flag that
// exists only in the help text.
func TestNoResyncFlagsLeavesTheDestinationAlone(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	subjects := fill(t, h.src, "INBOX", 3)

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{NoResyncFlags: true}, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	setFlags(t, h.src, "INBOX", subjects[1], "\\Seen")

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{NoResyncFlags: true}, nil, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := flagsOf(t, h.dst, "INBOX", subjects[1]); len(got) != 0 {
		t.Errorf("destination flags = %v, want none: the resync ran despite being switched off", got)
	}

	// And with the option absent the same change does land, so what the test
	// above observed was the option and not a resync that never works.
	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, nil); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if got := flagsOf(t, h.dst, "INBOX", subjects[1]); !slices.Equal(got, []string{"\\Seen"}) {
		t.Errorf("destination flags = %v, want [\\Seen]", got)
	}
}

// TestAnUnchangedFolderIsNotAskedForFlags is the synergy the fast path buys.
//
// A modification sequence that has not moved means no flag moved either, so the
// cheapest flag resync is the one that never starts. Without this the fast path
// would save a UID listing and then spend it again on a flag enumeration.
func TestAnUnchangedFolderIsNotAskedForFlags(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 5)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}
	fetches := &atomic.Int32{}
	wrap := func(c imapx.Conn) imapx.Conn {
		return countingSource{Conn: modseq(seq, uids)(c), fetches: fetches}
	}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, wrap, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := fetches.Load()
	if first == 0 {
		t.Fatal("the first run never fetched flags; the decorator is not in the path")
	}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, wrap, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := fetches.Load(); got != first {
		t.Errorf("a folder the fast path skipped was still asked for flags %d times", got-first)
	}
}

// TestTheFlagDeltaIsAskedForRelativeToTheWatermark.
//
// The whole economy of the thing is here: with a watermark the server is asked
// what changed since, and without one it has to hand back every flag in the
// mailbox. On iCloud's INBOX that is the difference between a small response
// and 414k of them, on every run.
func TestTheFlagDeltaIsAskedForRelativeToTheWatermark(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	subjects := fill(t, h.src, "INBOX", 3)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}
	asked := &atomic.Uint64{}
	wrap := func(c imapx.Conn) imapx.Conn {
		return askedSince{Conn: modseq(seq, uids)(c), since: asked}
	}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, wrap, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := asked.Load(); got != 0 {
		t.Errorf("the first run asked for changes since %d, want 0: it had no watermark to ask from", got)
	}

	seq.Store(140)
	setFlags(t, h.src, "INBOX", subjects[1], "\\Seen")

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, wrap, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := asked.Load(); got != 100 {
		t.Errorf("the second run asked for changes since %d, want 100: the watermark from the first run", got)
	}
}

// askedSince records the modification sequence a flag fetch was asked for.
type askedSince struct {
	imapx.Conn
	since *atomic.Uint64
}

func (a askedSince) FetchFlags(ctx context.Context, since uint64) ([]imapx.FlagSet, error) {
	a.since.Store(since)
	return a.Conn.FetchFlags(ctx, since)
}

// TestARenumberedSourceAsksForEveryFlag.
//
// A modification sequence is only meaningful within one UIDVALIDITY. After a
// renumbering the stored watermark describes a mailbox the server no longer
// has, and asking "what changed since 100" against the new one invites an
// answer that is wrong in the quiet direction: nothing came back, so nothing
// was updated, and the flags the state has just forgotten stay forgotten.
//
// The renumbering also wipes the message rows, so everything is re-adopted with
// no flags remembered — which is precisely when the full enumeration is needed.
func TestARenumberedSourceAsksForEveryFlag(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	subjects := fill(t, h.src, "INBOX", 3)
	setFlags(t, h.src, "INBOX", subjects[0], "\\Seen")

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}
	asked := &atomic.Uint64{}
	shift := &atomic.Uint32{}
	wrap := func(c imapx.Conn) imapx.Conn {
		return askedSince{Conn: renumbered{Conn: modseq(seq, uids)(c), by: shift}, since: asked}
	}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, wrap, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}

	shift.Store(7)
	seq.Store(140)

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, wrap, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := asked.Load(); got != 0 {
		t.Errorf("after renumbering the run asked for changes since %d, want 0: that sequence belongs to a mailbox the server no longer has", got)
	}
	if got := flagsOf(t, h.dst, "INBOX", subjects[0]); !slices.Equal(got, []string{"\\Seen"}) {
		t.Errorf("destination flags = %v, want [\\Seen]", got)
	}
}

// sulkingStore refuses to write flags until it is told to stop.
type sulkingStore struct {
	imapx.Conn
	refuse *atomic.Bool
	stores *atomic.Int32
}

func (s sulkingStore) StoreFlags(ctx context.Context, uid uint32, flags []string) error {
	if s.refuse.Load() {
		return errors.New("STORE rejected")
	}
	s.stores.Add(1)
	return s.Conn.StoreFlags(ctx, uid, flags)
}

// TestAFlagThatFailedToStoreIsTriedAgain.
//
// This is the tombstone trap in its flag-shaped form. The watermark means
// "everything the source held at that point is on the destination", and a
// folder that failed to write a flag has not met that. If the failure went
// uncounted the watermark would advance, the fast path would skip the folder
// from then on, and that flag would be lost for good — a permanent divergence
// created by one transient rejection.
func TestAFlagThatFailedToStoreIsTriedAgain(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	subjects := fill(t, h.src, "INBOX", 3)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}
	refuse := &atomic.Bool{}
	stores := &atomic.Int32{}
	wrapDst := func(c imapx.Conn) imapx.Conn {
		return sulkingStore{Conn: c, refuse: refuse, stores: stores}
	}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), wrapDst); err != nil {
		t.Fatalf("first run: %v", err)
	}

	setFlags(t, h.src, "INBOX", subjects[1], "\\Seen")
	seq.Store(140)
	refuse.Store(true)

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), wrapDst)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Failed == 0 {
		t.Error("a rejected STORE was not counted as a failure, so the folder looks finished")
	}

	// The source has not moved since, so only an unadvanced watermark can make
	// the run look at this folder again.
	refuse.Store(false)
	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), wrapDst); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if got := stores.Load(); got != 1 {
		t.Errorf("the destination was written %d times, want 1: the folder was skipped and the flag lost", got)
	}
	if got := flagsOf(t, h.dst, "INBOX", subjects[1]); !slices.Equal(got, []string{"\\Seen"}) {
		t.Errorf("destination flags = %v, want [\\Seen]", got)
	}
}

// TestFlagsAreRememberedOnceStored.
//
// Without recording what was written, every later run finds the stored flags
// still disagreeing and issues the same STORE again. Harmless one message at a
// time, and a permanent tax on a folder that anyone keeps touching.
func TestFlagsAreRememberedOnceStored(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	subjects := fill(t, h.src, "INBOX", 3)

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}
	stores := &atomic.Int32{}

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), counting(stores)); err != nil {
		t.Fatalf("first run: %v", err)
	}

	setFlags(t, h.src, "INBOX", subjects[1], "\\Seen")
	seq.Store(140)

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), counting(stores)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := stores.Load(); got != 1 {
		t.Fatalf("the run wrote flags %d times, want 1", got)
	}

	// A third run over a folder that still looks changed, with nothing new to
	// do. It should find the flags already agree.
	seq.Store(141)
	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), counting(stores))
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if got := stores.Load(); got != 1 {
		t.Errorf("the destination was written %d times in total, want 1: the flags that were stored were never remembered", got)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Reflagged != 0 {
		t.Errorf("the third run reported %d messages reflagged, want 0", fr.Reflagged)
	}
}
