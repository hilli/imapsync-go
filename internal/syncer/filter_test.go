package syncer_test

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/selection"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// stuffAt puts a message in a mailbox with a chosen age.
//
// Both of the message's dates are set from when, because a message whose Date:
// header agrees with its internal date is the ordinary case and keeps these
// tests independent of which one the filter reads. The tests that are about
// that choice use stuffDated and set the two apart.
//
// Dates are given relative to time.Now rather than as absolutes, because the
// run measures age against its own start and a fixed calendar date in a test
// would drift into meaning something different every year.
func stuffAt(t *testing.T, a account, mailbox string, when time.Time, body []byte) {
	t.Helper()

	stuffDated(t, a, mailbox, when, when, body)
}

// stuffDated puts a message in a mailbox whose two dates may disagree, which is
// the only way to see which of them the age bounds are measuring.
func stuffDated(t *testing.T, a account, mailbox string, internal, sent time.Time, body []byte) {
	t.Helper()

	if _, err := a.user.Append(mailbox, bytes.NewReader(withSentDate(body, sent)), &imap.AppendOptions{Time: internal}); err != nil {
		t.Fatalf("stuffing %q: %v", mailbox, err)
	}
}

// sentDateLine matches the fixed Date: header testMessage writes.
var sentDateLine = regexp.MustCompile(`(?m)^Date: .*\r\n`)

// withSentDate rewrites a message's Date: header, or removes it when sent is
// the zero time, so a test can build a message that has none.
func withSentDate(body []byte, sent time.Time) []byte {
	if sent.IsZero() {
		return sentDateLine.ReplaceAllLiteral(body, nil)
	}
	return sentDateLine.ReplaceAllLiteral(body, []byte("Date: "+sent.Format(time.RFC1123Z)+"\r\n"))
}

// paddedMessage is a message of roughly the requested size.
//
// Roughly is enough. Where the boundary falls exactly is settled by the
// selection package's own tests, against imapsync's source; what these tests
// need is a message unambiguously on one side of it.
func paddedMessage(subject, messageID string, size int) []byte {
	body := testMessage(subject, messageID)
	if pad := size - len(body); pad > 0 {
		body = append(body, []byte(strings.Repeat("x", pad))...)
	}
	return body
}

// subjectsIn reports the subjects a destination mailbox holds.
func subjectsIn(t *testing.T, a account, mailbox string) map[string]bool {
	t.Helper()

	seen := map[string]bool{}
	for _, body := range a.contents(t, mailbox) {
		for _, line := range strings.Split(body, "\r\n") {
			if subject, ok := strings.CutPrefix(line, "Subject: "); ok {
				seen[subject] = true
				break
			}
		}
	}
	return seen
}

func withFilter(f selection.Filter) func(*syncer.Options) {
	return func(o *syncer.Options) { o.Filter = f }
}

func TestAMessageLargerThanTheLimitIsNotCopied(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	h.src.stuff(t, "INBOX", paddedMessage("small", "small@example.test", 0))
	h.src.stuff(t, "INBOX", paddedMessage("huge", "huge@example.test", 8000))

	rep := h.run(t, withFilter(selection.Filter{MaxSize: 4000}))

	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 1 {
		t.Errorf("copied %d, want 1", fr.Copied)
	}
	if fr.Filtered != 1 {
		t.Errorf("reported %d filtered, want 1", fr.Filtered)
	}
	if fr.Failed != 0 {
		t.Errorf("reported %d failed; a message left out on purpose is not a failure", fr.Failed)
	}

	got := subjectsIn(t, h.dst, "INBOX")
	if !got["small"] {
		t.Error("the small message was not copied")
	}
	if got["huge"] {
		t.Error("the oversized message was copied anyway")
	}
}

func TestAMessageSmallerThanTheLimitIsNotCopied(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	h.src.stuff(t, "INBOX", paddedMessage("small", "small@example.test", 0))
	h.src.stuff(t, "INBOX", paddedMessage("big", "big@example.test", 8000))

	rep := h.run(t, withFilter(selection.Filter{MinSize: 4000}))

	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 1 || fr.Filtered != 1 {
		t.Errorf("copied %d and filtered %d, want 1 and 1", fr.Copied, fr.Filtered)
	}
	got := subjectsIn(t, h.dst, "INBOX")
	if got["small"] || !got["big"] {
		t.Errorf("--min-size kept the wrong message: %v", got)
	}
}

