package syncer_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/searchkey"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// uidOf finds one message by subject, which is how these tests name a message
// they want to remove without depending on the order UIDs were handed out in.
func uidOf(t *testing.T, a account, mailbox, subject string) uint32 {
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
	return uids[0]
}

// mustParse is the search key a test means, or a failure naming it.
func mustParse(t *testing.T, s string) searchkey.Key {
	t.Helper()

	key, err := searchkey.Parse(s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return key
}

func withSourceSearch(t *testing.T, s string) func(*syncer.Options) {
	t.Helper()

	key := mustParse(t, s)
	return func(o *syncer.Options) { o.SourceSearch = key }
}

func withDestSearch(t *testing.T, s string) func(*syncer.Options) {
	t.Helper()

	key := mustParse(t, s)
	return func(o *syncer.Options) { o.DestSearch = key }
}

// TestASourceSearchCopiesOnlyWhatItMatched.
//
// The whole point of the option, and the cheapest thing about it: the server
// answers with UIDs, so a search that excludes a message excludes it before
// anything is fetched.
func TestASourceSearchCopiesOnlyWhatItMatched(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	h.src.stuffWithFlags(t, "INBOX", []imap.Flag{imap.FlagSeen}, testMessage("read", "read@example.test"))
	h.src.stuff(t, "INBOX", testMessage("unread", "unread@example.test"))

	rep := h.run(t, withSourceSearch(t, "UNSEEN"))

	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 1 || fr.Filtered != 1 {
		t.Errorf("copied %d and filtered %d, want 1 and 1", fr.Copied, fr.Filtered)
	}
	if fr.Failed != 0 {
		t.Errorf("reported %d failed; a message the search did not match is not a failure", fr.Failed)
	}

	got := subjectsIn(t, h.dst, "INBOX")
	if got["read"] || !got["unread"] {
		t.Errorf("--source-search kept the wrong message: %v", got)
	}
}

// TestASourceSearchThatMatchesNothingCopiesNothingAndSucceeds.
//
// An empty SEARCH response is an answer, not a failure, and it must not be
// mistaken for "the search did not run" — which would copy the whole folder.
func TestASourceSearchThatMatchesNothingCopiesNothingAndSucceeds(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.src.stuff(t, "INBOX", testMessage("two", "two@example.test"))

	rep := h.run(t, withSourceSearch(t, "KEYWORD $NothingHasThis"))

	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 0 {
		t.Errorf("copied %d, want 0: the search matched nothing", fr.Copied)
	}
	if fr.Filtered != 2 {
		t.Errorf("reported %d filtered, want 2", fr.Filtered)
	}
	if fr.Failed != 0 {
		t.Errorf("reported %d failed, want 0", fr.Failed)
	}
	if got := subjectsIn(t, h.dst, "INBOX"); len(got) != 0 {
		t.Errorf("the destination is not empty: %v", got)
	}
}

// TestDeletingDoesNotReachAMessageTheSourceSearchLeftOut is the deletion
// hazard again, in the form the search creates.
//
// The size and age filters exclude messages after the source has been listed,
// so the listing they are excluded from is only ever the copy list. A search
// excludes them at the listing itself, which is where it would be easiest to
// narrow the wrong thing: if the search narrowed what the source is held to
// hold, every message it excludes would look to --delete2 like a message the
// source has lost, and would be destroyed on the destination.
func TestDeletingDoesNotReachAMessageTheSourceSearchLeftOut(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuffWithFlags(t, "INBOX", []imap.Flag{imap.FlagSeen}, testMessage("read", "read@example.test"))
	h.src.stuff(t, "INBOX", testMessage("unread", "unread@example.test"))

	if fr := folderReport(t, h.run(t), "INBOX"); fr.Copied != 2 {
		t.Fatalf("setup copied %d, want 2", fr.Copied)
	}

	rep := h.run(t, withSourceSearch(t, "UNSEEN"), func(o *syncer.Options) {
		o.Delete2 = true
		o.Force = true
	})

	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 0 {
		t.Errorf("deleted %d messages, want 0: the source still holds both", fr.Deleted)
	}
	got := subjectsIn(t, h.dst, "INBOX")
	if !got["read"] {
		t.Error("--delete2 destroyed a message that --source-search merely declined to copy again")
	}
	if !got["unread"] {
		t.Error("--delete2 destroyed a message the search selected")
	}
}

// TestASearchedRunDoesNotRecordAWatermark.
//
// A search key is usually immutable — UNSEEN is not, and neither is anything
// resting on flags or on a date relative to today. The folder's modification
// sequence does not move when a message merely ages out of SINCE, so a
// watermark recorded under a search would skip the folder for as long as the
// mailbox stayed quiet, and the message the search excludes today would never
// be copied.
//
// The same suppression the size and age filters get, for the same reason, and
// worth its own test because it comes from counting search exclusions as
// Filtered rather than from any code that mentions searching.
func TestASearchedRunDoesNotRecordAWatermark(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	h.src.stuffWithFlags(t, "INBOX", []imap.Flag{imap.FlagSeen}, testMessage("read", "read@example.test"))
	h.src.stuff(t, "INBOX", testMessage("unread", "unread@example.test"))

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	opts := syncer.Options{SourceSearch: mustParse(t, "UNSEEN")}
	first, err := syncFlaky(t, h, 1, 1, opts, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if fr := folderReport(t, first, "INBOX"); fr.Copied != 1 || fr.Filtered != 1 {
		t.Fatalf("first run copied %d and filtered %d, want 1 and 1", fr.Copied, fr.Filtered)
	}
	before := uids.Load()

	// The message is now unread — the flag changed on the source without the
	// folder's modification sequence moving, which is what the decorator is
	// holding still.
	second, err := syncFlaky(t, h, 1, 1, syncer.Options{}, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if uids.Load() == before {
		t.Fatal("the second run never re-listed the folder: a searched run recorded a watermark, " +
			"and the fast path will skip this folder for as long as the mailbox stays quiet")
	}
	if fr := folderReport(t, second, "INBOX"); fr.Copied != 1 {
		t.Errorf("second run copied %d, want 1: the message the search excluded", fr.Copied)
	}
	if got := subjectsIn(t, h.dst, "INBOX"); !got["read"] || !got["unread"] {
		t.Errorf("the destination is missing mail after both runs: %v", got)
	}
}

// TestASearchThatExcludedNothingKeepsTheFastPath is the other half.
//
// Suppressing the watermark on every searched run would cost a full re-diff of
// every folder for ever on an account that always passes --source-search, and
// a search that excluded nothing has mirrored the folder completely.
func TestASearchThatExcludedNothingKeepsTheFastPath(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	for i := range 3 {
		h.src.stuff(t, "INBOX", testMessage(fmt.Sprintf("m-%d", i), fmt.Sprintf("m%d@example.test", i)))
	}

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	opts := syncer.Options{SourceSearch: mustParse(t, "UNSEEN")}
	first, err := syncFlaky(t, h, 1, 1, opts, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if fr := folderReport(t, first, "INBOX"); fr.Filtered != 0 || fr.Copied != 3 {
		t.Fatalf("first run copied %d and filtered %d, want 3 and 0", fr.Copied, fr.Filtered)
	}
	before := uids.Load()

	if _, err := syncFlaky(t, h, 1, 1, opts, modseq(seq, uids), nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := uids.Load(); got != before {
		t.Errorf("the second run re-listed the folder %d times; a search that excluded nothing "+
			"should still let the folder be recorded as mirrored", got-before)
	}
}

// TestADestSearchNarrowsWhatIsDeleted.
//
// The option's whole purpose: of the messages the source no longer holds,
// delete only those the search picks out.
func TestADestSearchNarrowsWhatIsDeleted(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("keep", "keep@example.test"))
	h.src.stuff(t, "INBOX", testMessage("goes", "goes@example.test"))
	h.src.stuff(t, "INBOX", testMessage("stays", "stays@example.test"))

	if fr := folderReport(t, h.run(t), "INBOX"); fr.Copied != 3 {
		t.Fatalf("setup copied %d, want 3", fr.Copied)
	}

	// Two messages leave the source, so both are deletion candidates.
	for _, subject := range []string{"goes", "stays"} {
		removeFrom(t, h.src, "INBOX", uidOf(t, h.src, "INBOX", subject))
	}

	// The search picks one of the two out on the destination.
	setFlags(t, h.dst, "INBOX", "goes", `\Deleted`)

	rep := h.run(t, withDestSearch(t, "DELETED"), func(o *syncer.Options) {
		o.Delete2 = true
		o.Force = true
	})

	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 1 {
		t.Errorf("deleted %d, want 1: only the message the search matched", fr.Deleted)
	}
	got := subjectsIn(t, h.dst, "INBOX")
	if got["goes"] {
		t.Error("the message the search matched was not deleted")
	}
	if !got["stays"] {
		t.Error("--dest-search did not protect the message it excluded")
	}
	if !got["keep"] {
		t.Error("a message the source still holds was deleted")
	}
}

// TestADestSearchNeverCausesADuplicate is the divergence from imapsync,
// stated as a test.
//
// imapsync's --search2 narrows the destination listing itself, so a message it
// hides is not recognised as already present and is copied a second time; its
// own documentation warns about this. Here the search is applied to the
// deletion candidates alone and to nothing else, so it cannot reach adoption.
//
// The state database is deliberately not reused between the two runs: with a
// remembered UID pair, dedup would succeed without the search having been kept
// out of it, and the test would pass while proving nothing. On a fresh
// database, recognising the destination copy is the only thing standing
// between this run and a duplicate.
func TestADestSearchNeverCausesADuplicate(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("one", "one@example.test"))
	h.src.stuff(t, "INBOX", testMessage("two", "two@example.test"))

	if fr := folderReport(t, h.run(t), "INBOX"); fr.Copied != 2 {
		t.Fatalf("setup copied %d, want 2", fr.Copied)
	}

	h.dbPath = filepath.Join(t.TempDir(), "fresh.db")
	h.db = h.openDB(t)

	// A search matching nothing on the destination: under imapsync's meaning
	// this hides every destination message, and every source message is
	// copied again.
	rep := h.run(t, withDestSearch(t, "KEYWORD $NothingHasThis"), func(o *syncer.Options) {
		o.Delete2 = true
		o.Force = true
	})

	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 0 {
		t.Errorf("copied %d, want 0: --dest-search must not hide messages from adoption", fr.Copied)
	}
	if fr.Adopted != 2 {
		t.Errorf("adopted %d, want 2", fr.Adopted)
	}
	if fr.Deleted != 0 {
		t.Errorf("deleted %d, want 0: the source holds both", fr.Deleted)
	}
	if n := len(h.dst.contents(t, "INBOX")); n != 2 {
		t.Errorf("the destination holds %d messages, want 2: --dest-search duplicated mail", n)
	}
}

// TestADestSearchDoesNotWidenTheVictimList.
//
// A search is a narrowing, and only ever a narrowing. A destination message
// the search matches but which the source still holds is not a candidate for
// deletion and must not become one.
func TestADestSearchDoesNotWidenTheVictimList(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", testMessage("here", "here@example.test"))

	if fr := folderReport(t, h.run(t), "INBOX"); fr.Copied != 1 {
		t.Fatalf("setup copied %d, want 1", fr.Copied)
	}
	setFlags(t, h.dst, "INBOX", "here", `\Deleted`)

	rep := h.run(t, withDestSearch(t, "DELETED"), func(o *syncer.Options) {
		o.Delete2 = true
		o.Force = true
	})

	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 0 {
		t.Errorf("deleted %d, want 0: the source still holds the message", fr.Deleted)
	}
	if !subjectsIn(t, h.dst, "INBOX")["here"] {
		t.Error("--dest-search turned a search into an instruction to delete")
	}
}

// TestTheDryRunPreviewsWhatASearchedRunWouldCopy.
//
// A dry run whose preview disagrees with the real run is worse than no dry run
// at all, and the search runs in a different place from the size and age
// filters — on the server, before the listing is triaged — so the preview has
// to be shown to go through the same narrowing.
func TestTheDryRunPreviewsWhatASearchedRunWouldCopy(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	h.src.stuffWithFlags(t, "INBOX", []imap.Flag{imap.FlagSeen}, testMessage("read", "read@example.test"))
	h.src.stuff(t, "INBOX", testMessage("unread", "unread@example.test"))

	dry := h.run(t, withSourceSearch(t, "UNSEEN"), func(o *syncer.Options) { o.DryRun = true })
	if fr := folderReport(t, dry, "INBOX"); fr.Copied != 1 || fr.Filtered != 1 {
		t.Errorf("the dry run previewed %d copied and %d filtered, want 1 and 1", fr.Copied, fr.Filtered)
	}
	if got := subjectsIn(t, h.dst, "INBOX"); len(got) != 0 {
		t.Fatalf("the dry run copied something: %v", got)
	}

	real := h.run(t, withSourceSearch(t, "UNSEEN"))
	if fr := folderReport(t, real, "INBOX"); fr.Copied != 1 || fr.Filtered != 1 {
		t.Errorf("the real run copied %d and filtered %d, want 1 and 1", fr.Copied, fr.Filtered)
	}
}
