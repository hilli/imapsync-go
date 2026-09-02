package syncer_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/hilli/imapsync-go/internal/syncer"
)

// firstMessage is a mailbox's first message, as bytes, so a test can put an
// exact copy of it back.
//
// Bytes rather than a fresh testMessage call: the point of these tests is two
// messages that are identical on the destination, and a copy this tool made
// carries a stamp header the source message did not.
func firstMessage(t *testing.T, a account, mailbox string) []byte {
	t.Helper()

	msgs := a.contents(t, mailbox)
	if len(msgs) == 0 {
		t.Fatalf("%q is empty", mailbox)
	}
	return []byte(msgs[0])
}

// TestADestinationCopyHeldTwiceIsRemoved is the feature.
//
// The mess this cleans up is not one this tool makes any more — the source-side
// skip stops it at the fetch. It is the mess a mailbox already has when someone
// arrives with it, which is the case the option exists for.
func TestADestinationCopyHeldTwiceIsRemoved(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.run(t)

	// A second copy put there by something other than this run.
	h.dst.stuff(t, "INBOX", firstMessage(t, h.dst, "INBOX"))
	if got := len(h.dst.contents(t, "INBOX")); got != 2 {
		t.Fatalf("setup left %d messages, want 2", got)
	}

	rep := h.run(t, func(o *syncer.Options) {
		o.Full = true
		o.Delete2Duplicates = true
	})
	fr := folderReport(t, rep, "INBOX")
	if fr.Removed != 1 {
		t.Errorf("removed %d duplicates, want 1", fr.Removed)
	}
	if rep.Removed() != 1 {
		t.Errorf("run total reported %d removed, want 1", rep.Removed())
	}
	if got := sortedSubjects(t, h.dst); !slices.Equal(got, []string{"one"}) {
		t.Fatalf("destination holds %v, want one copy", got)
	}
}

// TestRemovingDuplicatesIsOffUnlessAskedFor.
//
// Off has to mean the machinery never ran. The metadata pass this feature makes
// over the whole destination folder is the cost that makes it opt-in, and a
// run that pays it without being asked has given that away.
func TestRemovingDuplicatesIsOffUnlessAskedFor(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.run(t)
	h.dst.stuff(t, "INBOX", firstMessage(t, h.dst, "INBOX"))

	rep := h.run(t, func(o *syncer.Options) { o.Full = true })
	if fr := folderReport(t, rep, "INBOX"); fr.Removed != 0 {
		t.Errorf("removed %d duplicates without being asked to", fr.Removed)
	}
	if got := len(h.dst.contents(t, "INBOX")); got != 2 {
		t.Errorf("destination holds %d messages, want both left alone", got)
	}
}

// TestDelete2ImpliesRemovingDuplicates.
//
// imapsync's rule, and it follows from what --delete2 asks for: a run told to
// make the destination look like the source, that then left it holding two
// copies of something the source holds one of, would be contradicting itself.
func TestDelete2ImpliesRemovingDuplicates(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	fill(t, h.src, "INBOX", 30)
	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}

	dup := firstMessage(t, h.dst, "INBOX")
	h.dst.stuff(t, "INBOX", dup)
	before := len(uidsIn(t, h.dst, "INBOX"))

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{Full: true, Delete2: true}, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	fr := folderReport(t, rep, "INBOX")
	if fr.Removed != 1 {
		t.Errorf("removed %d duplicates under --delete2, want 1", fr.Removed)
	}
	if fr.Deleted != 0 {
		t.Errorf("deleted %d messages; the source lost nothing", fr.Deleted)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != before-1 {
		t.Errorf("destination holds %d messages, want %d", got, before-1)
	}
}