func TestAMessageOlderThanMaxAgeIsNotCopied(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	now := time.Now()
	stuffAt(t, h.src, "INBOX", now.Add(-2*selection.Day), testMessage("recent", "recent@example.test"))
	stuffAt(t, h.src, "INBOX", now.Add(-200*selection.Day), testMessage("ancient", "ancient@example.test"))

	rep := h.run(t, withFilter(selection.Filter{MaxAge: 30 * selection.Day}))

	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 1 || fr.Filtered != 1 {
		t.Errorf("copied %d and filtered %d, want 1 and 1", fr.Copied, fr.Filtered)
	}
	got := subjectsIn(t, h.dst, "INBOX")
	if !got["recent"] || got["ancient"] {
		t.Errorf("--max-age kept the wrong message: %v", got)
	}
}

func TestAMessageNewerThanMinAgeIsNotCopied(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	now := time.Now()
	stuffAt(t, h.src, "INBOX", now.Add(-2*selection.Day), testMessage("recent", "recent@example.test"))
	stuffAt(t, h.src, "INBOX", now.Add(-200*selection.Day), testMessage("ancient", "ancient@example.test"))

	rep := h.run(t, withFilter(selection.Filter{MinAge: 30 * selection.Day}))

	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 1 || fr.Filtered != 1 {
		t.Errorf("copied %d and filtered %d, want 1 and 1", fr.Copied, fr.Filtered)
	}
	got := subjectsIn(t, h.dst, "INBOX")
	if got["recent"] || !got["ancient"] {
		t.Errorf("--min-age kept the wrong message: %v", got)
	}
}

