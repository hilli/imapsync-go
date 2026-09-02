package syncer_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/syncer"

	_ "modernc.org/sqlite" // the state database is read directly below
)

// twin is a message deliberately identical to another, byte for byte.
//
// Written as a plain copy rather than a second testMessage call so that the
// tests below cannot be quietly weakened by a change to testMessage that makes
// its output depend on a counter or a clock.
func twin(subject, messageID string) []byte {
	return slices.Clone(testMessage(subject, messageID))
}

// nearMiss is the message this feature must not touch: everything testMessage's
// identity is built from agrees with its twin, and the body does not.
//
// This is not a hypothetical. Adoption matches on six headers because that is
// all a destination will cheaply give up, and a message with no Message-ID is
// matched on five — which two notifications sent in the same second by the same
// robot will share while saying different things. Skipping one of those would
// lose mail.
func nearMiss(subject, messageID, body string) []byte {
	return []byte(fmt.Sprintf(
		"From: sender@example.test\r\n"+
			"To: %s\r\n"+
			"Subject: %s\r\n"+
			"Message-ID: <%s>\r\n"+
			"Date: Mon, 27 Aug 2026 12:00:00 +0000\r\n"+
			"\r\n"+
			"%s\r\n", testUser, subject, messageID, body))
}

// bodiesOf lists a mailbox's messages with their leading headers stripped, so
// that a stamped copy compares equal to the source it was made from.
func bodiesOf(t *testing.T, a account, mailbox string) []string {
	t.Helper()

	var out []string
	for _, msg := range a.contents(t, mailbox) {
		_, body, _ := strings.Cut(msg, "\r\n\r\n")
		out = append(out, strings.TrimSpace(body))
	}
	slices.Sort(out)
	return out
}

