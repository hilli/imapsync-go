package syncer_test

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// dedup drives a standalone deduplication run.
//
// The nil source pool is the point, not an economy. Every other test in this
// package builds both pools, so a Dedup that quietly reached for the source
// would pass all of them; here it would panic. The claim the command makes --
// that it examines one account and never contacts the other -- is only as good
// as something that cannot be true by accident.
func (h *harness) dedup(t *testing.T, sel syncer.DedupSelection, opts ...func(*syncer.Options)) syncer.DedupReport {
	t.Helper()

	o := syncer.Options{PairID: "test"}
	for _, f := range opts {
		f(&o)
	}

	s := syncer.New(nil, pooled(t, 4, imapx.SelectOptions{}, h.dst.dialFunc(t, nil)), h.db, nil, o)
	report, err := s.Dedup(context.Background(), sel)
	if err != nil {
		t.Fatalf("Dedup() error = %v", err)
	}
	return report
}

// dedupFolder finds one folder's report.
func dedupFolder(t *testing.T, rep syncer.DedupReport, dest string) syncer.DedupFolderReport {
	t.Helper()

	for _, fr := range rep.Folders {
		if fr.Dest == dest {
			if fr.Err != nil {
				t.Fatalf("folder %q failed: %v", dest, fr.Err)
			}
			return fr
		}
	}
	t.Fatalf("no report for %q", dest)
	return syncer.DedupFolderReport{}
}

// TestDedupNeedsNoSourceAtAll is the command's reason for existing.
//
// Asking someone to configure and run a copying tool over an account they only
// want tidied is asking them to point a copying tool at their mail. The
// separate command is worth having only if it genuinely does not sync, and the
// nil source pool in the harness is what makes that a fact rather than a claim.
func TestDedupNeedsNoSourceAtAll(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.run(t)
	h.dst.stuff(t, "INBOX", firstMessage(t, h.dst, "INBOX"))

	rep := h.dedup(t, syncer.DedupSelection{})
	if fr := dedupFolder(t, rep, "INBOX"); fr.Removed != 1 {
		t.Errorf("removed %d duplicates, want 1", fr.Removed)
	}
	if _, removed, _, _ := rep.Totals(); removed != 1 {
		t.Errorf("run total reported %d removed, want 1", removed)
	}
	if got := sortedSubjects(t, h.dst); !slices.Equal(got, []string{"one"}) {
		t.Fatalf("destination holds %v, want one copy", got)
	}
}

// TestDedupCleansAFolderNoSyncHasRecorded.
//
// The usual case for this command: an account that arrived with duplicates in
// it, whose folders appear in no state database anywhere. A run that only
// worked on folders it had previously synchronised would be useless for
// exactly the mailboxes it is for.
func TestDedupCleansAFolderNoSyncHasRecorded(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	if err := h.dst.user.Create("Arrived", nil); err != nil {
		t.Fatalf("creating destination folder: %v", err)
	}
	body := testMessage("stranger", "stranger@example.test")
	h.dst.stuff(t, "Arrived", body, body)

	rep := h.dedup(t, syncer.DedupSelection{Only: []string{"Arrived"}})
	fr := dedupFolder(t, rep, "Arrived")
	if fr.Removed != 1 {
		t.Errorf("removed %d duplicates, want 1", fr.Removed)
	}
	if fr.Population != 2 {
		t.Errorf("reported a population of %d, want 2", fr.Population)
	}
	if got := len(h.dst.contents(t, "Arrived")); got != 1 {
		t.Errorf("folder holds %d messages, want 1", got)
	}
}

// TestDedupLeavesNoDanglingClaim.
//
// The state database records that a particular destination message answers for
// a source message. Removing that one and keeping its twin would leave the
// record naming nothing, whereupon destination verification finds the copy
// missing and makes a third -- the two features would fight and the folder
// would grow every time it was cleaned.
func TestDedupLeavesNoDanglingClaim(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("kept", "kept@example.test"))
	h.run(t)
	h.dst.stuff(t, "INBOX", firstMessage(t, h.dst, "INBOX"))

	h.dedup(t, syncer.DedupSelection{})
	if n := doneRowsNamingNothing(t, h); n != 0 {
		t.Errorf("%d recorded copies name no destination message", n)
	}

	// The proof that no claim dangles: a full sync afterwards finds everything
	// where it was recorded and copies nothing back.
	rep := h.run(t, func(o *syncer.Options) { o.Full = true })
	if copied, _, _ := rep.Totals(); copied != 0 {
		t.Errorf("the sync after deduplication copied %d messages back", copied)
	}
	if got := len(h.dst.contents(t, "INBOX")); got != 1 {
		t.Errorf("destination holds %d messages, want 1", got)
	}
}