// TestAMessageLeftOutTodayIsCopiedWhenItQualifies is the hazard the state
// database creates and imapsync does not have.
//
// Eligibility under --min-age changes with the calendar while nothing about the
// message changes at all. So the source's modification sequence does not move,
// and if the folder recorded a completion watermark the fast path would skip it
// on every future run — and the message would never be copied, on a tool whose
// entire promise is that it does not lose mail.
//
// The modseq decorator holds the sequence still for exactly that reason: it
// reproduces the case a real server makes hard to arrange, where nothing has
// happened in the mailbox between the two runs.
func TestAMessageLeftOutTodayIsCopiedWhenItQualifies(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	now := time.Now()
	stuffAt(t, h.src, "INBOX", now.Add(-2*selection.Day), testMessage("recent", "recent@example.test"))
	stuffAt(t, h.src, "INBOX", now.Add(-200*selection.Day), testMessage("ancient", "ancient@example.test"))

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	// Today: the recent message is too new to qualify.
	first, err := syncFlaky(t, h, 1, 1,
		syncer.Options{Filter: selection.Filter{MinAge: 30 * selection.Day}}, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if fr := folderReport(t, first, "INBOX"); fr.Filtered != 1 || fr.Copied != 1 {
		t.Fatalf("first run copied %d and filtered %d, want 1 and 1", fr.Copied, fr.Filtered)
	}
	before := uids.Load()

	// Four days later, in effect. Nothing in the mailbox has changed and the
	// modification sequence has not moved; only the message's age has.
	second, err := syncFlaky(t, h, 1, 1,
		syncer.Options{Filter: selection.Filter{MinAge: selection.Day}}, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if uids.Load() == before {
		t.Error("the second run never re-listed the folder: a filtered run recorded a watermark, " +
			"and the fast path will skip this folder for as long as the mailbox stays quiet")
	}
	if fr := folderReport(t, second, "INBOX"); fr.Copied != 1 {
		t.Errorf("second run copied %d, want 1: the message that has now aged into range", fr.Copied)
	}
	if got := subjectsIn(t, h.dst, "INBOX"); !got["recent"] || !got["ancient"] {
		t.Errorf("the destination is missing mail after both runs: %v", got)
	}
}

// TestAFilterThatLeftNothingOutKeepsTheFastPath.
//
// The watermark is suppressed because a filtered folder is not fully mirrored.
// A folder where the filter excluded nothing *is* fully mirrored, so suppressing
// it there would cost a full re-diff of every folder on every run for no reason
// — on an account of 144 mailboxes and 776k messages, most of the run.
func TestAFilterThatLeftNothingOutKeepsTheFastPath(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	now := time.Now()
	for i := range 5 {
		stuffAt(t, h.src, "INBOX", now.Add(-200*selection.Day),
			testMessage(fmt.Sprintf("old-%d", i), fmt.Sprintf("old%d@example.test", i)))
	}

	seq := &atomic.Uint64{}
	seq.Store(100)
	uids := &atomic.Int32{}

	// Everything is old enough, so nothing is filtered out.
	opts := syncer.Options{Filter: selection.Filter{MinAge: 30 * selection.Day}}
	first, err := syncFlaky(t, h, 1, 1, opts, modseq(seq, uids), nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if fr := folderReport(t, first, "INBOX"); fr.Filtered != 0 || fr.Copied != 5 {
		t.Fatalf("first run copied %d and filtered %d, want 5 and 0", fr.Copied, fr.Filtered)
	}
	before := uids.Load()

	if _, err := syncFlaky(t, h, 1, 1, opts, modseq(seq, uids), nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := uids.Load(); got != before {
		t.Errorf("the second run re-listed the folder %d times; a filter that excluded nothing "+
			"should still let the folder be recorded as mirrored", got-before)
	}
}

// TestDeletingDoesNotReachAMessageTheFilterLeftOut is the deletion hazard.
//
// A filter narrows what is copied. If it also narrowed the source listing that
// deletion is decided against, every message the filter now excludes would look
// to --delete2 like a message the source no longer has — and would be deleted
// from the destination. A filter is a request to copy less, and turning it into
// an instruction to destroy mail is the worst thing this tool could do.
//
// Force is on so that the delete ceiling cannot be what saves the message: the
// question is whether deletion considers it at all.
func TestDeletingDoesNotReachAMessageTheFilterLeftOut(t *testing.T) {
	t.Parallel()

	// UIDPLUS, so that deletion genuinely works here. On a server that cannot
	// expunge by UID this test would pass without testing anything.
	h := newHarness(t, rev2Caps())
	now := time.Now()
	stuffAt(t, h.src, "INBOX", now.Add(-2*selection.Day), testMessage("recent", "recent@example.test"))
	stuffAt(t, h.src, "INBOX", now.Add(-200*selection.Day), testMessage("ancient", "ancient@example.test"))

	// A first, unfiltered run puts both on the destination.
	if fr := folderReport(t, h.run(t), "INBOX"); fr.Copied != 2 {
		t.Fatalf("setup copied %d, want 2", fr.Copied)
	}

	// Now a run that excludes the recent one and is allowed to delete.
	rep := h.run(t, withFilter(selection.Filter{MinAge: 30 * selection.Day}), func(o *syncer.Options) {
		o.Delete2 = true
		o.Force = true
	})

	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 0 {
		t.Errorf("deleted %d messages, want 0: the source still holds both", fr.Deleted)
	}
	got := subjectsIn(t, h.dst, "INBOX")
	if !got["recent"] {
		t.Error("--delete2 destroyed a message that the filter merely declined to copy again")
	}
	if !got["ancient"] {
		t.Error("--delete2 destroyed a message the filter selected")
	}
}

// TestDeletingStillWorksUnderAFilter is the other half.
//
// A guard that saved every message would pass the test above and be useless.
// A message genuinely gone from the source must still be deleted, filter or no
// filter, because the filter says nothing about it.
func TestDeletingStillWorksUnderAFilter(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps())
	now := time.Now()
	for i := range 4 {
		stuffAt(t, h.src, "INBOX", now.Add(-200*selection.Day),
			testMessage(fmt.Sprintf("old-%d", i), fmt.Sprintf("old%d@example.test", i)))
	}
	if fr := folderReport(t, h.run(t), "INBOX"); fr.Copied != 4 {
		t.Fatalf("setup copied %d, want 4", fr.Copied)
	}

	// The first message appended is source UID 1.
	removeFrom(t, h.src, "INBOX", 1)

	rep := h.run(t, withFilter(selection.Filter{MinAge: 30 * selection.Day}), func(o *syncer.Options) {
		o.Delete2 = true
		o.Force = true
	})
	if fr := folderReport(t, rep, "INBOX"); fr.Deleted != 1 {
		t.Errorf("deleted %d, want 1: a message gone from the source is gone whatever the filter says", fr.Deleted)
	}
	if subjectsIn(t, h.dst, "INBOX")["old-0"] {
		t.Error("the deleted message is still on the destination")
	}
}

// limitedDest reports an APPENDLIMIT the in-process server does not have.
type limitedDest struct {
	imapx.Conn
	limit uint32
}

func (l limitedDest) Caps() imapx.Caps {
	caps := l.Conn.Caps()
	caps.AppendLimit = &l.limit
	return caps
}

func appendLimited(limit uint32) func(imapx.Conn) imapx.Conn {
	return func(c imapx.Conn) imapx.Conn { return limitedDest{Conn: c, limit: limit} }
}

// TestTheAppendLimitIsObeyedAsReported.
//
// The compatibility layer accepts imapsync's --appendlimit and tells the user
// "the server's APPENDLIMIT is obeyed as reported". Until this, that statement
// was false: the capability was parsed and then nothing read it, so an
// oversized message was fetched in full, appended, refused, retried and finally
// recorded as a failure — which reads like a fault rather than like the rule it
// is.
func TestTheAppendLimitIsObeyedAsReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	h.src.stuff(t, "INBOX", paddedMessage("small", "small@example.test", 0))
	h.src.stuff(t, "INBOX", paddedMessage("huge", "huge@example.test", 8000))

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, appendLimited(4000))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	fr := folderReport(t, rep, "INBOX")
	if fr.Filtered != 1 {
		t.Errorf("reported %d filtered, want 1: the oversized message is above APPENDLIMIT", fr.Filtered)
	}
	if fr.Failed != 0 {
		t.Errorf("reported %d failed; a message the server said it would not take is not a fault", fr.Failed)
	}
	if got := subjectsIn(t, h.dst, "INBOX"); !got["small"] || got["huge"] {
		t.Errorf("APPENDLIMIT selected the wrong messages: %v", got)
	}
}

// TestAnUnreportedAppendLimitFiltersNothing keeps the previous test honest: a
// server that says nothing about APPENDLIMIT must be treated as having no limit
// rather than a limit of zero.
func TestAnUnreportedAppendLimitFiltersNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	h.src.stuff(t, "INBOX", paddedMessage("huge", "huge@example.test", 8000))

	rep := h.run(t)
	if fr := folderReport(t, rep, "INBOX"); fr.Copied != 1 || fr.Filtered != 0 {
		t.Errorf("copied %d and filtered %d, want 1 and 0", fr.Copied, fr.Filtered)
	}
}