// doneRowsNamingNothing counts rows the state database calls copied that name
// no destination message.
//
// Raw SQL because every accessor filters these out by design, so the only way
// to ask whether one was written is to go under them.
func doneRowsNamingNothing(t *testing.T, h *harness) int {
	t.Helper()

	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatalf("opening the state database: %v", err)
	}
	defer func() { _ = db.Close() }()

	var n int
	// The state constant is spelled out rather than imported so that renumbering
	// StateDone cannot silently turn this into a count of nothing.
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE state = 'done' AND dst_uid = 0`).Scan(&n); err != nil {
		t.Fatalf("counting settled rows: %v", err)
	}
	return n
}

// TestAMessageRepeatedInTheSourceIsCopiedOnce is the feature.
//
// imapsync skips duplicates by default and this is a drop-in, so a folder
// holding the same message twice must leave one copy on the destination.
func TestAMessageRepeatedInTheSourceIsCopiedOnce(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))
	h.src.stuff(t, "INBOX", testMessage("alone", "alone@example.test"))

	rep := h.run(t)
	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 2 {
		t.Errorf("copied %d messages, want 2: the repeat should not have been copied again", fr.Copied)
	}
	if fr.Duplicates != 1 {
		t.Errorf("reported %d duplicates, want 1", fr.Duplicates)
	}
	if rep.Duplicates() != 1 {
		t.Errorf("run total reported %d duplicates, want 1", rep.Duplicates())
	}
	// Subjects rather than a count, because a count of two is also what two
	// copies of "repeated" with "alone" dropped would give.
	if got := sortedSubjects(t, h.dst); !slices.Equal(got, []string{"alone", "repeated"}) {
		t.Fatalf("destination holds %v, want one copy of each distinct message", got)
	}
}

// TestASkippedRepeatIsNotFetchedAgainOnTheNextRun.
//
// The skip is only half the feature. If the repeat's row were left in flight,
// every later run would fetch its body again to reach the same conclusion, and
// the folder's watermark would never advance past it — so a settled folder
// would keep paying for a duplicate it settled months ago.
func TestASkippedRepeatIsNotFetchedAgainOnTheNextRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))

	if fr := folderReport(t, h.run(t), "INBOX"); fr.Duplicates != 1 {
		t.Fatalf("first run reported %d duplicates, want 1", fr.Duplicates)
	}

	// --full, because the cheap path would skip the folder on the message
	// counts alone and prove nothing about what the diff thinks.
	rep := h.run(t, func(o *syncer.Options) { o.Full = true })
	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 0 {
		t.Errorf("second run copied %d messages, want 0", fr.Copied)
	}
	if fr.Duplicates != 0 {
		t.Errorf("second run reported %d duplicates, want 0: the repeat was settled by the first run", fr.Duplicates)
	}
	if fr.Missing != 0 {
		t.Errorf("second run called %d copies missing; a repeat's row points at a copy that is still there", fr.Missing)
	}
	if got := sortedSubjects(t, h.dst); !slices.Equal(got, []string{"repeated"}) {
		t.Fatalf("destination holds %v after a second run, want one copy", got)
	}
}

// TestTwoMessagesThatMerelyLookAlikeAreBothCopied is the guard that makes the
// skip safe to have at all.
//
// Not copying a message loses it as surely as deleting one, so the evidence
// adoption works from is not enough here.
func TestTwoMessagesThatMerelyLookAlikeAreBothCopied(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", nearMiss("alert", "alert@example.test", "The disk is full."))
	h.src.stuff(t, "INBOX", nearMiss("alert", "alert@example.test", "The disk is fine."))

	rep := h.run(t)
	fr := folderReport(t, rep, "INBOX")
	if fr.Duplicates != 0 {
		t.Errorf("reported %d duplicates for two messages that differ", fr.Duplicates)
	}
	if fr.Copied != 2 {
		t.Errorf("copied %d messages, want both", fr.Copied)
	}
	want := []string{"The disk is fine.", "The disk is full."}
	if got := bodiesOf(t, h.dst, "INBOX"); !slices.Equal(got, want) {
		t.Fatalf("destination holds %v, want %v", got, want)
	}
}

// TestSyncDuplicatesCopiesEveryRepeat.
//
// The escape hatch, and the reason the skip can be a default: a user who wants
// the folder mirrored exactly can say so.
func TestSyncDuplicatesCopiesEveryRepeat(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))

	rep := h.run(t, func(o *syncer.Options) { o.SyncDuplicates = true })
	fr := folderReport(t, rep, "INBOX")
	if fr.Duplicates != 0 {
		t.Errorf("reported %d duplicates with --sync-duplicates given", fr.Duplicates)
	}
	if fr.Copied != 2 {
		t.Errorf("copied %d messages, want both", fr.Copied)
	}
	if got := sortedSubjects(t, h.dst); !slices.Equal(got, []string{"repeated", "repeated"}) {
		t.Fatalf("destination holds %v, want both copies", got)
	}
}

// refusingAppend fails the first n appends of a run.
type refusingAppend struct {
	imapx.Conn
	left *int
}

var errRefused = errors.New("destination refused the message")

func (r refusingAppend) Append(ctx context.Context, mailbox string, msg imapx.AppendMessage) (imapx.AppendResult, error) {
	if *r.left > 0 {
		*r.left--
		return imapx.AppendResult{}, errRefused
	}
	return r.Conn.Append(ctx, mailbox, msg)
}

// TestARepeatWhoseCopyDidNotLandIsLeftForTheNextRun.
//
// A repeat is recorded against the copy that stands for it, so if that copy
// never landed there is nothing to record it against. Writing the row anyway
// would say a message is on the destination that is not, and no later run would
// correct it — the row would read as done for ever.
//
// Leaving it in flight is the answer: the next run fetches both again and one
// of them lands.
func TestARepeatWhoseCopyDidNotLandIsLeftForTheNextRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))

	// One connection each so the two messages are fetched in order and the
	// refusal lands on the survivor rather than on nothing.
	left := 1
	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{Retry: brisk()}, nil,
		func(c imapx.Conn) imapx.Conn { return refusingAppend{Conn: c, left: &left} })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Failed == 0 {
		t.Fatalf("the refused append was not reported as a failure: %+v", fr)
	}
	if got := len(h.dst.contents(t, "INBOX")); got != 0 {
		t.Fatalf("destination holds %d messages, want none: the only copy was refused", got)
	}
	// The invariant the skip must not break. A settled row names a destination
	// message, and the survivor has none — so the repeat must be left in flight
	// rather than settled against nothing.
	//
	// Asked of the database rather than of the run, because both Mirrored and
	// ClaimedCount ignore a zero destination UID and would hide the false row
	// they were written to protect against. That the harm is absorbed today is
	// not a reason to write the row: the absorption lives in two queries a
	// third could be added beside.
	if n := doneRowsNamingNothing(t, h); n != 0 {
		t.Errorf("%d rows are recorded as copied while naming no destination message", n)
	}

	// The run that matters. Nothing was recorded, so both messages come back.
	rep = h.run(t)
	fr := folderReport(t, rep, "INBOX")
	if fr.Failed != 0 {
		t.Fatalf("second run failed %d messages: %+v", fr.Failed, fr)
	}
	if got := sortedSubjects(t, h.dst); !slices.Equal(got, []string{"repeated"}) {
		t.Fatalf("destination holds %v after the retry, want the message back exactly once", got)
	}

	// A third run, because the harm of recording the repeat against a copy that
	// never landed does not show up in the run that does it. The row would name
	// destination UID 0, the destination holds one message against two claims,
	// and verification would call the missing one missing and copy it — leaving
	// two copies of a message the source holds twice and the destination should
	// hold once.
	rep = h.run(t, func(o *syncer.Options) { o.Full = true })
	fr = folderReport(t, rep, "INBOX")
	if fr.Missing != 0 {
		t.Errorf("a later run called %d copies missing; every recorded row should name a copy that is there", fr.Missing)
	}
	if fr.Copied != 0 {
		t.Errorf("a later run copied %d messages, want 0", fr.Copied)
	}
	if got := sortedSubjects(t, h.dst); !slices.Equal(got, []string{"repeated"}) {
		t.Fatalf("destination holds %v, want one copy still", got)
	}
}

// TestDeletingWaitsUntilEveryRepeatIsGone is the bug this feature introduced
// into deletion, and the reason stillClaimed exists.
//
// One destination copy now stands for two source messages. Deletion nominates
// per recorded row, so the copy is condemned as soon as either source message
// goes — while the other is still sitting in the source folder. That deletes
// mail the source still holds, which is the one thing deletion must never do.
func TestDeletingWaitsUntilEveryRepeatIsGone(t *testing.T) {
	t.Parallel()

	h := newDeleteHarness(t)
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))
	h.src.stuff(t, "INBOX", twin("repeated", "repeated@example.test"))
	// Enough other mail that removing one message stays under the 10% ceiling,
	// so a deletion that happens is the feature and not the guard.
	fill(t, h.src, "INBOX", 30)

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := len(uidsIn(t, h.dst, "INBOX"))

	// One of the two identical messages goes. The other stays.
	removeFrom(t, h.src, "INBOX", uidsIn(t, h.src, "INBOX")[0])

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{Full: true, Delete2: true}, nil, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 0 {
		t.Errorf("deleted %d messages while the source still held the message", fr.Deleted)
	}
	if got := len(uidsIn(t, h.dst, "INBOX")); got != before {
		t.Errorf("destination holds %d messages, want %d: nothing should have gone", got, before)
	}
	if got := slices.Contains(sortedSubjects(t, h.dst), "repeated"); !got {
		t.Fatal("the copy was deleted while a source message still named it")
	}

	// And when the last one goes, so does the copy. Without this half the test
	// passes on a syncer that never deletes anything.
	removeFrom(t, h.src, "INBOX", uidsIn(t, h.src, "INBOX")[0])

	rep, err = syncFlaky(t, h, 1, 1, syncer.Options{Full: true, Delete2: true}, nil, nil)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 1 {
		t.Errorf("deleted %d messages once no source message named the copy, want 1", fr.Deleted)
	}
	if got := slices.Contains(sortedSubjects(t, h.dst), "repeated"); got {
		t.Error("the copy survived after both source messages had gone")
	}
}
