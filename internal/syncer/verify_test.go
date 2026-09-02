package syncer_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/hilli/imapsync-go/internal/syncer"
)

// sortedSubjects lists what a mailbox holds, duplicates included. Counting
// messages is not enough for these tests: the failure worth catching is a
// repair that drops the wrong claim, and that leaves the count right and the
// contents wrong.
func sortedSubjects(t *testing.T, a account) []string {
	t.Helper()

	var out []string
	for _, body := range a.contents(t, "INBOX") {
		for _, line := range strings.Split(body, "\r\n") {
			if subject, ok := strings.CutPrefix(line, "Subject: "); ok {
				out = append(out, subject)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// The measurement this whole feature came from. Before it existed, a
// destination copy deleted behind our back was never noticed and never
// re-made, because the state database recorded the source UID as done and
// nothing ever asked the destination whether that was still true. --full did
// not help: it re-diffs the source.
func TestACopyDeletedFromTheDestinationComesBack(t *testing.T) {
	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.src.stuff(t, "INBOX", testMessage("two", "two@example.test"))

	h.run(t)
	if got := len(h.dst.contents(t, "INBOX")); got != 2 {
		t.Fatalf("first run left %d messages on the destination, want 2", got)
	}

	removeFrom(t, h.dst, "INBOX", uidOf(t, h.dst, "INBOX", "two"))

	rep := h.run(t)
	fr := folderReport(t, rep, "INBOX")
	if fr.Missing != 1 {
		t.Errorf("reported %d missing copies, want 1", fr.Missing)
	}
	if fr.Copied != 1 {
		t.Errorf("copied %d messages, want the missing one back", fr.Copied)
	}
	// Sorted and compared whole rather than counted. A count of two is also
	// what you get from two copies of "one" with "two" still missing, which is
	// exactly what dropping the wrong claim produces.
	if got := sortedSubjects(t, h.dst); !slices.Equal(got, []string{"one", "two"}) {
		t.Fatalf("destination holds %v, want exactly one copy of each message", got)
	}
	if rep.Missing() != 1 {
		t.Errorf("run total reported %d missing, want 1", rep.Missing())
	}
}

// The counting gate is sound in one direction only. A destination that has
// lost a copy *and* gained an unrelated message has the same message count as
// a healthy one, so the free check cannot see it and --full is the answer.
func TestFullFindsAMissingCopyTheCountCannotSee(t *testing.T) {
	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.src.stuff(t, "INBOX", testMessage("two", "two@example.test"))
	h.run(t)

	// One copy out, one stranger in: the count is unchanged.
	removeFrom(t, h.dst, "INBOX", uidOf(t, h.dst, "INBOX", "two"))
	h.dst.stuff(t, "INBOX", testMessage("stranger", "stranger@example.test"))

	rep := h.run(t)
	if fr := folderReport(t, rep, "INBOX"); fr.Missing != 0 {
		t.Fatalf("the free check reported %d missing; it cannot see this case and must not claim to", fr.Missing)
	}

	rep = h.run(t, func(o *syncer.Options) { o.Full = true })
	fr := folderReport(t, rep, "INBOX")
	if fr.Missing != 1 {
		t.Errorf("--full reported %d missing copies, want 1", fr.Missing)
	}
	if got := sortedSubjects(t, h.dst); !slices.Equal(got, []string{"one", "stranger", "two"}) {
		t.Errorf("destination holds %v, want both copies plus the stranger", got)
	}
}

// A destination holding messages we never put there is normal, and must not be
// mistaken for a healthy one having lost something. This is the false-positive
// direction of the counting gate, and it is the direction that would cost a
// round trip on every folder of every run.
func TestStrangersOnTheDestinationTriggerNoVerification(t *testing.T) {
	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.run(t)
	h.dst.stuff(t, "INBOX", testMessage("stranger", "stranger@example.test"))
	h.src.stuff(t, "INBOX", testMessage("two", "two@example.test"))

	rep := h.run(t)
	fr := folderReport(t, rep, "INBOX")
	if fr.Missing != 0 {
		t.Errorf("reported %d missing copies against a destination that lost nothing", fr.Missing)
	}
	if fr.Copied != 1 {
		t.Errorf("copied %d, want only the new source message", fr.Copied)
	}
}

// --verify-dest=false is the documented way out, for someone who prunes the
// destination deliberately and does not want it refilled.
func TestVerificationCanBeTurnedOff(t *testing.T) {
	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.src.stuff(t, "INBOX", testMessage("two", "two@example.test"))
	h.run(t)
	removeFrom(t, h.dst, "INBOX", uidOf(t, h.dst, "INBOX", "two"))

	rep := h.run(t, func(o *syncer.Options) {
		o.NoVerifyDest = true
		o.Full = true
	})
	fr := folderReport(t, rep, "INBOX")
	if fr.Missing != 0 || fr.Copied != 0 {
		t.Errorf("missing=%d copied=%d; --verify-dest=false must not look or repair", fr.Missing, fr.Copied)
	}
	if got := len(h.dst.contents(t, "INBOX")); got != 1 {
		t.Errorf("destination holds %d messages, want the deletion left alone", got)
	}
}

// A dry run reports the missing copies and repairs nothing. Saying "already
// done" here would be the same silent lie the verification exists to remove.
func TestADryRunNamesMissingCopiesWithoutRestoringThem(t *testing.T) {
	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.src.stuff(t, "INBOX", testMessage("two", "two@example.test"))
	h.run(t)
	removeFrom(t, h.dst, "INBOX", uidOf(t, h.dst, "INBOX", "two"))

	rep := h.run(t, func(o *syncer.Options) { o.DryRun = true })
	fr := folderReport(t, rep, "INBOX")
	if fr.Missing != 1 {
		t.Errorf("dry run reported %d missing copies, want 1", fr.Missing)
	}
	if fr.Copied != 1 {
		t.Errorf("dry run said it would copy %d, want the missing one", fr.Copied)
	}
	if fr.AlreadyDone != 1 {
		t.Errorf("dry run counted %d as already done, want only the surviving copy", fr.AlreadyDone)
	}
	if got := len(h.dst.contents(t, "INBOX")); got != 1 {
		t.Errorf("dry run changed the destination: %d messages, want 1", got)
	}

	// And the repair still happens when a real run follows.
	if fr := folderReport(t, h.run(t), "INBOX"); fr.Copied != 1 {
		t.Errorf("the run after the dry run copied %d, want the missing message", fr.Copied)
	}
}