// TestASizeWindowWithNoRoomInItStopsTheRun.
//
// The alternative is a run that copies nothing and reports success, leaving the
// reader to work out that their two flags cancel.
func TestASizeWindowWithNoRoomInItStopsTheRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 3)

	_, err := syncFlaky(t, h, 1, 1,
		syncer.Options{Filter: selection.Filter{MinSize: 5000, MaxSize: 1000}}, nil, nil)
	if err == nil {
		t.Fatal("a filter that can select nothing was accepted")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error does not say what is wrong: %v", err)
	}
	if n := len(h.dst.contents(t, "INBOX")); n != 0 {
		t.Errorf("the run wrote %d messages before refusing", n)
	}
}

// TestAFilteredMessageIsNotWrittenOffAsVanished guards the order of two steps
// that look independent and are not.
//
// A UID the server lists and then has no message for is recorded as gone, for
// good, so that a folder does not ask about it on every run for ever. That
// conclusion is drawn from the message being absent from the FETCH response —
// so if the filter ran first, every message it excluded would be missing from
// that response too, and would be written off permanently. Raising --max-age
// later would then never bring them back.
func TestAFilteredMessageIsNotWrittenOffAsVanished(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	now := time.Now()
	stuffAt(t, h.src, "INBOX", now.Add(-2*selection.Day), testMessage("recent", "recent@example.test"))
	stuffAt(t, h.src, "INBOX", now.Add(-200*selection.Day), testMessage("ancient", "ancient@example.test"))

	first := h.run(t, withFilter(selection.Filter{MinAge: 30 * selection.Day}))
	if fr := folderReport(t, first, "INBOX"); fr.Vanished != 0 {
		t.Errorf("reported %d vanished, want 0: a filtered message is still there", fr.Vanished)
	}

	// The filter is dropped. If the first run had recorded the message as gone
	// the state database would refuse to look at it again.
	second := h.run(t)
	if fr := folderReport(t, second, "INBOX"); fr.Copied != 1 {
		t.Errorf("second run copied %d, want 1; vanished %d", fr.Copied, fr.Vanished)
	}
	if !subjectsIn(t, h.dst, "INBOX")["recent"] {
		t.Error("the filtered message was permanently written off as vanished")
	}
}