// TestDedupDryRunRemovesNothingAndSaysWhatItWould.
//
// The preview pays the full cost and is computed by the code that does the
// work, because a preview produced by a second implementation previews that
// implementation -- and this option deletes mail, which is where that would
// matter most.
func TestDedupDryRunRemovesNothingAndSaysWhatItWould(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.run(t)
	h.dst.stuff(t, "INBOX", firstMessage(t, h.dst, "INBOX"))

	rep := h.dedup(t, syncer.DedupSelection{}, func(o *syncer.Options) { o.DryRun = true })
	if fr := dedupFolder(t, rep, "INBOX"); fr.Removed != 1 {
		t.Errorf("the preview reported %d removable, want 1", fr.Removed)
	}
	if got := len(h.dst.contents(t, "INBOX")); got != 2 {
		t.Fatalf("the dry run left %d messages, want both still there", got)
	}

	if fr := dedupFolder(t, h.dedup(t, syncer.DedupSelection{}), "INBOX"); fr.Removed != 1 {
		t.Errorf("the real run removed %d, want the 1 the preview promised", fr.Removed)
	}
	if got := len(h.dst.contents(t, "INBOX")); got != 1 {
		t.Errorf("destination holds %d messages, want 1", got)
	}
}

// TestDedupExaminesOnlyTheFoldersAskedFor.
//
// A command that deletes needs to be aimable. Someone who has found duplicates
// in one folder should be able to clean that folder without authorising a
// removal pass over every mailbox in the account.
func TestDedupExaminesOnlyTheFoldersAskedFor(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	for _, name := range []string{"Wanted", "Untouched"} {
		if err := h.dst.user.Create(name, nil); err != nil {
			t.Fatalf("creating %q: %v", name, err)
		}
		body := testMessage("in "+name, name+"@example.test")
		h.dst.stuff(t, name, body, body)
	}

	rep := h.dedup(t, syncer.DedupSelection{Only: []string{"Wanted"}})
	if fr := dedupFolder(t, rep, "Wanted"); fr.Removed != 1 {
		t.Errorf("removed %d from the folder asked for, want 1", fr.Removed)
	}
	if got := len(h.dst.contents(t, "Untouched")); got != 2 {
		t.Errorf("a folder that was not asked for lost messages: %d left, want 2", got)
	}

	var skipped []string
	for _, s := range rep.Skips {
		skipped = append(skipped, s.Dest)
	}
	if !slices.Contains(skipped, "Untouched") {
		t.Errorf("skipped %v, which does not say that Untouched was left out", skipped)
	}
}

// TestDedupExcludesFoldersByPattern covers the regexp half of the selection,
// which --folder's exact-name matching cannot stand in for.
func TestDedupExcludesFoldersByPattern(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	for _, name := range []string{"Lists/go", "Lists/perl", "Private"} {
		if err := h.dst.user.Create(name, nil); err != nil {
			t.Fatalf("creating %q: %v", name, err)
		}
		body := testMessage("in "+name, "x@example.test")
		h.dst.stuff(t, name, body, body)
	}

	rep := h.dedup(t, syncer.DedupSelection{
		Include: []*regexp.Regexp{regexp.MustCompile("^Lists/")},
		Exclude: []*regexp.Regexp{regexp.MustCompile("perl$")},
	})

	if fr := dedupFolder(t, rep, "Lists/go"); fr.Removed != 1 {
		t.Errorf("Lists/go: removed %d, want 1", fr.Removed)
	}
	for _, left := range []string{"Lists/perl", "Private"} {
		if got := len(h.dst.contents(t, left)); got != 2 {
			t.Errorf("%s lost messages: %d left, want 2", left, got)
		}
	}
}

// TestDedupRefusesAFolderTwoSourcesCopyInto.
//
// Two sets of records over one mailbox means re-pointing one set and leaving
// the other naming a message that is gone. Planning refuses that collision, so
// this should never arise -- which is the reason to report it rather than to
// assume it, since the cost of being wrong is a record pointing at deleted mail.
func TestDedupRefusesAFolderTwoSourcesCopyInto(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	if err := h.dst.user.Create("Shared", nil); err != nil {
		t.Fatalf("creating destination folder: %v", err)
	}
	body := testMessage("shared", "shared@example.test")
	h.dst.stuff(t, "Shared", body, body)

	ctx := context.Background()
	for _, source := range []string{"One", "Two"} {
		if _, err := h.db.EnsureFolder(ctx, "test", source, "Shared"); err != nil {
			t.Fatalf("recording folder %q: %v", source, err)
		}
	}

	rep := h.dedup(t, syncer.DedupSelection{Only: []string{"Shared"}})
	var failed error
	for _, fr := range rep.Folders {
		if fr.Dest == "Shared" {
			failed = fr.Err
		}
	}
	if failed == nil {
		t.Fatalf("deduplicated a folder two source folders copy into")
	}
	if got := len(h.dst.contents(t, "Shared")); got != 2 {
		t.Errorf("the refused folder lost messages: %d left, want 2", got)
	}
}