// TestDelete2DuplicatesCanBeDeclined.
//
// An option that can only ever turn destruction on is not an option. This
// project has already shipped one boolean that could not be turned off from the
// side that mattered, and it was --delete2.
func TestDelete2DuplicatesCanBeDeclined(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	fill(t, h.src, "INBOX", 30)
	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	h.dst.stuff(t, "INBOX", firstMessage(t, h.dst, "INBOX"))
	before := len(uidsIn(t, h.dst, "INBOX"))

	rep, err := syncFlaky(t, h, 1, 1,
		syncer.Options{Full: true, Delete2: true, NoDelete2Duplicates: true}, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Removed != 0 {
		t.Errorf("removed %d duplicates after being told not to", fr.Removed)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != before {
		t.Errorf("destination holds %d messages, want %d untouched", got, before)
	}
}

// TestADryRunReportsDuplicatesAndRemovesNone.
//
// The whole point of a dry run before a destructive option, and the reason it
// pays the full cost: a preview that guessed would be previewing a different
// program from the one that runs next.
func TestADryRunReportsDuplicatesAndRemovesNone(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.run(t)
	h.dst.stuff(t, "INBOX", firstMessage(t, h.dst, "INBOX"))

	rep := h.run(t, func(o *syncer.Options) {
		o.Full = true
		o.DryRun = true
		o.Delete2Duplicates = true
	})
	if fr := folderReport(t, rep, "INBOX"); fr.Removed != 1 {
		t.Errorf("the dry run reported %d duplicates, want 1", fr.Removed)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != 2 {
		t.Fatalf("the dry run left %d messages, want both still there", got)
	}

	// And the real run afterwards does what the preview said. A preview nothing
	// checks against the run is a preview of nothing.
	rep = h.run(t, func(o *syncer.Options) {
		o.Full = true
		o.Delete2Duplicates = true
	})
	if fr := folderReport(t, rep, "INBOX"); fr.Removed != 1 {
		t.Errorf("the real run removed %d duplicates, want the 1 the preview promised", fr.Removed)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != 1 {
		t.Errorf("destination holds %d messages, want 1", got)
	}
}

// TestTwoDestinationMessagesThatMerelyLookAlikeAreBothKept.
//
// The key is six headers and a byte count the server claimed, which is evidence
// and not proof: two notifications sent in the same second by the same robot
// agree on all of it while saying different things. Deleting one of those is
// destroying mail, so the bodies are compared in full first.
//
// Both bodies here are the same length on purpose. A test whose near-miss also
// differs in size passes on an implementation that never compares bodies at
// all, because the size alone would have separated them.
func TestTwoDestinationMessagesThatMerelyLookAlikeAreBothKept(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.dst.stuff(t, "INBOX", nearMiss("alert", "alert@example.test", "The disk is full."))
	h.dst.stuff(t, "INBOX", nearMiss("alert", "alert@example.test", "The disk is fine."))
	h.src.stuff(t, "INBOX", testMessage("unrelated", "unrelated@example.test"))

	rep := h.run(t, func(o *syncer.Options) {
		o.Full = true
		o.Delete2Duplicates = true
	})
	if fr := folderReport(t, rep, "INBOX"); fr.Removed != 0 {
		t.Errorf("removed %d messages that only looked alike", fr.Removed)
	}
	want := []string{"Body of unrelated.", "The disk is fine.", "The disk is full."}
	if got := bodiesOf(t, h.dst, "INBOX"); !slices.Equal(got, want) {
		t.Fatalf("destination holds %v, want %v", got, want)
	}
}

// TestDeduplicationLeavesNoDanglingClaim.
//
// The consequence the survivor rule exists for. Removing the copy a state row
// points at leaves that row naming nothing, whereupon destination verification
// finds the claim unhonoured and copies a third — so the two features would
// fight, and the folder would grow every time it was cleaned.
//
// Which copy survives is asserted in internal/dedup, where a claimed copy can
// be given a UID above an unclaimed one. It cannot be here: adoption always
// takes the lowest matching UID, so in any run this harness can stage the
// claimed copy is also the lowest, and the preference would be indistinguishable
// from the tie break. What this test can prove is the end state, and the end
// state is the point.
func TestDeduplicationLeavesNoDanglingClaim(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.run(t)
	// A stranger identical to the copy this run made. Stamping is by digest
	// rather than by a fresh identifier, so the two are byte identical and
	// group together.
	h.dst.stuff(t, "INBOX", firstMessage(t, h.dst, "INBOX"))

	rep := h.run(t, func(o *syncer.Options) {
		o.Full = true
		o.Delete2Duplicates = true
	})
	if fr := folderReport(t, rep, "INBOX"); fr.Removed != 1 {
		t.Fatalf("removed %d duplicates, want 1", fr.Removed)
	}

	// The run that matters. A dangling claim shows up here as a copy, not as an
	// error, which is why the count is checked rather than the exit status.
	rep = h.run(t, func(o *syncer.Options) { o.Full = true })
	fr := folderReport(t, rep, "INBOX")
	if fr.Missing != 0 || fr.Copied != 0 {
		t.Errorf("a later run found %d missing and copied %d; deduplication left a dangling claim", fr.Missing, fr.Copied)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != 1 {
		t.Errorf("destination holds %d messages after a later run, want 1", got)
	}
	if n := doneRowsNamingNothing(t, h); n != 0 {
		t.Errorf("%d rows are recorded as copied while naming no destination message", n)
	}
}

// TestRemovingADuplicateLeavesEverySourceMessageRepresented.
//
// Two source messages, identical, copied before the source-side skip existed:
// two destination copies, two rows, one row each. Removing one copy must
// re-point its row at the survivor, not drop it — a source message with no row
// is a source message the next run copies again, and the duplicate comes
// straight back.
func TestRemovingADuplicateLeavesEverySourceMessageRepresented(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))

	// --sync-duplicates to build the mess this option cleans up: two source
	// messages, two copies, two rows.
	h.run(t, func(o *syncer.Options) { o.SyncDuplicates = true })
	if got := len(uidsIn(t, h.dst, "INBOX")); got != 2 {
		t.Fatalf("setup left %d copies, want 2", got)
	}

	rep := h.run(t, func(o *syncer.Options) {
		o.Full = true
		o.SyncDuplicates = true
		o.Delete2Duplicates = true
	})
	if fr := folderReport(t, rep, "INBOX"); fr.Removed != 1 {
		t.Fatalf("removed %d duplicates, want 1", fr.Removed)
	}

	// The run that matters. Both source messages must still be accounted for,
	// or this one copies the second one back and the cleaning was pointless.
	rep = h.run(t, func(o *syncer.Options) {
		o.Full = true
		o.SyncDuplicates = true
		o.Delete2Duplicates = true
	})
	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 0 {
		t.Errorf("a later run copied %d messages; a source message lost its row", fr.Copied)
	}
	if fr.Missing != 0 {
		t.Errorf("a later run found %d claims unhonoured", fr.Missing)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != 1 {
		t.Errorf("destination holds %d messages, want 1", got)
	}
	if n := doneRowsNamingNothing(t, h); n != 0 {
		t.Errorf("%d rows are recorded as copied while naming no destination message", n)
	}
}

// TestRemovingDuplicatesRespectsTheDeletionCeiling.
//
// A key that collided across a whole folder is exactly the accident the ceiling
// exists to stop, and duplicate removal is deletion however it is named. The
// floor is what makes this testable at all: below it every nomination is
// allowed, so the folder has to be large enough to clear it.
func TestRemovingDuplicatesRespectsTheDeletionCeiling(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	for i := range 40 {
		body := testMessage(fmt.Sprintf("m%02d", i), fmt.Sprintf("m%d@example.test", i))
		h.dst.stuff(t, "INBOX", body)
		h.dst.stuff(t, "INBOX", body)
	}
	h.src.stuff(t, "INBOX", testMessage("unrelated", "unrelated@example.test"))

	rep := h.run(t, func(o *syncer.Options) {
		o.Full = true
		o.Delete2Duplicates = true
	})
	fr := folderReport(t, rep, "INBOX")
	if fr.Removed != 0 {
		t.Errorf("removed %d messages, want none: half the folder is over the ceiling", fr.Removed)
	}
	if fr.Refused != 40 {
		t.Errorf("refused %d removals, want 40", fr.Refused)
	}

	// And --force carries it out, so the test cannot pass on a build that
	// simply never removes anything.
	rep = h.run(t, func(o *syncer.Options) {
		o.Full = true
		o.Delete2Duplicates = true
		o.Force = true
	})
	if fr := folderReport(t, rep, "INBOX"); fr.Removed != 40 {
		t.Errorf("removed %d duplicates with --force, want 40", fr.Removed)
	}
}

// TestADuplicateOfAMessageWithNoIdentityIsLeftAlone.
//
// dedup refuses to group a weak identity, and this proves the refusal survives
// the trip through the syncer. ident calls an identity weak when almost no
// header survived, which means absence of a match says nothing — and acting on
// it here destroys mail rather than merely failing to copy it.
func TestADuplicateOfAMessageWithNoIdentityIsLeftAlone(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	// No Message-ID, no Subject, no From: nothing to identify it by.
	bare := []byte("To: " + testUser + "\r\n\r\nbody\r\n")
	h.dst.stuff(t, "INBOX", bare)
	h.dst.stuff(t, "INBOX", bare)
	h.src.stuff(t, "INBOX", testMessage("unrelated", "unrelated@example.test"))

	rep := h.run(t, func(o *syncer.Options) {
		o.Full = true
		o.Delete2Duplicates = true
		o.Force = true
	})
	if fr := folderReport(t, rep, "INBOX"); fr.Removed != 0 {
		t.Errorf("removed %d messages too thinly identified to group", fr.Removed)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != 3 {
		t.Errorf("destination holds %d messages, want all 3 left alone", got)
	}
}