// TestADryRunSaysHowMuchTheFilterLeavesOut.
//
// The question somebody runs --dry-run --max-age to have answered is exactly
// how much the bound excludes. A preview that counted every message as "to
// copy" regardless would answer a different question while looking like an
// answer to theirs.
func TestADryRunSaysHowMuchTheFilterLeavesOut(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	now := time.Now()
	stuffAt(t, h.src, "INBOX", now.Add(-2*selection.Day), testMessage("recent", "recent@example.test"))
	stuffAt(t, h.src, "INBOX", now.Add(-200*selection.Day), testMessage("ancient", "ancient@example.test"))

	rep := h.run(t, withFilter(selection.Filter{MinAge: 30 * selection.Day}), func(o *syncer.Options) {
		o.DryRun = true
	})

	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 1 {
		t.Errorf("previewed %d to copy, want 1", fr.Copied)
	}
	if fr.Filtered != 1 {
		t.Errorf("previewed %d filtered, want 1", fr.Filtered)
	}
	if n := len(h.dst.contents(t, "INBOX")); n != 0 {
		t.Errorf("the dry run wrote %d messages", n)
	}
}

// TestADryRunWithNoFilterCountsEverything keeps the previous test from being
// satisfied by a preview that simply under-counts.
func TestADryRunWithNoFilterCountsEverything(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 7)

	rep := h.run(t, func(o *syncer.Options) { o.DryRun = true })
	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 7 || fr.Filtered != 0 {
		t.Errorf("previewed %d to copy and %d filtered, want 7 and 0", fr.Copied, fr.Filtered)
	}
}

// meteredSource records how many messages a run asked metadata for.
type meteredSource struct {
	imapx.Conn
	n *atomic.Int32
}

func (m meteredSource) FetchMeta(ctx context.Context, uids []uint32, fields []string) ([]imapx.MessageMeta, error) {
	m.n.Add(int32(len(uids)))
	return m.Conn.FetchMeta(ctx, uids, fields)
}

// TestAnUnfilteredDryRunFetchesNoMetadata.
//
// A dry run answers its question from the UID listing alone. The metadata pass
// a filter needs is a per-message round trip against the source, so running it
// when no filter is set would turn a cheap preview of a 400,000-message folder
// into an expensive one that returns the same answer.
func TestAnUnfilteredDryRunFetchesNoMetadata(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 5)

	var fetched atomic.Int32
	count := func(c imapx.Conn) imapx.Conn { return meteredSource{Conn: c, n: &fetched} }

	if _, err := syncFlaky(t, h, 1, 1, syncer.Options{DryRun: true}, count, nil); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := fetched.Load(); got != 0 {
		t.Errorf("an unfiltered dry run fetched metadata for %d messages, want none", got)
	}

	fetched.Store(0)
	opts := syncer.Options{DryRun: true, Filter: selection.Filter{MaxSize: 1 << 20}}
	if _, err := syncFlaky(t, h, 1, 1, opts, count, nil); err != nil {
		t.Fatalf("filtered dry run: %v", err)
	}
	if got := fetched.Load(); got != 5 {
		t.Errorf("a filtered dry run fetched metadata for %d messages, want 5", got)
	}
}

// The age bounds read the Date: header, not the internal date.
//
// This is imapsync's default, and it is the reverse of the obvious guess. The
// difference shows up in exactly the situation this tool is for: a migration
// that did not preserve internal dates leaves every message looking as though
// it arrived on the day it was copied, and --max-age measured that way would
// select the whole mailbox.
func TestAgeIsMeasuredFromTheSentDateByDefault(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	now := time.Now()
	// Both arrived yesterday; they were written months apart.
	stuffDated(t, h.src, "INBOX", now.Add(-1*selection.Day), now.Add(-2*selection.Day),
		testMessage("written-recently", "recent@example.test"))
	stuffDated(t, h.src, "INBOX", now.Add(-1*selection.Day), now.Add(-200*selection.Day),
		testMessage("written-long-ago", "ancient@example.test"))

	rep := h.run(t, withFilter(selection.Filter{MaxAge: 30 * selection.Day}))

	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 1 || fr.Filtered != 1 {
		t.Errorf("copied %d and filtered %d, want 1 and 1", fr.Copied, fr.Filtered)
	}
	got := subjectsIn(t, h.dst, "INBOX")
	if !got["written-recently"] {
		t.Error("the recently written message was not copied")
	}
	if got["written-long-ago"] {
		t.Error("a message written 200 days ago was copied; --max-age reads the Date: header")
	}
}

// The same two messages, with --age-basis internal, both qualify — which is
// what keeps the test above from passing for the wrong reason.
func TestTheInternalBasisMeasuresArrivalInstead(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	now := time.Now()
	stuffDated(t, h.src, "INBOX", now.Add(-1*selection.Day), now.Add(-2*selection.Day),
		testMessage("written-recently", "recent@example.test"))
	stuffDated(t, h.src, "INBOX", now.Add(-1*selection.Day), now.Add(-200*selection.Day),
		testMessage("written-long-ago", "ancient@example.test"))

	rep := h.run(t, withFilter(selection.Filter{
		MaxAge: 30 * selection.Day,
		Basis:  selection.BasisInternal,
	}))

	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 2 || fr.Filtered != 0 {
		t.Errorf("copied %d and filtered %d, want 2 and 0: both arrived yesterday", fr.Copied, fr.Filtered)
	}
}

// A message with no Date: header is judged by when it arrived, not excluded.
func TestAMessageWithNoSentDateIsJudgedByItsArrival(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	now := time.Now()
	// No Date: header at all, and an arrival well inside the bound.
	stuffDated(t, h.src, "INBOX", now.Add(-1*selection.Day), time.Time{},
		testMessage("undated-recent", "undated-recent@example.test"))
	stuffDated(t, h.src, "INBOX", now.Add(-200*selection.Day), time.Time{},
		testMessage("undated-old", "undated-old@example.test"))

	rep := h.run(t, withFilter(selection.Filter{MaxAge: 30 * selection.Day}))

	fr := folderReport(t, rep, "INBOX")
	if fr.Copied != 1 || fr.Filtered != 1 {
		t.Errorf("copied %d and filtered %d, want 1 and 1", fr.Copied, fr.Filtered)
	}
	if !subjectsIn(t, h.dst, "INBOX")["undated-recent"] {
		t.Error("an undated message that arrived yesterday was not copied")
	}
}

// A dry run on the default basis must fetch the Date: header it measures with.
//
// The copy path gets that header free, because it digests it. The dry run
// digests nothing and so asks for no headers at all — which would leave it
// measuring every message against a zero Date: and falling back to arrival,
// quietly previewing a different run from the one that would happen.
func TestADryRunPreviewsTheSameMessagesTheRealRunWouldCopy(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	now := time.Now()
	stuffDated(t, h.src, "INBOX", now.Add(-1*selection.Day), now.Add(-2*selection.Day),
		testMessage("written-recently", "recent@example.test"))
	stuffDated(t, h.src, "INBOX", now.Add(-1*selection.Day), now.Add(-200*selection.Day),
		testMessage("written-long-ago", "ancient@example.test"))

	filter := withFilter(selection.Filter{MaxAge: 30 * selection.Day})

	preview := folderReport(t, h.run(t, filter, func(o *syncer.Options) { o.DryRun = true }), "INBOX")
	if preview.Copied != 1 || preview.Filtered != 1 {
		t.Fatalf("previewed %d to copy and %d filtered, want 1 and 1", preview.Copied, preview.Filtered)
	}

	real := folderReport(t, h.run(t, filter), "INBOX")
	if real.Copied != preview.Copied || real.Filtered != preview.Filtered {
		t.Errorf("previewed %d/%d but copied %d/%d", preview.Copied, preview.Filtered, real.Copied, real.Filtered)
	}
}
